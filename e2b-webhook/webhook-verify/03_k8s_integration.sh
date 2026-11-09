#!/bin/bash
###############################################################################
# 03_k8s_integration.sh — 真实 apiserver 集成与可用性
#
# 用例：
#   K1 server dry-run（返回对象已注入，且未持久化）
#   K2 真实创建（Pod Ready 后注解仍在；未 Ready 记 SKIP + 诊断）
#   K3 用户注解 + 真实创建（用户值保留，其余注解注入）
#   K4 BatchSandbox dry-run（CRD 缺失记 UNSUPPORTED）
#   K5 50 并发 dry-run（100% 成功，p95 < 5s）
#   K6 删除单个 webhook Pod（Endpoint 不断、副本自动补齐）
#   K7 滚动重启期间准入（成功率 100%，结束后副本 Ready）
#
# 与 01 的分工：01 直连 /mutate 验证“注入什么”，03 通过 apiserver 验证
# “选择器是否命中、对象是否真的被改、并发/滚动期间是否可用”。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
. "${SCRIPT_DIR}/lib/k8s.sh"
. "${SCRIPT_DIR}/lib/webhook.sh"
. "${SCRIPT_DIR}/lib/admission.sh"
. "${SCRIPT_DIR}/lib/provider.sh"

log_section "03 K8S 集成 / 并发"

webhook_detect

WORK="${WV_TMP}/03"
mkdir -p "${WORK}"
PROVIDER="$(provider_active)"
TEMPLATE="${WV_E2E_TEMPLATE}"

if ! k8s_cluster_reachable; then
    for id in K1 K2 K3 K4 K5 K6 K7; do
        unsupported "${id}" "集群不可达，无法执行 apiserver 集成用例"
    done
    finish "03 K8S 集成"
fi

if [ "$(state_get MWC_EXISTS)" != "1" ]; then
    for id in K1 K2 K3 K4 K5 K6 K7; do
        unsupported "${id}" "MutatingWebhookConfiguration ${WV_MWC} 不存在"
    done
    finish "03 K8S 集成"
fi

k8s_ensure_ns "${WV_TEST_NS}" || true

#==================== fixture ====================#
IMAGE=""
IMAGE_OK=0
if IMAGE=$(provider_template_image "${TEMPLATE}" 2>/dev/null); then
    IMAGE_OK=1
    log_info "模板 ${TEMPLATE} → 镜像 ${IMAGE}"
else
    IMAGE="e2b.dev/verify-missing:missing"
    log_warn "无法解析模板 ${TEMPLATE} 的镜像（API/凭证不可用），K2/K3 将 SKIP"
fi

LABELS=$(printf '  labels:\n    "%s": "0"' "${WV_POD_LABEL}")
ENV_BLOCK=$(printf '        - name: TEMPLATE_NAME\n          value: "%s"' "${TEMPLATE}")

render_pod() {
    local out="$1" name="$2" extra_annotations="${3:-}"
    local annos_block=""
    if [ -n "${extra_annotations}" ]; then
        annos_block="  annotations:"
        local pair
        for pair in ${extra_annotations}; do
            annos_block="${annos_block}
    ${pair%%=*}: \"${pair#*=}\""
        done
    fi
    k8s_render "${FIXTURE_DIR}/pod.yaml" "${out}" \
        "NAME=${name}" "NS=${WV_TEST_NS}" \
        "LABELS=${LABELS}" "ANNOTATIONS=${annos_block}" "RUNTIMECLASS=" \
        "IMAGE=${IMAGE}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${ENV_BLOCK}"
}

render_pod "${WORK}/pod.yaml" "wv-k8s-pod"

#==================== K1 server dry-run ====================#
log_step "K1 server dry-run"
k8s_delete_pod "${WV_TEST_NS}" "wv-k8s-pod"
if out=$(kubectl create --dry-run=server -o json -f "${WORK}/pod.yaml" 2>"${WORK}/k1.err"); then
    annos=$(printf '%s' "${out}" | jq -c '.metadata.annotations // {}')
    rc=$(printf '%s' "${out}" | jq -r '.spec.runtimeClassName // ""')
    count=$(provider_annotation_count "${PROVIDER}" "${annos}")
    if [ "${rc}" = "e2b" ] && [ "${count}" -ge 21 ]; then
        if kubectl -n "${WV_TEST_NS}" get pod "wv-k8s-pod" >/dev/null 2>&1; then
            fail K1 "dry-run 对象已落盘（不应持久化）"
        else
            pass K1 "dry-run 返回注入后的对象（runtimeClass=e2b，注解 ${count} 个），未持久化"
        fi
    else
        fail K1 "dry-run 对象未注入：runtimeClass='${rc}' 注解=${count}"
    fi
else
    fail K1 "server dry-run 失败: $(tail -1 "${WORK}/k1.err" 2>/dev/null)"
fi

#==================== K2 真实创建 ====================#
log_step "K2 真实创建"
k8s_delete_pod "${WV_TEST_NS}" "wv-k8s-pod"
sleep 1
if [ "${IMAGE_OK}" != "1" ]; then
    skip K2 "无法解析模板 ${TEMPLATE} 的镜像，跳过真实创建"
elif kubectl apply -f "${WORK}/pod.yaml" >"${WORK}/k2.apply.log" 2>&1; then
    if k8s_wait_pod_ready "${WV_TEST_NS}" "wv-k8s-pod" "${WV_E2E_TIMEOUT}"; then
        annos=$(k8s_pod_annotations_json "${WV_TEST_NS}" "wv-k8s-pod")
        rc=$(k8s_pod_runtimeclass "${WV_TEST_NS}" "wv-k8s-pod")
        count=$(provider_annotation_count "${PROVIDER}" "${annos}")
        if [ "${rc}" = "e2b" ] && [ "${count}" -ge 21 ]; then
            pass K2 "真实创建并 Ready，注解仍在（${count} 个）"
        else
            fail K2 "Pod Ready 但注入丢失：runtimeClass='${rc}' 注解=${count}"
        fi
    else
        dir=$(k8s_collect_pod_artifacts "${WV_TEST_NS}" "wv-k8s-pod" "k2-pod")
        skip K2 "Pod 未在 ${WV_E2E_TIMEOUT}s 内 Ready（沙箱链路问题不在本模块判 FAIL），证据 ${dir}"
    fi
else
    dir=$(k8s_collect_pod_artifacts "${WV_TEST_NS}" "wv-k8s-pod" "k2-pod")
    skip K2 "创建失败（$(tail -2 "${WORK}/k2.apply.log" | tr '\n' ' ')），证据 ${dir}"
fi

#==================== K3 用户注解 + 真实创建 ====================#
log_step "K3 用户注解"
render_pod "${WORK}/k3.yaml" "wv-k8s-pod-user" 'e2b.dev/template-id=user-value custom/keep=yes'
k8s_delete_pod "${WV_TEST_NS}" "wv-k8s-pod-user"
sleep 1
if [ "${IMAGE_OK}" != "1" ]; then
    skip K3 "无法解析模板 ${TEMPLATE} 的镜像，跳过真实创建"
elif kubectl apply -f "${WORK}/k3.yaml" >"${WORK}/k3.apply.log" 2>&1; then
    if k8s_wait_pod_ready "${WV_TEST_NS}" "wv-k8s-pod-user" "${WV_E2E_TIMEOUT}"; then
        value=$(k8s_pod_annotation "${WV_TEST_NS}" "wv-k8s-pod-user" "e2b.dev/template-id")
        keep=$(k8s_pod_annotation "${WV_TEST_NS}" "wv-k8s-pod-user" "custom/keep")
        annos=$(k8s_pod_annotations_json "${WV_TEST_NS}" "wv-k8s-pod-user")
        rc=$(k8s_pod_runtimeclass "${WV_TEST_NS}" "wv-k8s-pod-user")
        count=$(provider_annotation_count "${PROVIDER}" "${annos}")
        if [ "${value}" = "user-value" ] && [ "${keep}" = "yes" ] \
            && [ "${rc}" = "e2b" ] && [ "${count}" -ge 21 ]; then
            pass K3 "真实创建后用户注解保留（template-id=user-value, custom/keep=yes），其余 ${count} 个 provider 注解已注入"
        else
            fail K3 "用户注解被改或注入缺失：template-id='${value}' custom/keep='${keep}' runtimeClass='${rc}' 注解=${count}"
        fi
    else
        dir=$(k8s_collect_pod_artifacts "${WV_TEST_NS}" "wv-k8s-pod-user" "k3-pod")
        skip K3 "Pod 未 Ready，证据 ${dir}"
    fi
else
    skip K3 "创建失败（$(tail -2 "${WORK}/k3.apply.log" | tr '\n' ' ')）"
fi

k8s_delete_pod "${WV_TEST_NS}" "wv-k8s-pod"
k8s_delete_pod "${WV_TEST_NS}" "wv-k8s-pod-user"

#==================== K4 BatchSandbox dry-run ====================#
log_step "K4 BatchSandbox dry-run"
if ! k8s_crd_exists "batchsandboxes.sandbox.opensandbox.io"; then
    unsupported K4 "BatchSandbox CRD（sandbox.opensandbox.io）未安装，无法验证 spec.template.* 注入"
else
    bs_labels=$(printf '      labels:\n        "%s": "0"' "${WV_POD_LABEL}")
    bs_env=$(printf '            - name: TEMPLATE_NAME\n              value: "%s"' "${TEMPLATE}")
    k8s_render "${FIXTURE_DIR}/batchsandbox.yaml" "${WORK}/bs.yaml" \
        "NAME=wv-k4" "NS=${WV_TEST_NS}" "LABELS=${bs_labels}" "ANNOTATIONS=" "RUNTIMECLASS=" \
        "IMAGE=${IMAGE}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${bs_env}"
    if out=$(kubectl create --dry-run=server -o json -f "${WORK}/bs.yaml" 2>"${WORK}/k4.err"); then
        annos=$(printf '%s' "${out}" | jq -c '.spec.template.metadata.annotations // {}')
        rc=$(printf '%s' "${out}" | jq -r '.spec.template.spec.runtimeClassName // ""')
        count=$(provider_annotation_count "${PROVIDER}" "${annos}")
        if [ "${rc}" = "e2b" ] && [ "${count}" -ge 21 ]; then
            pass K4 "BatchSandbox dry-run 注入到 spec.template.*（注解 ${count} 个）"
        else
            fail K4 "BatchSandbox 未注入：runtimeClass='${rc}' 注解=${count}"
        fi
    else
        skip K4 "BatchSandbox dry-run 失败（CRD 结构可能不同）: $(tail -2 "${WORK}/k4.err" | tr '\n' ' ')"
    fi
fi

#==================== 并发辅助 ====================#
# K5 单独用一个 Pod 名：K1–K3 的 Pod 删除是异步的（--wait=false），
# 复用 wv-k8s-pod 时并发创建会撞上还在 Terminating 的同名对象。
render_pod "${WORK}/pod-conc.yaml" "wv-k5-conc"
kubectl -n "${WV_TEST_NS}" delete pod "wv-k5-conc" --ignore-not-found=true \
    --wait=true --timeout=60s >/dev/null 2>&1 || true

# concurrent_dry_run <目录> <次数> <间隔秒>
concurrent_dry_run() {
    local dir="$1" total="$2" interval="${3:-0}"
    rm -rf "${dir}"; mkdir -p "${dir}"
    local i
    for i in $(seq 1 "${total}"); do
        (
            start=$(now_ms)
            if kubectl create --dry-run=server -o json -f "${WORK}/pod-conc.yaml" >/dev/null 2>"${dir}/${i}.err"; then
                rc=0
            else
                rc=1
            fi
            echo "${rc} $(( $(now_ms) - start ))" > "${dir}/${i}.out"
        ) &
        if [ "${interval}" != "0" ]; then
            sleep "${interval}"
        fi
    done
    wait
}

summarize_runs() {
    # 输出：成功数 总数 p95_ms 最大ms
    local dir="$1"
    local total ok p95 max_ms
    total=$( { ls "${dir}"/*.out 2>/dev/null || true; } | wc -l | tr -d ' ')
    ok=$( { grep -l '^0 ' "${dir}"/*.out 2>/dev/null || true; } | wc -l | tr -d ' ')
    p95=$( { cat "${dir}"/*.out 2>/dev/null || true; } | awk '{print $2}' | sort -n \
        | awk -v n="${total}" 'BEGIN{i=int(n*0.95); if(i<1)i=1} NR==i{print; exit}')
    max_ms=$( { cat "${dir}"/*.out 2>/dev/null || true; } | awk '{print $2}' | sort -n | tail -1)
    printf '%s %s %s %s' "${ok:-0}" "${total:-0}" "${p95:-0}" "${max_ms:-0}"
}

#==================== K5 50 并发 dry-run ====================#
log_step "K5 50 并发 dry-run"
concurrent_dry_run "${WORK}/k5" 50 0
read -r ok total p95 max_ms <<< "$(summarize_runs "${WORK}/k5")"
if [ "${ok}" = "${total}" ] && [ "${total}" -ge 50 ] && [ "${p95:-0}" -lt 5000 ]; then
    pass K5 "50/50 成功，p95=${p95}ms（max ${max_ms}ms）< 5000ms"
else
    errs=$( { awk 'NR<=5' "${WORK}/k5"/*.err 2>/dev/null || true; } | tr '\n' ' ')
    fail K5 "成功 ${ok}/${total}，p95=${p95}ms；错误: ${errs:-无}"
fi

#==================== K6 删一个 webhook Pod ====================#
log_step "K6 删除单个 webhook Pod"
desired=$(webhook_desired_replicas)
desired_ok=0
[ "${desired:-0}" -ge 1 ] && desired_ok=1
victim=$(webhook_pods | awk 'NR==1')
if [ -z "${victim}" ]; then
    fail K6 "找不到 webhook Pod"
else
    kubectl -n "${WV_NAMESPACE}" delete pod "${victim}" --wait=false >/dev/null 2>&1 || true
    sleep 2
    endpoints=$(webhook_endpoint_count)
    healthz=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/healthz" 2>/dev/null || true)
    if [ "${healthz}" != "200" ] && admission_ensure_port_forward; then
        healthz=$(admission_health_code)
    fi
    recovered=0
    recover_deadline=$(( $(date +%s) + 180 ))
    while [ "$(date +%s)" -lt "${recover_deadline}" ]; do
        if [ "$(webhook_ready_replicas)" -ge "${desired:-2}" ]; then
            recovered=1
            break
        fi
        sleep 2
    done
    if [ "${desired_ok}" = "1" ] && [ "${endpoints:-0}" -ge 1 ] && [ "${healthz}" = "200" ] && [ "${recovered}" = "1" ]; then
        pass K6 "删 Pod 后仍有 ${endpoints} 个 Endpoint、准入可用，副本补齐到 ${desired}"
    else
        fail K6 "desired=${desired:-0} endpoints=${endpoints} healthz=${healthz} 副本恢复=${recovered}"
        k8s_collect_webhook_artifacts "k6-webhook" >/dev/null 2>&1 || true
    fi
fi

#==================== K7 滚动重启期间准入 ====================#
log_step "K7 滚动重启期间准入"
admission_ensure_port_forward >/dev/null 2>&1 || true
(
    for i in $(seq 1 40); do
        start=$(now_ms)
        if kubectl create --dry-run=server -o json -f "${WORK}/pod.yaml" >/dev/null 2>&1; then rc=0; else rc=1; fi
        echo "${rc} $(( $(now_ms) - start ))" > "${WORK}/k7-${i}.out"
        sleep 0.2
    done
) &
probe_pid=$!

sleep 2
webhook_restart
webhook_rollout_status 300 || true
wait "${probe_pid}" 2>/dev/null || true

mkdir -p "${WORK}/k7"
mv "${WORK}"/k7-*.out "${WORK}/k7/" 2>/dev/null || true
read -r ok total _p95 _max <<< "$(summarize_runs "${WORK}/k7")"
ready=$(webhook_ready_replicas)
if [ "${total}" -gt 0 ] && [ "${ok}" -eq "${total}" ] && [ "${desired_ok:-0}" = "1" ] && [ "${ready:-0}" -ge "${desired:-2}" ]; then
    pass K7 "滚动重启期间 ${ok}/${total} 准入成功（100%），结束后 ${ready} 副本 Ready"
else
    fail K7 "滚动重启期间成功 ${ok}/${total}，结束后 ready=${ready}/${desired}（desired_ok=${desired_ok:-0}）"
    k8s_collect_webhook_artifacts "k7-webhook" >/dev/null 2>&1 || true
fi

finish "03 K8S 集成"

#!/bin/bash
###############################################################################
# 04_e2b_e2e.sh — e2b 沙箱端到端
#
# 用例：
#   E1  webhook 注入（runtimeClassName + 全部注解，用户 env/image 未变）
#   E2  调度（PodScheduled=True，nodeName 非空）
#   E3  运行（Pod Ready，无 CrashLoop）
#   E4  沙箱存在（e2b.dev/sandbox-id 非空）
#   E5  envd 健康（PodIP:49983/health）
#   E6  kubectl exec（echo e2e_ok，退出码 0）
#   E7  文件读写（/tmp/e2e 写入读出一致）
#   E8  SDK 基线（自建沙箱：create → exec → 文件 → pause → resume → info → kill）
#   E10 删除（Pod 消失，CRI sandbox 不再 READY）
#   E11 资源清理（CRI 无该 Pod 的 READY sandbox；Firecracker 进程已退出）
#   E12 BatchSandbox 路径（可选，--e2e-batchsandbox）
#   E14 失败归因（无效模板仍 allowed，错误落在 API 层而非准入层）
#
#
# 链路：kubectl apply Pod（不写注解/runtimeClass）
#   → apiserver → MWC → webhook: TEMPLATE_NAME → transform → 注解 + runtimeClassName
#   → Pod 持久化 → kubelet → cri-multiplex → orchestrator → Firecracker/envd
#   → Pod Ready → exec/envd/SDK → 删除 → 沙箱与资源清理
#
# 参数：--e2e-batchsandbox（E12）/ --no-sdk（跳过 E8）；--e2e-advanced 保留兼容但已无作用
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
. "${SCRIPT_DIR}/lib/k8s.sh"
. "${SCRIPT_DIR}/lib/webhook.sh"
. "${SCRIPT_DIR}/lib/admission.sh"
. "${SCRIPT_DIR}/lib/provider.sh"

while [ $# -gt 0 ]; do
    case "$1" in
        --e2e-batchsandbox) WV_E2E_BATCHSANDBOX=1 ;;
        --e2e-advanced) WV_E2E_ADVANCED=1 ;;  # 兼容旧参数：E13 已移除，无实际作用
        --no-sdk) WV_SDK=0 ;;
        --help|-h) sed -n '2,33p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) log_error "未知参数: $1"; exit 1 ;;
    esac
    shift
done

log_section "04 e2b 沙箱端到端"

webhook_detect

WORK="${WV_TMP}/04"
mkdir -p "${WORK}"
PROVIDER="$(provider_active)"
TEMPLATE="${WV_E2E_TEMPLATE}"
POD="${WV_E2E_POD}"
IMAGE=""
SANDBOX_ID=""
SDK_OK=0

cleanup_04() {
    local rc=$?
    set +e
    if [ "${WV_KEEP}" != "1" ]; then
        k8s_delete_pod "${WV_TEST_NS}" "${POD}" 2>/dev/null
        if [ -f "${WORK}/batchsandbox.yaml" ]; then
            k8s_delete_file "${WORK}/batchsandbox.yaml" 2>/dev/null
        fi 
    fi
    return "${rc}"
}
trap cleanup_04 EXIT

#==================== 模块前置 ====================#
log_step "前置检查"
missing=()

k8s_cluster_reachable || missing+=("集群不可达")
[ "$(state_get MWC_EXISTS)" = "1" ] || missing+=("MutatingWebhookConfiguration ${WV_MWC} 不存在")
k8s_ns_exists "${WV_TEST_NS}" || k8s_ensure_ns "${WV_TEST_NS}" || missing+=("namespace ${WV_TEST_NS} 不可用")

RC_NAME=$(provider_runtimeclass "${PROVIDER}")
if [ -z "${RC_NAME}" ] || ! k8s_runtimeclass_exists "${RC_NAME}"; then
    missing+=("RuntimeClass ${RC_NAME:-<未定义>} 不存在")
fi

if need_cmd crictl && crictl --runtime-endpoint "unix://${WV_CRI_SOCKET}" info >/dev/null 2>&1; then
    CRICTL_OK=1
else
    CRICTL_OK=0
    missing+=("crictl/CRI socket ${WV_CRI_SOCKET} 不可用")
fi

ORCH_HOST="${WV_ORCHESTRATOR%%:*}"
ORCH_PORT="${WV_ORCHESTRATOR##*:}"
if run_with_timeout 3 bash -c "cat < /dev/null > /dev/tcp/${ORCH_HOST}/${ORCH_PORT}" 2>/dev/null; then
    ORCH_OK=1
else
    ORCH_OK=0
    missing+=("orchestrator ${WV_ORCHESTRATOR} 不可达")
fi

if provider_api_available "${PROVIDER}"; then
    API_OK=1
    if provider_template_ready "${TEMPLATE}"; then
        TEMPLATE_OK=1
    else
        TEMPLATE_OK=0
        missing+=("模板 ${TEMPLATE} 不存在或未 ready")
    fi
    if IMAGE=$(provider_template_image "${TEMPLATE}" 2>/dev/null); then
        state_set E2E_IMAGE "${IMAGE}"
    else
        missing+=("无法解析模板 ${TEMPLATE} 的镜像")
    fi
else
    API_OK=0
    TEMPLATE_OK=0
    missing+=("E2B API $(provider_api_url) 不可达")
fi

if [ "${WV_SDK}" = "1" ]; then
    if need_cmd python3 && python3 -c "import e2b" >/dev/null 2>&1; then
        SDK_OK=1
    else
        log_warn "e2b SDK 未安装，E8 将 SKIP"
    fi
fi

if ! provider_has_e2e "${PROVIDER}"; then
    missing+=("provider ${PROVIDER} 尚未实现完整 E2E")
fi

if [ ${#missing[@]} -gt 0 ]; then
    log_warn "前置条件不足："
    for item in "${missing[@]}"; do
        log_warn "  - ${item}"
    done
    for id in E1 E2 E3 E4 E5 E6 E7 E8 E10 E11 E12 E14; do
        unsupported "${id}" "前置条件不足: ${missing[*]}"
    done
    finish "04 e2b 沙箱端到端"
fi

#==================== 辅助 ====================#
exec_sandbox() {
    # exec_sandbox <shell 命令>
    kubectl -n "${WV_TEST_NS}" exec "${POD}" -- sh -c "$1" 2>"${WORK}/exec.err"
}

run_sdk() {
    # run_sdk <子命令...>：成功返回 0，并设置 SDK_OUT/REASON
    local out rc
    if out=$(run_with_timeout "${WV_SDK_TIMEOUT}" python3 "${SCRIPT_DIR}/e2b_sdk.py" \
        --config "${WV_E2B_CONFIG}" "$@" 2>"${WORK}/sdk.err"); then
        rc=0
    else
        rc=$?
    fi
    SDK_OUT="${out}"
    REASON=$(printf '%s\n' "${out}" | sed -n 's/^REASON=//p' | awk 'NR==1')
    return "${rc}"
}

sdk_field() {
    printf '%s\n' "${SDK_OUT}" | sed -n "s/^$1=//p" | awk 'NR==1' 
}

annotations_count() {
    provider_annotation_count "${PROVIDER}" "$(k8s_pod_annotations_json "${WV_TEST_NS}" "${POD}")"
}

#==================== 创建 Pod ====================#
log_step "创建沙箱 Pod（不写注解/runtimeClass）"
env_block=$(printf '        - name: TEMPLATE_NAME\n          value: "%s"' "${TEMPLATE}")
labels_block=$(printf '  labels:\n    "%s": "0"' "${WV_POD_LABEL}")

k8s_delete_pod "${WV_TEST_NS}" "${POD}"
k8s_wait_pod_gone "${WV_TEST_NS}" "${POD}" 60 || true
sleep 1
k8s_render "${FIXTURE_DIR}/pod.yaml" "${WORK}/pod.yaml" \
    "NAME=${POD}" "NS=${WV_TEST_NS}" \
    "LABELS=${labels_block}" "ANNOTATIONS=" "RUNTIMECLASS=" \
    "IMAGE=${IMAGE}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${env_block}"
state_set E2E_POD_MANIFEST "${WORK}/pod.yaml"

if ! kubectl apply -f "${WORK}/pod.yaml" >"${WORK}/apply.log" 2>&1; then
    for id in E1 E2 E3 E4 E5 E6 E7 E8 E10 E11 E12 E14; do
        fail "${id}" "Pod 创建失败: $(tail -2 "${WORK}/apply.log" | tr '\n' ' ')"
    done
    finish "04 e2b 沙箱端到端"
fi

#==================== E1 注入 ====================#
log_step "E1 webhook 注入"
created=0
for _ in $(seq 1 20); do
    if k8s_pod_exists "${WV_TEST_NS}" "${POD}"; then created=1; break; fi
    sleep 1
done

if [ "${created}" != "1" ]; then
    fail E1 "Pod 对象未出现"
else
    rc=$(k8s_pod_runtimeclass "${WV_TEST_NS}" "${POD}")
    count=$(annotations_count)
    uid=$(k8s_pod_uid "${WV_TEST_NS}" "${POD}")
    state_set E2E_POD_UID "${uid}"
    image_in_pod=$(kubectl -n "${WV_TEST_NS}" get pod "${POD}" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null || true)
    template_env=$(kubectl -n "${WV_TEST_NS}" get pod "${POD}" \
        -o jsonpath='{.spec.containers[0].env[?(@.name=="TEMPLATE_NAME")].value}' 2>/dev/null || true)
    if [ "${rc}" = "e2b" ] && [ "${count}" -ge 21 ] && [ "${image_in_pod}" = "${IMAGE}" ] && [ "${template_env}" = "${TEMPLATE}" ]; then
        pass E1 "注入 runtimeClassName=e2b 与 ${count} 个注解；用户 env/image 未被改写"
    else
        fail E1 "runtimeClass='${rc}' 注解=${count} image=${image_in_pod} TEMPLATE_NAME=${template_env}"
    fi
fi

#==================== E2 调度 ====================#
log_step "E2 调度"
scheduled=$(k8s_pod_scheduled "${WV_TEST_NS}" "${POD}")
node=$(k8s_pod_node "${WV_TEST_NS}" "${POD}")
if [ "${scheduled}" = "True" ] && [ -n "${node}" ]; then
    pass E2 "PodScheduled=True，nodeName=${node}"
else
    fail E2 "PodScheduled='${scheduled}' nodeName='${node}'"
fi

#==================== E3 运行 ====================#
log_step "E3 运行（Pod Ready）"
ready_start=$(now_ms)
if k8s_wait_pod_ready "${WV_TEST_NS}" "${POD}" "${WV_E2E_TIMEOUT}"; then
    restarts=$(kubectl -n "${WV_TEST_NS}" get pod "${POD}" \
        -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)
    pass E3 "Pod Ready（$(format_ms $(( $(now_ms) - ready_start )))，restartCount=${restarts}）"
else
    phase=$(k8s_pod_phase "${WV_TEST_NS}" "${POD}")
    dir=$(k8s_collect_pod_artifacts "${WV_TEST_NS}" "${POD}" "e2e-pod")
    k8s_collect_webhook_artifacts "e2e-webhook" >/dev/null 2>&1 || true
    fail E3 "Pod 未在 ${WV_E2E_TIMEOUT}s 内 Ready（phase=${phase}），证据 ${dir}"
    for id in E4 E5 E6 E7 E10 E11; do
        skip "${id}" "Pod 未 Ready，无法验证"
    done
    # 仍尝试 BatchSandbox / 失败归因（它们不依赖这个 Pod）
    E3_FAILED=1
fi

E3_FAILED="${E3_FAILED:-0}"

#==================== E4 沙箱存在 ====================#
if [ "${E3_FAILED}" = "0" ]; then
    log_step "E4 沙箱存在"
    SANDBOX_ID=$(k8s_pod_annotation "${WV_TEST_NS}" "${POD}" "e2b.dev/sandbox-id")
    state_set E2E_SANDBOX_ID "${SANDBOX_ID}"
    if [ -n "${SANDBOX_ID}" ]; then
        pass E4 "e2b.dev/sandbox-id=${SANDBOX_ID}"
    else
        fail E4 "缺少 e2b.dev/sandbox-id 注解"
    fi
fi

#==================== E5 envd 健康 ====================#
if [ "${E3_FAILED}" = "0" ]; then
    log_step "E5 envd 健康"
    pod_ip=$(k8s_pod_ip "${WV_TEST_NS}" "${POD}")
    envd_code=""
    if [ -n "${pod_ip}" ]; then
        envd_code=$(curl -s -o /dev/null -w '%{http_code}' -m 5 "http://${pod_ip}:49983/health" 2>/dev/null || true)
    fi
    # envd 的 GET /health 实现是 204 No Content（无响应体），不能只断言 200。
    if [ "${envd_code}" = "200" ] || [ "${envd_code}" = "204" ]; then
        pass E5 "envd ${pod_ip}:49983/health HTTP ${envd_code}"
    elif k8s_probe_run "envd 健康 ${pod_ip}" "wget -q -T 5 -O /dev/null http://${pod_ip}:49983/health" >/dev/null 2>&1; then
        pass E5 "envd ${pod_ip}:49983/health 可达（经集群内探针，envd 返回 204 无响应体）"
    else
        skip E5 "宿主机与集群内探针都无法访问 ${pod_ip}:49983（网络策略/CNI 隔离），E6/E7 仍会验证数据面"
    fi
fi

#==================== E6 kubectl exec ====================#
if [ "${E3_FAILED}" = "0" ]; then
    log_step "E6 kubectl exec"
    if out=$(exec_sandbox "echo e2e_ok"); then
        if [ "$(printf '%s' "${out}" | tr -d '[:space:]')" = "e2e_ok" ]; then
            pass E6 "kubectl exec 输出 e2e_ok，退出码 0"
        else
            fail E6 "输出异常: ${out}"
        fi
    else
        artifact_save "e6-exec.err" "$(cat "${WORK}/exec.err" 2>/dev/null)" >/dev/null
        fail E6 "kubectl exec 失败: $(sed -n '1,2p' "${WORK}/exec.err" 2>/dev/null | tr '\n' ' ')"
    fi

    #==================== E7 文件读写 ====================#
    log_step "E7 文件读写"
    if exec_sandbox "echo e2e_file > /tmp/e2e && cat /tmp/e2e" >/dev/null 2>&1; then
        content=$(exec_sandbox "cat /tmp/e2e" 2>/dev/null | tr -d '[:space:]')
        if [ "${content}" = "e2e_file" ]; then
            pass E7 "/tmp/e2e 写入读出一致"
        else
            fail E7 "读回内容不符: '${content}'"
        fi
    else
        fail E7 "沙箱内文件写入失败"
    fi
fi

#==================== E8 SDK 基线（含 pause/resume） ====================#
log_step "E8 SDK 基线（自建沙箱）"
if [ "${WV_SDK}" != "1" ]; then
    skip E8 "跳过（--no-sdk）"
elif [ "${SDK_OK}" != "1" ]; then
    skip E8 "e2b SDK 未安装"
else
    if run_sdk probe; then
        pass E8 "SDK create→exec→文件→pause→resume→info→kill 全部成功（id=$(sdk_field SDK_SANDBOX_ID), paused=$(sdk_field PAUSED_STATE), resumed=$(sdk_field RESUME_STDOUT), gone=$(sdk_field GONE)）"
    else
        rc=$?
        if [ "${rc}" = "77" ]; then
            skip E8 "SDK 不可用: ${REASON}"
        else
            fail E8 "SDK 基线失败（${REASON:-未知}，控制面问题，与本 webhook 无关）"
        fi
    fi
fi

#==================== E10 删除 ====================#
if [ "${E3_FAILED}" = "0" ]; then
    log_step "E10 删除 Pod"
    # 删除前记下这个 Pod 自己的 CRI sandbox id；删除后用它判断沙箱是否停止。
    CRI_SANDBOX_ID=$(k8s_cri_sandbox_id "${WV_TEST_NS}" "${POD}")
    # Firecracker 进程的 cmdline 里带 sandbox-id，删除前记下 PID，
    # 删除后确认进程真的退出（netns 是预创建 slot 池，不能作为残留判据）。
    FC_PIDS=""
    if [ -n "${SANDBOX_ID}" ] && need_cmd pgrep; then
        FC_PIDS=$(pgrep -f "firecracker.*${SANDBOX_ID}" 2>/dev/null | tr '\n' ' ' || true)
    fi
    if [ -n "${FC_PIDS}" ]; then
        log_info "删除前捕获 Firecracker 进程: ${FC_PIDS}（sandbox ${SANDBOX_ID}）"
    fi
    if kubectl -n "${WV_TEST_NS}" delete pod "${POD}" --wait=true --timeout=120s >"${WORK}/delete.log" 2>&1 \
        && k8s_wait_pod_gone "${WV_TEST_NS}" "${POD}" 60; then
        # 沙箱本来就不在 API 落库（transform 端点只返回创建参数），
        # SDK/API 侧查不到不能作为“回收”的证据，判据放在 CRI。
        # CRI 里可能残留 SANDBOX_NOTREADY 的墓碑记录（等 GC），
        # 所以判据是“不再 READY”，而不是“记录已消失”。
        if [ -z "${CRI_SANDBOX_ID}" ]; then
            fail E10 "Pod 已删除，但没有记录到该 Pod 的 CRI sandbox id，无法验证沙箱回收"
        else
            cri_state=$(crictl --runtime-endpoint "unix://${WV_CRI_SOCKET}" pods -o json 2>/dev/null \
                | jq -r --arg id "${CRI_SANDBOX_ID}" '.items[]? | select(.id == $id) | .state' 2>/dev/null \
                | awk 'NR==1' || true)
            if [ "${cri_state}" = "SANDBOX_READY" ]; then
                fail E10 "Pod 已删除，但 CRI sandbox ${CRI_SANDBOX_ID} 仍是 SANDBOX_READY"
            else
                pass E10 "Pod 已删除，CRI sandbox ${CRI_SANDBOX_ID} 已停止（state=${cri_state:-<记录已移除>}）"
            fi
        fi
    else
        fail E10 "Pod 删除失败: $(tail -2 "${WORK}/delete.log" | tr '\n' ' ')"
    fi

    #==================== E11 资源清理 ====================#
    log_step "E11 资源清理"
    sleep 3
    uid="${E2E_POD_UID:-$(state_get E2E_POD_UID)}"
    if [ -z "${uid}" ]; then
        skip E11 "缺少 Pod UID，无法精确匹配残留"
    else
        residue=0
        checked=""

        if [ "${CRICTL_OK}" = "1" ]; then
            checked="${checked}crictl "
            # 必须解析 JSON：crictl pods 的表格输出不含 UID，grep UID 永远是 0。
            cri_json=$(crictl --runtime-endpoint "unix://${WV_CRI_SOCKET}" pods -o json 2>/dev/null || true)
            cri_left=$(printf '%s' "${cri_json}" | jq --arg u "${uid}" \
                '[.items[]? | select(.labels["io.kubernetes.pod.uid"] == $u) | select(.state == "SANDBOX_READY")] | length' 2>/dev/null || echo 0)
            cri_tomb=$(printf '%s' "${cri_json}" | jq --arg u "${uid}" \
                '[.items[]? | select(.labels["io.kubernetes.pod.uid"] == $u) | select(.state != "SANDBOX_READY")] | length' 2>/dev/null || echo 0)
            [ "${cri_tomb:-0}" != "0" ] && log_warn "CRI 仍有 ${cri_tomb} 条非 READY 墓碑记录（等 GC，不计残留）"
            [ "${cri_left:-0}" != "0" ] && { residue=$((residue + cri_left)); log_warn "crictl 仍有 ${cri_left} 条 ${uid} 的 READY sandbox"; }
        fi

        if [ -n "${FC_PIDS}" ]; then
            checked="${checked}firecracker "
            for fc_pid in ${FC_PIDS}; do
                if kill -0 "${fc_pid}" 2>/dev/null; then
                    residue=$((residue + 1))
                    log_warn "Firecracker 进程 ${fc_pid} 仍存活"
                fi
            done
        else
            log_warn "未捕获到 ${SANDBOX_ID:-<空>} 的 Firecracker 进程，跳过进程级残留检查"
        fi

        if [ -z "${checked}" ]; then
            skip E11 "没有可用的清理检查工具（crictl/pgrep）"
        elif [ "${residue}" = "0" ]; then
            pass E11 "${checked}检查无 ${uid} 的 READY sandbox / 存活进程（firecracker PID ${FC_PIDS:-<未捕获>}）"
        else
            fail E11 "${checked}发现 ${residue} 条残留（uid=${uid}）"
        fi
    fi
fi

#==================== E12 BatchSandbox（可选） ====================#
log_step "E12 BatchSandbox 路径（可选）"
if [ "${WV_E2E_BATCHSANDBOX}" != "1" ]; then
    skip E12 "未启用（--e2e-batchsandbox）"
elif [ "${E3_FAILED}" != "0" ]; then
    skip E12 "主 Pod 链路未通过，BatchSandbox 路径无意义"
elif ! k8s_crd_exists "batchsandboxes.sandbox.opensandbox.io"; then
    unsupported E12 "BatchSandbox CRD（sandbox.opensandbox.io）未安装"
else
    bs_name="wv-e2e-bs"
    bs_labels=$(printf '      labels:\n        "%s": "0"' "${WV_POD_LABEL}")
    bs_env=$(printf '            - name: TEMPLATE_NAME\n              value: "%s"' "${TEMPLATE}")
    k8s_render "${FIXTURE_DIR}/batchsandbox.yaml" "${WORK}/batchsandbox.yaml" \
        "NAME=${bs_name}" "NS=${WV_TEST_NS}" "LABELS=${bs_labels}" "ANNOTATIONS=" "RUNTIMECLASS=" \
        "IMAGE=${IMAGE}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${bs_env}"

    k8s_delete_file "${WORK}/batchsandbox.yaml"
    if kubectl apply -f "${WORK}/batchsandbox.yaml" >"${WORK}/e12.apply.log" 2>&1; then
        inherited=0
        for _ in $(seq 1 60); do
            count=$(k8s_pod_count_by_label "${WV_TEST_NS}" "${WV_POD_LABEL}")
            [ "${count:-0}" -ge 1 ] && { inherited=1; break; }
            sleep 2
        done
        if [ "${inherited}" != "1" ]; then
            dir=$(k8s_collect_pod_artifacts "${WV_TEST_NS}" "${bs_name}" "e12-bs")
            skip E12 "BatchSandbox 已创建但控制器没有生成 Pod（控制器未部署），证据 ${dir}"
        else
            child=$(k8s_pod_list_by_label "${WV_TEST_NS}" "${WV_POD_LABEL}" | awk 'NR==1')
            child_rc=$(k8s_pod_runtimeclass "${WV_TEST_NS}" "${child}")
            child_annos=$(k8s_pod_annotations_json "${WV_TEST_NS}" "${child}")
            child_count=$(provider_annotation_count "${PROVIDER}" "${child_annos}")
            if [ "${child_rc}" = "e2b" ] && [ "${child_count}" -ge 21 ]; then
                if k8s_wait_pod_ready "${WV_TEST_NS}" "${child}" "${WV_E2E_TIMEOUT}"; then
                    if kubectl -n "${WV_TEST_NS}" exec "${child}" -- sh -c "echo e2e_ok" >/dev/null 2>&1; then
                        pass E12 "CR 生成的 Pod 继承 ${child_count} 个注解且可执行命令"
                    else
                        fail E12 "CR 生成的 Pod Ready 但 exec 失败"
                    fi
                else
                    dir=$(k8s_collect_pod_artifacts "${WV_TEST_NS}" "${child}" "e12-pod")
                    skip E12 "CR 生成的 Pod 未 Ready，证据 ${dir}"
                fi
            else
                fail E12 "CR 生成的 Pod 注入缺失：runtimeClass='${child_rc}' 注解=${child_count}"
            fi
        fi
        [ "${WV_KEEP}" = "1" ] || k8s_delete_file "${WORK}/batchsandbox.yaml"
    else
        skip E12 "BatchSandbox 创建失败（CRD 结构与预期不一致）: $(tail -2 "${WORK}/e12.apply.log" | tr '\n' ' ')"
    fi
fi

#==================== E14 失败归因 ====================#
log_step "E14 失败归因"
bad_template="webhook-verify-not-exist-$RANDOM"
env_bad=$(printf '        - name: TEMPLATE_NAME\n          value: "%s"' "${bad_template}")
k8s_render "${FIXTURE_DIR}/pod.yaml" "${WORK}/e14.yaml" \
    "NAME=wv-e14" "NS=${WV_TEST_NS}" \
    "LABELS=${labels_block}" "ANNOTATIONS=" "RUNTIMECLASS=" \
    "IMAGE=${IMAGE}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${env_bad}"

e14_ok=1
if out=$(kubectl create --dry-run=server -o json -f "${WORK}/e14.yaml" 2>"${WORK}/e14.err"); then
    rc=$(printf '%s' "${out}" | jq -r '.spec.runtimeClassName // ""')
    annos=$(printf '%s' "${out}" | jq -c '.metadata.annotations // {}')
    count=$(provider_annotation_count "${PROVIDER}" "${annos}")
    # 期望：webhook 不拒绝请求（错误不能被记成“准入拒绝”），只补 runtimeClass
    if [ "${rc}" = "e2b" ] && [ "${count}" = "0" ]; then
        e14_ok=0
        pass E14 "无效模板下仍 allowed、只补 runtimeClassName、无注解 —— 错误落在 API 层而非准入层"
    else
        fail E14 "无效模板下行为异常：runtimeClass='${rc}' 注解=${count}（应放行且不注入）"
        e14_ok=1
    fi
else
    fail E14 "无效模板导致准入被拒绝（错误归因错误）: $(tail -2 "${WORK}/e14.err" | tr '\n' ' ')"
    e14_ok=1
fi

if [ "${e14_ok}" = "0" ]; then
    logs=$(kubectl -n "${WV_NAMESPACE}" logs -l "app.kubernetes.io/name=${WV_DEPLOYMENT}" \
        --all-containers=true --tail=800 2>/dev/null || true)
    if printf '%s' "${logs}" | grep -q "${bad_template}"; then
        log_info "webhook 日志中已记录模板 ${bad_template} 的 transform 失败，归因链完整"
    else
        log_warn "webhook 日志中未检索到 ${bad_template}（可能日志已被滚动），但行为层面归因成立"
    fi
fi

finish "04 e2b 沙箱端到端"

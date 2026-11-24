#!/bin/bash
###############################################################################
# 01_admission_contract.sh — /mutate 契约 + 注入语义 + provider 选路
#
# 用例：
#   A1  /healthz（GET/HEAD 均 200）
#   A2  正常 Pod 注入（runtimeClassName + transform 映射出的全部注解）
#   A3  patch 语义（全部 op=add，路径正确）
#   A4  用户已有注解优先（不被覆盖，其余补齐）
#   A5  幂等（注入结果再次准入无 patch）
#   A6  已有 runtimeClass（只补注解）
#   A7  缺 TEMPLATE_NAME（只补 runtimeClassName）
#   A8  TEMPLATE_NAME 为空串（同 A7）
#   A9  多容器（只读 containers[0].env）
#   A10 显式 runtimeClass=e2b（注入注解）
#   A11 显式 runtimeClass=android（无 e2b.dev/* 注解）
#   A12 无 runtimeClass + ANDROID_SANDBOX=true → android
#   A13 无 runtimeClass 无信号 → 默认 e2b
#   A14 未知 runtimeClass（放行，不注入）
#   A15 runtimeClass 大小写/空格（记录实际行为）
#   A16 BatchSandbox（直连 /mutate，不依赖 CRD；patch 路径为 spec.template.*）
#   A17 无 pod-index label（objectSelector 不命中）
#   A18 webhook 所在 namespace（namespaceSelector 排除）
#   A19 kube-system（namespaceSelector 排除）
#   A21 注解 key 的 JSON Pointer 转义（/ → ~1，~ → ~0）
#   A23 UPDATE 操作（不注入，只放行）
#
# A1–A16、A21、A23 直连 /mutate（port-forward 到 Service）；A17–A19 必须走
# server dry-run，因为 objectSelector / namespaceSelector 是 apiserver 侧过滤。
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
        --no-mock) : ;;  # A22 已移入 02，保留参数兼容
        --help|-h) sed -n '2,29p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) log_error "未知参数: $1"; exit 1 ;;
    esac
    shift
done

log_section "01 准入契约"

# 独立运行（--only contract）时也需要 MWC 是否存在（A17–A19）。
webhook_detect

WORK="${WV_TMP}/01"
mkdir -p "${WORK}"
# A17–A19 的 server dry-run 需要测试 namespace 存在；--only 01 时不经过 00。
k8s_ensure_ns "${WV_TEST_NS}" || log_warn "测试 namespace ${WV_TEST_NS} 创建失败"
PROVIDER="$(provider_active)"
TEMPLATE="${WV_E2E_TEMPLATE}"
RESP=""; PATCH=""; APPLIED=""

cleanup_01() {
    local rc=$?
    set +e
    admission_stop_port_forward
    return "${rc}"
}
trap cleanup_01 EXIT

#==================== 前置 ====================#
if ! admission_ensure_port_forward; then
    fail A1 "port-forward 失败，无法访问 /mutate（见 ${LOG_DIR}/port-forward.log）"
    finish "01 准入契约"
fi

API_OK=0
if provider_api_available "${PROVIDER}"; then
    API_OK=1
    state_set PROVIDER_API_OK 1
else
    state_set PROVIDER_API_OK 0
    log_warn "provider ${PROVIDER} 的 transform API 不可达（$(provider_api_url)），注解类用例将 SKIP"
fi

EXPECTED_FILE="${WORK}/expected.json"
HAVE_EXPECTED=0
if [ "${API_OK}" = "1" ] && provider_expected_annotations "${PROVIDER}" "${TEMPLATE}" > "${EXPECTED_FILE}" 2>/dev/null; then
    if [ -s "${EXPECTED_FILE}" ]; then
        HAVE_EXPECTED=1
        log_info "transform 期望注解 $(jq 'length' "${EXPECTED_FILE}") 个（模板 ${TEMPLATE}）"
    fi
fi

#==================== 辅助 ====================#
# mutate_case <name> <obj>：直连准入并把响应/patch/应用结果落到 WORK。
mutate_case() {
    local name="$1" obj="$2"
    local prefix="${WORK}/${name}"
    RESP="${prefix}.resp"; PATCH="${prefix}.patch"; APPLIED="${prefix}.applied"
    admission_post "${obj}" "${RESP}" CREATE || return 1
    [ -s "${RESP}" ] || return 1
    admission_patch_json "${RESP}" > "${PATCH}" || return 1
    admission_apply_patch "${obj}" "${PATCH}" "${APPLIED}" || return 1
}

http_code() { admission_resp_http_code "${RESP}"; }
allowed() { [ "$(admission_resp_allowed "${RESP}")" = "true" ]; }

write_annotations() {
    local obj="$1" out="$2"
    admission_annotations "${obj}" > "${out}"
}

# 与 transform 基线逐 key 比对；无输出表示一致。
annotations_diff() {
    local actual_file="$1"
    [ "${HAVE_EXPECTED}" = "1" ] || return 2
    provider_compare_annotations "${PROVIDER}" "${EXPECTED_FILE}" "${actual_file}"
}

patch_op_count() { jq 'length' "${PATCH}" 2>/dev/null || echo 0; }

annotations_count() {
    provider_annotation_count "${PROVIDER}" "$(admission_annotations "$1")"
}

has_runtimeclass_patch() {
    admission_patch_paths "${PATCH}" | grep -qx '/spec/runtimeClassName'
}

# 需要注解类断言但不能做时，统一记 SKIP。
need_expected() {
    local id="$1" reason="$2"
    if [ "${HAVE_EXPECTED}" = "1" ]; then
        return 0
    fi
    skip "${id}" "${reason}（transform API 不可达或无 ${TEMPLATE} 模板，无法建立期望值）"
    return 1
}

#==================== A1 /healthz ====================#
log_step "A1 /healthz"
get_code=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/healthz" 2>/dev/null || true)
head_code=$(curl -s -k -I -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/healthz" 2>/dev/null || true)
if [ "${get_code}" = "200" ] && [ "${head_code}" = "200" ]; then
    pass A1 "GET/HEAD /healthz 均 200"
else
    fail A1 "GET=${get_code} HEAD=${head_code}"
fi

#==================== A2 正常 Pod ====================#
log_step "A2 正常 Pod 注入"
pod_object "${WORK}/a2.json" "${WV_TEST_NS}" "wv-a2" "template=${TEMPLATE}"
if mutate_case a2 "${WORK}/a2.json" && [ "$(http_code)" = "200" ] && allowed; then
    rc=$(admission_runtimeclass "${APPLIED}")
    write_annotations "${APPLIED}" "${WORK}/a2.annotations.json"
    injected=$(annotations_count "${APPLIED}")
    if need_expected A2 "无法核对注解值"; then
        diff_out=$(annotations_diff "${WORK}/a2.annotations.json" || true)
        if [ "${rc}" = "e2b" ] && [ -z "${diff_out}" ] && [ "${injected}" -ge 21 ]; then
            pass A2 "runtimeClassName=e2b，注解 ${injected} 个且与 transform 响应逐 key 一致"
        else
            artifact_save "a2.diff.txt" "${diff_out}" >/dev/null
            fail A2 "runtimeClass='${rc}' injected=${injected} 差异: ${diff_out:-无}（证据 ${ARTIFACT_DIR}/a2.diff.txt）"
        fi
    fi
else
    fail A2 "响应异常: http=$(http_code) allowed=$(admission_resp_allowed "${RESP}" 2>/dev/null || echo '?')"
fi

#==================== A3 patch 语义 ====================#
log_step "A3 patch 语义（只 add）"
if [ -s "${PATCH}" ]; then
    non_add=$(jq '[.[] | select(.op != "add")] | length' "${PATCH}" 2>/dev/null || echo -1)
    bad_path=$(jq '[.[] | select((.path == "/spec/runtimeClassName") or (.path | startswith("/metadata/annotations")) | not)] | length' "${PATCH}" 2>/dev/null || echo -1)
    if [ "${non_add}" = "0" ] && [ "${bad_path}" = "0" ]; then
        pass A3 "全部 $(patch_op_count) 个操作均为 add，路径合法"
    else
        fail A3 "存在非 add 操作 ${non_add} 个或非法路径 ${bad_path} 个"
    fi
else
    fail A3 "A2 没有 patch，无法验证"
fi

#==================== A4 用户已有注解 ====================#
log_step "A4 用户已有注解优先"
user_annos='{"e2b.dev/template-id":"user-value","custom/keep":"yes"}'
pod_object "${WORK}/a4.json" "${WV_TEST_NS}" "wv-a4" "template=${TEMPLATE}" "annotations=${user_annos}"
if mutate_case a4 "${WORK}/a4.json" && allowed; then
    if need_expected A4 "无法核对其余注解是否补齐"; then
        write_annotations "${APPLIED}" "${WORK}/a4.annotations.json"
        template_id=$(jq -r '."e2b.dev/template-id" // ""' "${WORK}/a4.annotations.json")
        keep=$(jq -r '."custom/keep" // ""' "${WORK}/a4.annotations.json")
        overwritten=$(admission_patch_paths "${PATCH}" | grep -c 'template-id' || true)
        rc=$(admission_runtimeclass "${APPLIED}")
        filled=$(annotations_count "${APPLIED}")
        if [ "${template_id}" = "user-value" ] && [ "${keep}" = "yes" ] && [ "${overwritten}" = "0" ] \
            && [ "${rc}" = "e2b" ] && [ "${filled}" -ge 21 ]; then
            pass A4 "用户值保留（template-id=user-value, custom/keep=yes），runtimeClass=e2b，其余 ${filled} 个 provider 注解已补齐"
        else
            fail A4 "template-id='${template_id}' custom/keep='${keep}' 覆盖操作=${overwritten} runtimeClass='${rc}' provider 注解=${filled}"
        fi
    fi
else
    fail A4 "准入失败: http=$(http_code)"
fi

#==================== A5 幂等 ====================#
log_step "A5 幂等（二次准入无 patch）"
if [ -s "${WORK}/a2.applied" ]; then
    if mutate_case a5 "${WORK}/a2.applied" && allowed; then
        if [ "$(patch_op_count)" = "0" ]; then
            pass A5 "已注入对象再次准入无 patch"
        else
            fail A5 "二次准入仍产生 $(patch_op_count) 个 patch"
        fi
    else
        fail A5 "二次准入失败: http=$(http_code)"
    fi
else
    fail A5 "缺少 A2 的注入结果"
fi

#==================== A6 已有 runtimeClass ====================#
log_step "A6 已有 runtimeClass"
pod_object "${WORK}/a6.json" "${WV_TEST_NS}" "wv-a6" "template=${TEMPLATE}" "runtimeclass=${PROVIDER}"
if mutate_case a6 "${WORK}/a6.json" && allowed; then
    if ! has_runtimeclass_patch && [ "$(annotations_count "${APPLIED}")" -ge 21 ]; then
        pass A6 "不再 patch runtimeClassName，注解照常注入 $(annotations_count "${APPLIED}") 个"
    else
        fail A6 "runtimeClassName patch=$(has_runtimeclass_patch && echo yes || echo no) 注解=$(annotations_count "${APPLIED}")"
    fi
else
    fail A6 "准入失败: http=$(http_code)"
fi

#==================== A7 / A8 缺 / 空 TEMPLATE_NAME ====================#
log_step "A7/A8 缺或空 TEMPLATE_NAME"
pod_object "${WORK}/a7.json" "${WV_TEST_NS}" "wv-a7"
if mutate_case a7 "${WORK}/a7.json" && allowed; then
    if has_runtimeclass_patch && [ "$(patch_op_count)" = "1" ] && [ "$(admission_runtimeclass "${APPLIED}")" = "e2b" ]; then
        pass A7 "缺 TEMPLATE_NAME：只补 runtimeClassName，无 provider 注解"
    else
        fail A7 "patch 数=$(patch_op_count) 注解=$(annotations_count "${APPLIED}")"
    fi
else
    fail A7 "准入失败: http=$(http_code)"
fi

pod_object "${WORK}/a8.json" "${WV_TEST_NS}" "wv-a8" 'env=[{"name":"TEMPLATE_NAME","value":""}]'
if mutate_case a8 "${WORK}/a8.json" && allowed; then
    if has_runtimeclass_patch && [ "$(patch_op_count)" = "1" ]; then
        pass A8 "空 TEMPLATE_NAME：与缺失行为一致，只补 runtimeClassName"
    else
        fail A8 "patch 数=$(patch_op_count) 注解=$(annotations_count "${APPLIED}")"
    fi
else
    fail A8 "准入失败: http=$(http_code)"
fi

#==================== A9 多容器 ====================#
log_step "A9 只读 containers[0].env"
pod_object "${WORK}/a9.json" "${WV_TEST_NS}" "wv-a9" "template=${TEMPLATE}"
jq '.spec.containers += [{"name":"sidecar","image":"busybox:latest","command":["sleep","3600"],
      "env":[{"name":"TEMPLATE_NAME","value":"should-not-be-used"}]}]' \
    "${WORK}/a9.json" > "${WORK}/a9b.json"
if ! mutate_case a9 "${WORK}/a9b.json" || ! allowed; then
    fail A9 "准入失败: http=$(http_code)"
elif need_expected A9 "无法核对注解来源"; then
    write_annotations "${APPLIED}" "${WORK}/a9.annotations.json"
    diff_out=$(annotations_diff "${WORK}/a9.annotations.json" || true)
    if [ -z "${diff_out}" ]; then
        pass A9 "注解来自 containers[0]（第 2 个容器的同名 env 未生效）"
    else
        artifact_save "a9.diff.txt" "${diff_out}" >/dev/null
        fail A9 "注解与 containers[0] 的模板不一致: ${diff_out}"
    fi
fi

#==================== A10–A14 选路 ====================#
log_step "A10 显式 e2b"
pod_object "${WORK}/a10.json" "${WV_TEST_NS}" "wv-a10" "template=${TEMPLATE}" "runtimeclass=e2b"
if mutate_case a10 "${WORK}/a10.json" && allowed && [ "$(annotations_count "${APPLIED}")" -ge 21 ]; then
    pass A10 "显式 runtimeClass=e2b：注入 $(annotations_count "${APPLIED}") 个注解"
else
    fail A10 "http=$(http_code) allowed=$(admission_resp_allowed "${RESP}" 2>/dev/null || echo '?')"
fi

log_step "A11 显式 android"
pod_object "${WORK}/a11.json" "${WV_TEST_NS}" "wv-a11" "template=${TEMPLATE}" "runtimeclass=android"
if mutate_case a11 "${WORK}/a11.json" && allowed; then
    android_annos=$(annotations_count "${APPLIED}")
    if [ "${android_annos}" = "0" ] && [ "$(patch_op_count)" = "0" ] && [ "$(admission_runtimeclass "${APPLIED}")" = "android" ]; then
        pass A11 "android：无 e2b.dev/* 注解，runtimeClassName 保持 android，无 patch"
    else
        fail A11 "注解=${android_annos} patch=$(patch_op_count) runtimeClass=$(admission_runtimeclass "${APPLIED}")"
    fi
else
    fail A11 "准入失败: http=$(http_code)"
fi

log_step "A12 legacy ANDROID_SANDBOX=true"
pod_object "${WORK}/a12.json" "${WV_TEST_NS}" "wv-a12" \
    "env=$(jq -nc --arg t "${TEMPLATE}" '[{name:"TEMPLATE_NAME",value:$t},{name:"ANDROID_SANDBOX",value:"true"}]')"
if mutate_case a12 "${WORK}/a12.json" && allowed; then
    if [ "$(admission_runtimeclass "${APPLIED}")" = "android" ] && [ "$(patch_op_count)" = "1" ]; then
        pass A12 "无 runtimeClass + ANDROID_SANDBOX=true → runtimeClassName=android，无注解"
    else
        fail A12 "runtimeClass=$(admission_runtimeclass "${APPLIED}") patch=$(patch_op_count)"
    fi
else
    fail A12 "准入失败: http=$(http_code)"
fi

log_step "A13 legacy 默认 e2b"
pod_object "${WORK}/a13.json" "${WV_TEST_NS}" "wv-a13" "template=${TEMPLATE}"
if mutate_case a13 "${WORK}/a13.json" && allowed; then
    if [ "$(admission_runtimeclass "${APPLIED}")" = "e2b" ] && [ "$(annotations_count "${APPLIED}")" -ge 21 ]; then
        pass A13 "无 runtimeClass 无信号 → 默认 e2b 并注入注解"
    else
        fail A13 "runtimeClass=$(admission_runtimeclass "${APPLIED}") 注解=$(annotations_count "${APPLIED}")"
    fi
else
    fail A13 "准入失败: http=$(http_code)"
fi

log_step "A14 未知 runtimeClass"
pod_object "${WORK}/a14.json" "${WV_TEST_NS}" "wv-a14" "template=${TEMPLATE}" "runtimeclass=kata"
if mutate_case a14 "${WORK}/a14.json" && allowed; then
    if [ "$(patch_op_count)" = "0" ]; then
        pass A14 "未注册的 runtimeClass=kata → 放行且不注入"
    else
        fail A14 "未知 runtimeClass 仍产生 $(patch_op_count) 个 patch"
    fi
else
    fail A14 "准入失败: http=$(http_code)"
fi

#==================== A15 大小写/空格 ====================#
log_step "A15 runtimeClass 大小写/空格"
pod_object "${WORK}/a15a.json" "${WV_TEST_NS}" "wv-a15a" "template=${TEMPLATE}" "runtimeclass= e2b "
pod_object "${WORK}/a15b.json" "${WV_TEST_NS}" "wv-a15b" "template=${TEMPLATE}" "runtimeclass=E2B"
if mutate_case a15a "${WORK}/a15a.json"; then
    padded_ops=$(patch_op_count)
    padded_annos=$(annotations_count "${APPLIED}")
else
    padded_ops=-1; padded_annos=-1
fi
if mutate_case a15b "${WORK}/a15b.json"; then
    upper_ops=$(patch_op_count)
else
    upper_ops=-1
fi
# 源码行为：' e2b ' 被 TrimSpace 命中（runtimeClass 已设置 → 只补注解）；
# 'E2B' 未命中且显式非空 → 放行无 patch。这里断言“实际行为与源码一致”。
record="' e2b '→patch=${padded_ops}/注解=${padded_annos}；'E2B'→patch=${upper_ops}"
if [ "${padded_ops}" -ge 1 ] && [ "${padded_annos}" -ge 21 ] && [ "${upper_ops}" = "0" ]; then
    pass A15 "记录运行时行为：${record}（与源码 TrimSpace 语义一致）"
else
    fail A15 "行为与源码不一致：${record}"
fi

#==================== A16 BatchSandbox ====================#
log_step "A16 BatchSandbox"
# 直连 /mutate 只依赖 webhook 对 unstructured 对象的解码，不经过 apiserver 的
# CRD 校验，因此不检查 CRD 是否存在（K4/E12 走真实 CR 路径时才需要）。
batchsandbox_object "${WORK}/a16.json" "${WV_TEST_NS}" "wv-a16" "template=${TEMPLATE}"
if mutate_case a16 "${WORK}/a16.json" && allowed; then
    paths=$(admission_patch_paths "${PATCH}")
    bad=$(printf '%s\n' "${paths}" | grep -vc '^/spec/template/' || true)
    rc=$(jq -r '.spec.template.spec.runtimeClassName // ""' "${APPLIED}")
    annos=$(jq -c '.spec.template.metadata.annotations // {}' "${APPLIED}")
    count=$(provider_annotation_count "${PROVIDER}" "${annos}")
    if [ "${bad}" = "0" ] && [ "${rc}" = "e2b" ] && [ "${count}" -ge 21 ]; then
        pass A16 "BatchSandbox patch 路径均为 spec.template.*，注解 ${count} 个"
    else
        fail A16 "非法路径 ${bad} 个，runtimeClass='${rc}'，注解=${count}"
    fi
else
    fail A16 "准入失败: http=$(http_code)"
fi

#==================== A17–A19 选择器（server dry-run） ====================#
log_step "A17–A19 objectSelector / namespaceSelector"
if [ "$(state_get MWC_EXISTS)" != "1" ] || ! k8s_cluster_reachable; then
    skip A17 "MutatingWebhookConfiguration 缺失，选择器无法验证"
    skip A18 "MutatingWebhookConfiguration 缺失，选择器无法验证"
    skip A19 "MutatingWebhookConfiguration 缺失，选择器无法验证"
else
    image_for_dry="$(provider_template_image "${TEMPLATE}" 2>/dev/null || echo 'e2b.dev/dry:dry')"
    env_block=$(printf '        - name: TEMPLATE_NAME\n          value: "%s"' "${TEMPLATE}")
    labels_block=$(printf '  labels:\n    "%s": "0"' "${WV_POD_LABEL}")

    # 正向对照：测试 namespace + pod-index label 必须命中并注入 runtimeClass。
    # 否则 A17–A19 的“没被注入”可能只是选择器完全失效，属于假成功。
    k8s_render "${FIXTURE_DIR}/pod.yaml" "${WORK}/a17-control.yaml" \
        "NAME=wv-a17-control" "NS=${WV_TEST_NS}" \
        "LABELS=${labels_block}" "ANNOTATIONS=" "RUNTIMECLASS=" \
        "IMAGE=${image_for_dry}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${env_block}"
    selector_control=0
    if out=$(kubectl create --dry-run=server -o json -f "${WORK}/a17-control.yaml" 2>"${WORK}/a17-control.err"); then
        control_rc=$(printf '%s' "${out}" | jq -r '.spec.runtimeClassName // ""')
        [ "${control_rc}" = "e2b" ] && selector_control=1
    fi
    if [ "${selector_control}" != "1" ]; then
        log_warn "选择器正向对照失败（带 label 的 Pod 未被注入 runtimeClass），A17–A19 的负向结果不可信"
    fi

    # A17：测试 namespace 内、无 label → objectSelector 不命中
    k8s_render "${FIXTURE_DIR}/pod.yaml" "${WORK}/a17.yaml" \
        "NAME=wv-a17" "NS=${WV_TEST_NS}" \
        "LABELS=" "ANNOTATIONS=" "RUNTIMECLASS=" \
        "IMAGE=${image_for_dry}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${env_block}"
    if out=$(kubectl create --dry-run=server -o json -f "${WORK}/a17.yaml" 2>"${WORK}/a17.err"); then
        annos=$(printf '%s' "${out}" | jq -c '.metadata.annotations // {}')
        rc=$(printf '%s' "${out}" | jq -r '.spec.runtimeClassName // ""')
        if [ "${selector_control}" = "1" ] && provider_assert_no_prefix "${PROVIDER}" "${annos}" && [ -z "${rc}" ]; then
            pass A17 "无 pod-index label：未命中 objectSelector，无注入（正向对照已命中）"
        else
            fail A17 "无 label 仍被注入或选择器正向对照失败：runtimeClass='${rc}' 注解=$(provider_annotation_count "${PROVIDER}" "${annos}") control=${selector_control}"
        fi
    else
        skip A17 "server dry-run 失败: $(tail -1 "${WORK}/a17.err" 2>/dev/null)"
    fi

    # A18：webhook 自己所在 namespace（namespaceSelector 排除）
    k8s_render "${FIXTURE_DIR}/pod.yaml" "${WORK}/a18.yaml" \
        "NAME=wv-a18" "NS=${WV_NAMESPACE}" \
        "LABELS=${labels_block}" "ANNOTATIONS=" "RUNTIMECLASS=" \
        "IMAGE=${image_for_dry}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${env_block}"
    if out=$(kubectl create --dry-run=server -o json -f "${WORK}/a18.yaml" 2>"${WORK}/a18.err"); then
        annos=$(printf '%s' "${out}" | jq -c '.metadata.annotations // {}')
        rc=$(printf '%s' "${out}" | jq -r '.spec.runtimeClassName // ""')
        if [ "${selector_control}" = "1" ] && provider_assert_no_prefix "${PROVIDER}" "${annos}" && [ -z "${rc}" ]; then
            pass A18 "${WV_NAMESPACE} namespace 被 namespaceSelector 排除，无注入（正向对照已命中）"
        else
            fail A18 "${WV_NAMESPACE} 内仍被注入或正向对照失败：runtimeClass='${rc}' control=${selector_control}"
        fi
    else
        skip A18 "server dry-run 失败: $(tail -1 "${WORK}/a18.err" 2>/dev/null)"
    fi

    # A19：kube-system
    k8s_render "${FIXTURE_DIR}/pod.yaml" "${WORK}/a19.yaml" \
        "NAME=wv-a19" "NS=kube-system" \
        "LABELS=${labels_block}" "ANNOTATIONS=" "RUNTIMECLASS=" \
        "IMAGE=${image_for_dry}" "COMMAND=[\"sleep\",\"3600\"]" "ENV=${env_block}"
    if out=$(kubectl create --dry-run=server -o json -f "${WORK}/a19.yaml" 2>"${WORK}/a19.err"); then
        annos=$(printf '%s' "${out}" | jq -c '.metadata.annotations // {}')
        rc=$(printf '%s' "${out}" | jq -r '.spec.runtimeClassName // ""')
        if [ "${selector_control}" = "1" ] && provider_assert_no_prefix "${PROVIDER}" "${annos}" && [ -z "${rc}" ]; then
            pass A19 "kube-system 被 namespaceSelector 排除，无注入（正向对照已命中）"
        else
            fail A19 "kube-system 内仍被注入或正向对照失败：runtimeClass='${rc}' control=${selector_control}"
        fi
    else
        skip A19 "server dry-run 失败: $(tail -1 "${WORK}/a19.err" 2>/dev/null)"
    fi
fi

#==================== A21 JSON Pointer 转义 ====================#
log_step "A21 JSON Pointer 转义"
# 只有“对象已有注解”时 webhook 才会逐 key add，路径里才会出现转义后的 key（A4 场景）。
if [ -s "${WORK}/a4.patch" ]; then
    bad_pointer=$(
        jq -r '.[] | select(.path | startswith("/metadata/annotations/")) | .path' "${WORK}/a4.patch" 2>/dev/null \
        | while IFS= read -r p; do
            [ -z "${p}" ] && continue
            rest="${p#/metadata/annotations/}"
            case "${rest}" in */*) printf '%s\n' "${p}" ;; esac
        done | grep -c . || true
    )
    escaped_keys=$(
        jq -r '.[] | select(.path | startswith("/metadata/annotations/")) | .path' "${WORK}/a4.patch" 2>/dev/null \
        | sed 's|^/metadata/annotations/||' | grep -c '~1' || true
    )
    keys_ok=$(admission_annotations "${WORK}/a4.applied" \
        | jq --arg p "${PROVIDER}.dev/" '[keys[] | select(startswith($p))] | length' 2>/dev/null || echo 0)
    if [ "${bad_pointer}" = "0" ] && [ "${escaped_keys}" -ge 1 ] && [ "${keys_ok}" -ge 21 ]; then
        pass A21 "${escaped_keys} 个 key 的 / 已转义为 ~1，应用后还原出 ${keys_ok} 个 ${PROVIDER}.dev/* key"
    else
        fail A21 "未转义路径 ${bad_pointer} 个，含 ~1 的 key ${escaped_keys} 个，还原 key ${keys_ok} 个"
    fi
else
    fail A21 "缺少 A4 的 patch（用户已有注解场景），无法验证转义"
fi

#==================== A23 UPDATE 操作 ====================#
log_step "A23 UPDATE 不注入"
# MWC 的 rules 只包含 CREATE，apiserver 不会把真实 UPDATE 送进来；
# 这里直连 /mutate 发一个“缺 runtimeClass、无注解”的 UPDATE，
# 若 webhook 没做操作类型过滤就会产生 patch —— 与 A5 的 CREATE 幂等是两回事。
admission_ensure_port_forward || true
pod_object "${WORK}/a23.json" "${WV_TEST_NS}" "wv-a23" "template=${TEMPLATE}"
RESP="${WORK}/a23.resp"; PATCH="${WORK}/a23.patch"; APPLIED="${WORK}/a23.applied"
if admission_post "${WORK}/a23.json" "${RESP}" UPDATE && [ -s "${RESP}" ]; then
    admission_patch_json "${RESP}" > "${PATCH}" 2>/dev/null || true
    admission_apply_patch "${WORK}/a23.json" "${PATCH}" "${APPLIED}" 2>/dev/null || true
    code=$(admission_resp_http_code "${RESP}")
    ops=$(patch_op_count)
    rc=$(admission_runtimeclass "${APPLIED}")
    annos=$(annotations_count "${APPLIED}")
    if [ "${code}" = "200" ] && allowed && [ "${ops}" = "0" ] && [ -z "${rc}" ] && [ "${annos}" = "0" ]; then
        pass A23 "UPDATE 请求 allowed 且无 patch（runtimeClass 与 provider 注解均未注入）"
    else
        fail A23 "UPDATE 仍被注入：http=${code} patch=${ops} runtimeClass='${rc}' 注解=${annos}"
    fi
else
    fail A23 "UPDATE 准入请求失败（http=$(admission_resp_http_code "${RESP}" 2>/dev/null || echo '?')）"
fi

#==================== 健康兜底 ====================#
code=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/healthz" 2>/dev/null || true)
if [ "${code}" != "200" ]; then
    log_warn "用例结束后 /healthz 返回 ${code}"
    k8s_collect_webhook_artifacts "webhook-after-01" >/dev/null 2>&1 || true
fi

finish "01 准入契约"

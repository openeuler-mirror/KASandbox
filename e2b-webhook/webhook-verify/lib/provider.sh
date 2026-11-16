#!/bin/bash
###############################################################################
# provider.sh — 后端差异的唯一落点
#
# 核心脚本只调用 provider_*；新增后端 = PROVIDER_TABLE 加一行 + 04 加分支。
# 这里同时承载“当前后端（e2b）的 API/注解映射”，因为要拿真实 transform
# 响应做期望值，无法抽象成完全后端无关的逻辑。
###############################################################################
set -euo pipefail

# name|runtimeClass|annotation_prefix|requires_api|e2e
PROVIDER_TABLE="${PROVIDER_TABLE:-
e2b|e2b|e2b.dev/|1|1
android|android||0|0
}"

provider_active() { printf '%s' "${WV_PROVIDER:-e2b}"; }

provider_list() {
    printf '%s\n' "${PROVIDER_TABLE}" | while IFS='|' read -r name _rest; do
        [ -z "${name}" ] && continue
        case "${name}" in \#*) continue ;; esac
        printf '%s\n' "${name}"
    done
}

provider_field() {
    local want="$1" field="$2"
    printf '%s\n' "${PROVIDER_TABLE}" | while IFS='|' read -r name rc prefix api e2e; do
        [ "${name}" = "${want}" ] || continue
        case "${field}" in
            runtimeclass) printf '%s' "${rc}" ;;
            prefix) printf '%s' "${prefix}" ;;
            requires_api) printf '%s' "${api:-0}" ;;
            e2e) printf '%s' "${e2e:-0}" ;;
        esac
        return 0
    done
}

provider_registered() {
    local name
    for name in $(provider_list); do
        [ "${name}" = "$1" ] && return 0
    done
    return 1
}

provider_by_runtimeclass() {
    local rc="$1" name
    for name in $(provider_list); do
        [ "$(provider_field "${name}" runtimeclass)" = "${rc}" ] && { printf '%s' "${name}"; return 0; }
    done
    return 1
}

# provider_default <android_sandbox_env_value>
provider_default() {
    local legacy_android="${1:-}"
    if is_true "${legacy_android}" && provider_registered android; then
        printf 'android'
    else
        printf 'e2b'
    fi
}

provider_annotation_prefix() { provider_field "$1" prefix; }
provider_requires_api() { [ "$(provider_field "$1" requires_api)" = "1" ]; }
provider_has_e2e() { [ "$(provider_field "$1" e2e)" = "1" ]; }
provider_runtimeclass() { provider_field "$1" runtimeclass; }

#==================== 注解断言 ====================#
# provider_annotations_json <provider> <annotations-json>：只保留该后端前缀的注解。
provider_annotations_json() {
    local provider="$1" annos="$2" prefix
    prefix=$(provider_annotation_prefix "${provider}")
    if [ -z "${prefix}" ]; then
        printf '{}'
        return 0
    fi
    printf '%s' "${annos}" | jq -c --arg p "${prefix}" \
        'with_entries(select(.key | startswith($p)))' 2>/dev/null || printf '{}'
}

provider_annotation_count() {
    provider_annotations_json "$1" "$2" | jq 'length'
}

# provider_assert_no_prefix <provider> <annotations-json>：无该后端注解返回 0。
provider_assert_no_prefix() {
    [ "$(provider_annotation_count "$1" "$2")" = "0" ]
}

provider_dynamic_keys() {
    case "$1" in
        e2b) printf 'e2b.dev/sandbox-id\ne2b.dev/execution-id' ;;
        *) printf '' ;;
    esac
}

#==================== API ====================#
provider_api_url() {
    if [ -n "${WV_API_URL:-}" ]; then
        printf '%s' "${WV_API_URL}"
        return 0
    fi
    if [ -n "${E2B_API_URL:-}" ]; then
        printf '%s' "${E2B_API_URL}"
        return 0
    fi
    local ip
    ip=$(kubectl -n "${WV_NAMESPACE}" get svc api -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
    if [ -n "${ip}" ] && [ "${ip}" != "None" ]; then
        printf 'http://%s:3000' "${ip}"
        return 0
    fi
    printf 'http://localhost:3000'
}

provider_api_health() {
    local url="$1"
    curl -sf -m 5 "${url%/}/health" -o /dev/null 2>/dev/null
}

# 返回 0 表示“当前后端所需的 API 可用”；不需要 API 的后端直接 0。
provider_api_available() {
    local provider="${1:-$(provider_active)}"
    provider_requires_api "${provider}" || return 0
    provider_api_health "$(provider_api_url)"
}

provider_transform_response() {
    local template="$1" url key
    url=$(provider_api_url)
    key=$(webhook_api_key 2>/dev/null || true)
    if [ -z "${template}" ]; then
        printf '{}'
        return 0
    fi
    curl -sS -m 10 -X POST "${url%/}/sandboxes/transform" \
        -H 'Content-Type: application/json' \
        ${key:+-H "X-API-Key: ${key}"} \
        --data-binary "$(jq -nc --arg t "${template}" '{templateName:$t}')" 2>/dev/null
}

#==================== 期望注解（e2b 映射的 jq 版） ====================#
# 与 e2b-webhook/providers/transform.go:toE2BAnnotations 一一对应：
#   - 15 个稳定键；
#   - allow-internet / envd-access-token / network / env-vars / volume-mounts / auto-resume 带默认值；
#   - metadata / sandbox-id 仅在非空时设置。
_e2b_expected_jq() {
    cat <<'JQ'
def present($s; $k): (($s | has($k)) and ($s[$k] != null));
def sorted_map($o): ($o | to_entries | sort_by(.key) | from_entries);
def clean_obj($o): (($o // {}) | with_entries(select(.value != null) | select((.value | type) != "array" or (.value | length) > 0)));

.sandbox as $s
| {
    "e2b.dev/base_template_id": (if present($s; "baseTemplateId") then ($s.baseTemplateId | tostring) else "" end),
    "e2b.dev/template-id": (if present($s; "templateId") then ($s.templateId | tostring) else "" end),
    "e2b.dev/build-id": (if present($s; "buildId") then ($s.buildId | tostring) else "" end),
    "e2b.dev/team-id": (if present($s; "teamId") then ($s.teamId | tostring) else "" end),
    "e2b.dev/vcpu": (if present($s; "vcpu") then ($s.vcpu | tostring) else "0" end),
    "e2b.dev/ram-mb": (if present($s; "ramMb") then ($s.ramMb | tostring) else "0" end),
    "e2b.dev/total-disk-size-mb": (if present($s; "totalDiskSizeMb") then ($s.totalDiskSizeMb | tostring) else "0" end),
    "e2b.dev/max-sandbox-length": (if present($s; "maxSandboxLength") then ($s.maxSandboxLength | tostring) else "0" end),
    "e2b.dev/huge-pages": (if present($s; "hugePages") then ($s.hugePages | tostring) else "false" end),
    "e2b.dev/auto-pause": (if present($s; "autoPause") then ($s.autoPause | tostring) else "false" end),
    "e2b.dev/snapshot": (if present($s; "snapshot") then ($s.snapshot | tostring) else "false" end),
    "e2b.dev/envd-version": (if present($s; "envdVersion") then ($s.envdVersion | tostring) else "" end),
    "e2b.dev/kernel-version": (if present($s; "kernelVersion") then ($s.kernelVersion | tostring) else "" end),
    "e2b.dev/firecracker-version": (if present($s; "firecrackerVersion") then ($s.firecrackerVersion | tostring) else "" end),
    "e2b.dev/execution-id": (if present($s; "executionId") then ($s.executionId | tostring) else "" end),
    "e2b.dev/allow-internet": (if present($s; "allowInternetAccess") then ($s.allowInternetAccess | tostring) else "true" end),
    "e2b.dev/envd-access-token": (if present($s; "envdAccessToken") then ($s.envdAccessToken | tostring) else "" end),
    "e2b.dev/network":
        (if present($s; "network")
         then ({egress: clean_obj($s.network.egress), ingress: clean_obj($s.network.ingress)} | tojson)
         else "{\"egress\":{},\"ingress\":{}}" end),
    "e2b.dev/env-vars":
        (if present($s; "envVars") and (($s.envVars | length) > 0)
         then (sorted_map($s.envVars) | tojson) else "{}" end),
    "e2b.dev/volume-mounts":
        (if present($s; "volumeMounts") and (($s.volumeMounts | length) > 0)
         then ($s.volumeMounts | map(clean_obj(.)) | tojson) else "[]" end),
    "e2b.dev/auto-resume":
        (if present($s; "autoResume") and (($s.autoResume.policy // "") != "")
         then ({policy: $s.autoResume.policy} | tojson) else "{\"policy\":\"off\"}" end)
    }
| if present($s; "metadata") and (($s.metadata | length) > 0)
  then . + {"e2b.dev/metadata": (sorted_map($s.metadata) | tojson)} else . end
| if present($s; "sandboxId") and (($s.sandboxId | tostring) != "")
  then . + {"e2b.dev/sandbox-id": ($s.sandboxId | tostring)} else . end
JQ
}

e2b_expected_annotations() {
    local template="$1" response
    response=$(provider_transform_response "${template}")
    if [ -z "${response}" ]; then
        return 1
    fi
    if ! printf '%s' "${response}" | jq -e '.sandbox' >/dev/null 2>&1; then
        log_warn "transform 响应异常: ${response:0:200}"
        return 1
    fi
    printf '%s' "${response}" | jq -c "$(_e2b_expected_jq)"
}

provider_expected_annotations() {
    case "$1" in
        e2b) e2b_expected_annotations "$2" ;;
        android) printf '{}' ;;
        *) printf '{}' ;;
    esac
}

#==================== 注解比对 ====================#
# provider_compare_annotations <provider> <expected.json> <actual-annotations.json>
# 输出差异行；无差异时输出为空并返回 0。
provider_compare_annotations() {
    local provider="$1" expected="$2" actual="$3" prefix dynamic
    prefix=$(provider_annotation_prefix "${provider}")
    dynamic=$(provider_dynamic_keys "${provider}" | jq -R -s -c 'split("\n") | map(select(length > 0))')

    jq -nr \
        --slurpfile e "${expected}" \
        --slurpfile a "${actual}" \
        --arg p "${prefix}" \
        --argjson dyn "${dynamic}" '
        def same($x; $y):
            ($x == $y)
            or ((try ($x | fromjson) catch null) as $xj
                | (try ($y | fromjson) catch null) as $yj
                | ($xj != null and $xj == $yj));
        $e[0] as $exp
        | ($a[0] | with_entries(select(.key | startswith($p)))) as $act
        | [
            ($exp | to_entries[] | .key as $k
                | select(($act | has($k)) | not)
                | "缺少 \($k)"),
            ($act | to_entries[] | .key as $k
                | select(($exp | has($k)) | not)
                | "多出 \($k)=\(.value)"),
            ($exp | to_entries[] | .key as $k | .value as $v
                | select(($dyn | index($k)) == null)
                | select(($act | has($k)) and (same($act[$k]; $v) | not))
                | "值不符 \($k): 期望 \($v) 实际 \($act[$k])")
        ] | .[]'
}

#==================== 模板 ====================#
# provider_template_image <alias>：解析成 cri 需要的 e2b.dev/<templateID>:<buildID>。
provider_template_image() {
    local alias="$1" url key response
    url=$(provider_api_url)
    key=$(webhook_api_key 2>/dev/null || true)
    response=$(curl -sS -m 10 "${url%/}/templates?limit=100" \
        ${key:+-H "X-API-Key: ${key}"} 2>/dev/null || true)
    [ -n "${response}" ] || return 1

    local template_id build_id
    template_id=$(printf '%s' "${response}" | jq -r --arg a "${alias}" \
        '[.[] | select(.aliases != null and (.aliases | index($a)))] | .[0].templateID // ""' 2>/dev/null || true)
    build_id=$(printf '%s' "${response}" | jq -r --arg a "${alias}" \
        '[.[] | select(.aliases != null and (.aliases | index($a)))] | .[0].buildID // ""' 2>/dev/null || true)
    if [ -z "${template_id}" ] || [ -z "${build_id}" ]; then
        return 1
    fi
    state_set TEMPLATE_ID "${template_id}"
    state_set BUILD_ID "${build_id}"
    state_set TEMPLATE_ALIAS "${alias}"
    printf 'e2b.dev/%s:%s' "${template_id}" "${build_id}"
}

provider_template_ready() {
    local alias="$1" url key
    url=$(provider_api_url)
    key=$(webhook_api_key 2>/dev/null || true)
    curl -sS -m 10 "${url%/}/templates?limit=100" ${key:+-H "X-API-Key: ${key}"} 2>/dev/null \
        | jq -e --arg a "${alias}" \
            '[.[] | select(.aliases != null and (.aliases | index($a)) and .buildStatus == "ready")] | length > 0' \
        >/dev/null 2>&1
}

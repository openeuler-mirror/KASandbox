#!/bin/bash
###############################################################################
# e2b_env.sh — E2B 环境变量、模板元数据与 Pod fixture 生成
#
# 该文件由 lib/common.sh source。目标：
#   1. 兼容历史 .env，同时自动纠正失联的 API URL / 过期 API key（优先使用
#      /root/.e2b/config.json 中当前有效的 teamApiKey）。
#   2. 从 E2B API 动态解析当前可用的 template/build/team 元数据，避免脚本中
#      固定写死已经失效的 build-id/template-id。
#   3. 统一生成 cri-multiplex 直连 Pod JSON 与 kubelet Pod YAML。
###############################################################################

E2B_VERIFY_DIR="${SCRIPT_DIR_COMMON:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
E2B_ENV_FILE="${E2B_ENV_FILE:-${E2B_VERIFY_DIR}/.env}"
E2B_CONFIG_JSON="${E2B_CONFIG_JSON:-/root/.e2b/config.json}"
E2B_API_URL="${E2B_API_URL:-http://127.0.0.1:3000}"
E2B_API_URL_FALLBACK="${E2B_API_URL_FALLBACK:-http://127.0.0.1:3000}"
E2B_API_KEY="${E2B_API_KEY:-}"
E2B_ACCESS_TOKEN="${E2B_ACCESS_TOKEN:-}"
E2B_TEAM_ID="${E2B_TEAM_ID:-}"
E2B_TEMPLATE_ALIAS="${E2B_TEMPLATE_ALIAS:-ubuntu-22-04-custom-1}"
E2B_TEMPLATE_ID="${E2B_TEMPLATE_ID:-}"
E2B_BUILD_ID="${E2B_BUILD_ID:-}"
E2B_ENVD_VERSION="${E2B_ENVD_VERSION:-0.5.3}"
E2B_KERNEL_VERSION="${E2B_KERNEL_VERSION:-vmlinux-6.1.158}"
E2B_FIRECRACKER_VERSION="${E2B_FIRECRACKER_VERSION:-v1.13.1}"
E2B_VCPU="${E2B_VCPU:-}"
E2B_RAM_MB="${E2B_RAM_MB:-}"
E2B_DISK_MB="${E2B_DISK_MB:-}"
E2B_MAX_SANDBOX_LENGTH="${E2B_MAX_SANDBOX_LENGTH:-10000}"
E2B_RESOLVED_API_URL=""
E2B_METADATA_RESOLVED=0
E2B_METADATA_LOADED=0

e2b_log() { echo -e "${CYAN}[E2B]${NC}   $*" >&2; }
e2b_warn() { echo -e "${YELLOW}[E2B]${NC}   $*" >&2; }

# 读取 .env；环境变量优先于文件（方便 CI/临时覆盖）。
load_e2b_env() {
    if [ "${E2B_METADATA_LOADED}" = "1" ]; then
        return 0
    fi
    E2B_METADATA_LOADED=1

    if [ -f "${E2B_ENV_FILE}" ]; then
        # shellcheck disable=SC1090
        set -a
        . "${E2B_ENV_FILE}"
        set +a
    fi

    # .env 中的凭证可能已经过期；config.json 是本机最近一次部署写入的 live 凭证。
    if [ -f "${E2B_CONFIG_JSON}" ] && command -v jq >/dev/null 2>&1; then
        local cfg_key cfg_token cfg_team
        cfg_key=$(jq -r '.teamApiKey // empty' "${E2B_CONFIG_JSON}" 2>/dev/null || true)
        cfg_token=$(jq -r '.accessToken // empty' "${E2B_CONFIG_JSON}" 2>/dev/null || true)
        cfg_team=$(jq -r '.teamId // empty' "${E2B_CONFIG_JSON}" 2>/dev/null || true)
        if [ -n "${cfg_key}" ]; then
            E2B_API_KEY="${cfg_key}"
        fi
        if [ -n "${cfg_token}" ]; then
            E2B_ACCESS_TOKEN="${cfg_token}"
        fi
        if [ -n "${cfg_team}" ] && [ -z "${E2B_TEAM_ID}" ]; then
            E2B_TEAM_ID="${cfg_team}"
        fi
    fi

    export E2B_API_URL E2B_API_KEY E2B_ACCESS_TOKEN E2B_TEAM_ID \
        E2B_TEMPLATE_ALIAS E2B_TEMPLATE_ID E2B_BUILD_ID \
        E2B_ENVD_VERSION E2B_KERNEL_VERSION E2B_FIRECRACKER_VERSION \
        E2B_VCPU E2B_RAM_MB E2B_DISK_MB E2B_MAX_SANDBOX_LENGTH
}

_resolve_api_url() {
    if [ -n "${E2B_RESOLVED_API_URL}" ]; then
        echo "${E2B_RESOLVED_API_URL}"
        return 0
    fi

    load_e2b_env
    local url="${E2B_API_URL%/}"
    local code
    code=$(curl -sS -m 3 -o /dev/null -w '%{http_code}' "${url}/health" 2>/dev/null || true)
    if [ "${code}" = "200" ]; then
        E2B_RESOLVED_API_URL="${url}"
    else
        local fallback="${E2B_API_URL_FALLBACK%/}"
        code=$(curl -sS -m 3 -o /dev/null -w '%{http_code}' "${fallback}/health" 2>/dev/null || true)
        if [ "${code}" = "200" ]; then
            e2b_warn "E2B_API_URL=${url} 不可达，改用 ${fallback}"
            E2B_RESOLVED_API_URL="${fallback}"
        else
            E2B_RESOLVED_API_URL="${url}"
        fi
    fi
    echo "${E2B_RESOLVED_API_URL}"
}

# e2b_api <GET|POST|DELETE> <path> [json_body]
# stdout 仅输出 response body；HTTP 状态码非 2xx 时返回非 0。
e2b_api() {
    local method="$1"
    local path="$2"
    local body="${3:-}"
    load_e2b_env

    local url
    url="$(_resolve_api_url)"
    local tmp code
    tmp=$(mktemp /tmp/e2b-api.XXXXXX)
    local args=(-sS -m 20 -X "${method}" "${url}${path}" -H "X-API-Key: ${E2B_API_KEY}")
    if [ -n "${body}" ]; then
        args+=(-H 'Content-Type: application/json' -d "${body}")
    fi
    code=$(curl "${args[@]}" -o "${tmp}" -w '%{http_code}' 2>/dev/null || true)

    if [ "${code}" = "401" ] || [ "${code}" = "403" ]; then
        # .env key 失效时 reload config.json 再试一次
        E2B_METADATA_LOADED=0
        load_e2b_env
        args=(-sS -m 20 -X "${method}" "${url}${path}" -H "X-API-Key: ${E2B_API_KEY}")
        if [ -n "${body}" ]; then
            args+=(-H 'Content-Type: application/json' -d "${body}")
        fi
        code=$(curl "${args[@]}" -o "${tmp}" -w '%{http_code}' 2>/dev/null || true)
    fi

    cat "${tmp}" 2>/dev/null || true
    rm -f "${tmp}"
    case "${code}" in
        2??) return 0 ;;
        *) return 1 ;;
    esac
}

# resolve_e2b_template [--force]
# 解析出 E2B_TEMPLATE_ID / E2B_BUILD_ID / E2B_TEAM_ID / 资源规格。
resolve_e2b_template() {
    local force=0
    [ "${1:-}" = "--force" ] && force=1
    load_e2b_env

    if [ "${E2B_METADATA_RESOLVED}" = "1" ] && [ "${force}" = "0" ] &&
       [ -n "${E2B_TEMPLATE_ID}" ] && [ -n "${E2B_BUILD_ID}" ] && [ -n "${E2B_TEAM_ID}" ]; then
        return 0
    fi

    if [ "${force}" = "1" ]; then
        E2B_TEMPLATE_ID=""
        E2B_BUILD_ID=""
        E2B_METADATA_RESOLVED=0
    fi

    if [ -z "${E2B_TEMPLATE_ID}" ] || [ -z "${E2B_BUILD_ID}" ]; then
        local templates selected
        if ! templates=$(e2b_api GET "/templates"); then
            e2b_warn "无法从 E2B API 读取模板列表（API 不可达或凭证无效）"
            return 1
        fi
        if ! command -v jq >/dev/null 2>&1; then
            e2b_warn "jq 不可用，无法解析模板元数据"
            return 1
        fi
        selected=$(echo "${templates}" | jq -c --arg alias "${E2B_TEMPLATE_ALIAS}" '
            (map(select((.buildStatus // "") == "ready" and ((.aliases // []) | index($alias) != null))) | .[0])
            // (map(select((.buildStatus // "") == "ready")) | .[0])
            // empty' 2>/dev/null || true)
        if [ -z "${selected}" ] || [ "${selected}" = "null" ]; then
            e2b_warn "未找到可用的 E2B 模板（alias=${E2B_TEMPLATE_ALIAS}）"
            return 1
        fi

        E2B_TEMPLATE_ID=$(echo "${selected}" | jq -r '.templateID // .templateId // empty')
        E2B_BUILD_ID=$(echo "${selected}" | jq -r '.buildID // .buildId // empty')
        local selected_alias
        selected_alias=$(echo "${selected}" | jq -r '.aliases[0] // empty')
        [ -n "${selected_alias}" ] || selected_alias="${E2B_TEMPLATE_ALIAS}"
        E2B_TEMPLATE_ALIAS="${selected_alias}"
        E2B_VCPU=$(echo "${selected}" | jq -r '.cpuCount // empty')
        E2B_RAM_MB=$(echo "${selected}" | jq -r '.memoryMB // empty')
        E2B_DISK_MB=$(echo "${selected}" | jq -r '.diskSizeMB // empty')
        E2B_ENVD_VERSION=$(echo "${selected}" | jq -r '.envdVersion // empty' | sed 's/^$/0.5.3/')
    fi

    if [ -z "${E2B_TEAM_ID}" ] && [ -f "${E2B_CONFIG_JSON}" ]; then
        E2B_TEAM_ID=$(jq -r '.teamId // empty' "${E2B_CONFIG_JSON}" 2>/dev/null || true)
    fi

    if [ -z "${E2B_TEMPLATE_ID}" ] || [ -z "${E2B_BUILD_ID}" ] || [ -z "${E2B_TEAM_ID}" ]; then
        e2b_warn "E2B 元数据不完整: template=${E2B_TEMPLATE_ID:-<empty>} build=${E2B_BUILD_ID:-<empty>} team=${E2B_TEAM_ID:-<empty>}"
        return 1
    fi

    E2B_VCPU="${E2B_VCPU:-2}"
    E2B_RAM_MB="${E2B_RAM_MB:-2048}"
    E2B_DISK_MB="${E2B_DISK_MB:-943}"
    E2B_ENVD_VERSION="${E2B_ENVD_VERSION:-0.5.3}"
    E2B_METADATA_RESOLVED=1
    export E2B_TEMPLATE_ID E2B_BUILD_ID E2B_TEAM_ID E2B_TEMPLATE_ALIAS \
        E2B_VCPU E2B_RAM_MB E2B_DISK_MB E2B_ENVD_VERSION

    e2b_log "模板: alias=${E2B_TEMPLATE_ALIAS} template=${E2B_TEMPLATE_ID} build=${E2B_BUILD_ID}"
    return 0
}

_new_execution_id() {
    if [ -r /proc/sys/kernel/random/uuid ]; then
        cat /proc/sys/kernel/random/uuid
    elif command -v uuidgen >/dev/null 2>&1; then
        uuidgen
    else
        python3 -c 'import uuid; print(uuid.uuid4())'
    fi
}

_new_envd_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    else
        python3 -c 'import secrets; print(secrets.token_hex(32))'
    fi
}

# write_default_pod_json [uid] [name] [out_json]
# 用当前模板元数据生成 cri-multiplex 直连 Pod JSON（execution-id/token 每次随机）。
write_default_pod_json() {
    local uid="${1:-${POD_UID}}"
    local name="${2:-test-default-pod}"
    local out="${3:-${POD_JSON}}"
    local exec_id token

    resolve_e2b_template || return 1
    exec_id="$(_new_execution_id)"
    token="$(_new_envd_token)"

    command -v jq >/dev/null 2>&1 || { e2b_warn "jq 不可用，无法生成 Pod JSON"; return 1; }

    jq -n \
        --arg name "${name}" \
        --arg uid "${uid}" \
        --arg base "${E2B_TEMPLATE_ID}" \
        --arg template "${E2B_TEMPLATE_ID}" \
        --arg build "${E2B_BUILD_ID}" \
        --arg team "${E2B_TEAM_ID}" \
        --arg vcpu "${E2B_VCPU}" \
        --arg ram "${E2B_RAM_MB}" \
        --arg disk "${E2B_DISK_MB}" \
        --arg maxlen "${E2B_MAX_SANDBOX_LENGTH}" \
        --arg envd "${E2B_ENVD_VERSION}" \
        --arg kernel "${E2B_KERNEL_VERSION}" \
        --arg fc "${E2B_FIRECRACKER_VERSION}" \
        --arg execid "${exec_id}" \
        --arg token "${token}" \
        '{
          metadata: {name: $name, namespace: "default", uid: $uid},
          annotations: {
            "e2b.dev/base_template_id": $base,
            "e2b.dev/template-id": $template,
            "e2b.dev/build-id": $build,
            "e2b.dev/team-id": $team,
            "e2b.dev/vcpu": $vcpu,
            "e2b.dev/ram-mb": $ram,
            "e2b.dev/total-disk-size-mb": $disk,
            "e2b.dev/max-sandbox-length": $maxlen,
            "e2b.dev/huge-pages": "true",
            "e2b.dev/auto-pause": "false",
            "e2b.dev/snapshot": "false",
            "e2b.dev/allow-internet": "true",
            "e2b.dev/envd-version": $envd,
            "e2b.dev/kernel-version": $kernel,
            "e2b.dev/firecracker-version": $fc,
            "e2b.dev/execution-id": $execid,
            "e2b.dev/envd-access-token": $token,
            "e2b.dev/env-vars": "{}",
            "e2b.dev/network": "{\"egress\":{},\"ingress\":{}}",
            "e2b.dev/volume-mounts": "[]",
            "e2b.dev/auto-resume": "{\"policy\":\"off\"}"
          },
          labels: {app: "test"},
          log_directory: "/tmp",
          linux: {security_context: {namespace_options: {network: 2}}}
        }' > "${out}" || return 1

    export POD_UID="${uid}" CONTAINER_ID="${uid}-c"
    return 0
}

# ensure_default_pod_json
# 保持 POD_JSON 可复用，但确保 template/build/team 是当前 live 值。
ensure_default_pod_json() {
    resolve_e2b_template || return 1
    if [ ! -f "${POD_JSON}" ]; then
        write_default_pod_json "${POD_UID}" "test-default-pod" "${POD_JSON}" || return 1
        return 0
    fi
    if [ "${E2B_POD_JSON_REFRESH:-0}" = "1" ]; then
        write_default_pod_json "${POD_UID}" "test-default-pod" "${POD_JSON}" || return 1
        return 0
    fi
    if ! command -v jq >/dev/null 2>&1; then
        return 0
    fi
    local exec_id token
    exec_id=$(jq -r '.annotations["e2b.dev/execution-id"] // empty' "${POD_JSON}" 2>/dev/null || true)
    token=$(jq -r '.annotations["e2b.dev/envd-access-token"] // empty' "${POD_JSON}" 2>/dev/null || true)
    if [ -z "${exec_id}" ] || [ -z "${token}" ]; then
        write_default_pod_json "${POD_UID}" "test-default-pod" "${POD_JSON}" || return 1
        return 0
    fi

    local tmp="${POD_JSON}.refresh-tmp"
    if jq \
        --arg base "${E2B_TEMPLATE_ID}" \
        --arg template "${E2B_TEMPLATE_ID}" \
        --arg build "${E2B_BUILD_ID}" \
        --arg team "${E2B_TEAM_ID}" \
        --arg vcpu "${E2B_VCPU}" \
        --arg ram "${E2B_RAM_MB}" \
        --arg disk "${E2B_DISK_MB}" \
        --arg envd "${E2B_ENVD_VERSION}" \
        '.annotations["e2b.dev/base_template_id"] = $base
         | .annotations["e2b.dev/template-id"] = $template
         | .annotations["e2b.dev/build-id"] = $build
         | .annotations["e2b.dev/team-id"] = $team
         | .annotations["e2b.dev/vcpu"] = $vcpu
         | .annotations["e2b.dev/ram-mb"] = $ram
         | .annotations["e2b.dev/total-disk-size-mb"] = $disk
         | .annotations["e2b.dev/envd-version"] = $envd' \
        "${POD_JSON}" > "${tmp}" 2>/dev/null; then
        mv "${tmp}" "${POD_JSON}"
    else
        rm -f "${tmp}" 2>/dev/null || true
    fi
    return 0
}

# write_e2b_pod_yaml <pod_name> <out_yaml>
write_e2b_pod_yaml() {
    local pod_name="$1"
    local out="$2"
    local exec_id token

    resolve_e2b_template || return 1
    exec_id="$(_new_execution_id)"
    token="$(_new_envd_token)"

    cat > "${out}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  annotations:
    e2b.dev/base_template_id: "${E2B_TEMPLATE_ID}"
    e2b.dev/template-id: "${E2B_TEMPLATE_ID}"
    e2b.dev/build-id: "${E2B_BUILD_ID}"
    e2b.dev/team-id: "${E2B_TEAM_ID}"
    e2b.dev/vcpu: "${E2B_VCPU}"
    e2b.dev/ram-mb: "${E2B_RAM_MB}"
    e2b.dev/total-disk-size-mb: "${E2B_DISK_MB}"
    e2b.dev/max-sandbox-length: "${E2B_MAX_SANDBOX_LENGTH}"
    e2b.dev/huge-pages: "true"
    e2b.dev/auto-pause: "false"
    e2b.dev/snapshot: "false"
    e2b.dev/allow-internet: "true"
    e2b.dev/envd-version: "${E2B_ENVD_VERSION}"
    e2b.dev/kernel-version: "${E2B_KERNEL_VERSION}"
    e2b.dev/firecracker-version: "${E2B_FIRECRACKER_VERSION}"
    e2b.dev/execution-id: "${exec_id}"
    e2b.dev/envd-access-token: "${token}"
    e2b.dev/env-vars: "{}"
    e2b.dev/network: "{\"egress\":{},\"ingress\":{}}"
    e2b.dev/volume-mounts: "[]"
    e2b.dev/auto-resume: "{\"policy\":\"off\"}"
spec:
  runtimeClassName: e2b
  restartPolicy: Never
  containers:
  - name: app
    image: e2b.dev/${E2B_TEMPLATE_ID}:${E2B_BUILD_ID}
    imagePullPolicy: IfNotPresent
    command: ["sleep", "3600"]
EOF
    return 0
}

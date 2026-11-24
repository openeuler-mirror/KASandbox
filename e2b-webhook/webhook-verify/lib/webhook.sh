#!/bin/bash
###############################################################################
# webhook.sh — e2b-webhook 的 Deployment/Service/MWC/Secret 操作
#
# 覆盖：现状探测、基线保存、镜像构建/推送/替换、env 注入与恢复、回滚。
# 这里集中出现“当前实例”的具体实现（Deployment 名、Harbor、deploy.sh），
# 上层脚本只调用函数，不直接拼 kubectl 资源名。
###############################################################################
set -euo pipefail

#==================== 镜像仓库 ====================#
# 解析顺序：显式 REGISTRY_URL → deploy/.env → 集群里正在运行的镜像前缀。
webhook_registry_url() {
    if [ -n "${REGISTRY_URL:-}" ]; then
        printf '%s' "${REGISTRY_URL}"
        return 0
    fi

    local env_file="${REPO_ROOT}/deploy/.env"
    if [ -f "${env_file}" ]; then
        local server_ip harbor_port project
        server_ip=$(grep -E '^export SERVER_IP=' "${env_file}" | sed -n '1p' | cut -d'"' -f2)
        harbor_port=$(grep -E '^export HARBOR_HTTPS_PORT=' "${env_file}" | sed -n '1p' | cut -d'=' -f2- | tr -d '"')
        project=$(grep -E '^export REGISTRY_PROJECT=' "${env_file}" | sed -n '1p' | cut -d'=' -f2- | tr -d '"')
        if [ -n "${server_ip}" ] && [ -n "${project}" ]; then
            printf '%s:%s/%s' "${server_ip}" "${harbor_port:-30443}" "${project}"
            return 0
        fi
    fi

    # 兜底：从已运行组件反推（api/edge 的镜像前缀就是 Harbor 地址）。
    local image
    for image in $(kubectl -n "${WV_NAMESPACE}" get deploy -o jsonpath='{range .items[*]}{.spec.template.spec.containers[0].image}{"\n"}{end}' 2>/dev/null); do
        case "${image}" in
            */*)
                printf '%s' "${image%/*}"
                return 0
                ;;
        esac
    done
    return 1
}

webhook_image_repo() {
    if [ -n "${WV_IMAGE_REPO}" ]; then
        printf '%s' "${WV_IMAGE_REPO}"
        return 0
    fi
    local registry
    registry=$(webhook_registry_url 2>/dev/null || true)
    if [ -z "${registry}" ]; then
        return 1
    fi
    printf '%s/e2b-webhook' "${registry}"
}

webhook_image_ref() {
    if [ -z "${IMAGE_REF}" ]; then
        local repo
        repo=$(webhook_image_repo) || return 1
        IMAGE_REF="${repo}:${WV_IMAGE_TAG}"
    fi
    printf '%s' "${IMAGE_REF}"
}

# 构建并推送后的镜像摘要（形如 sha256:...）。
# 必须用 RepoDigests（registry manifest digest）：Pod 的 imageID 就是这个值，
# 而本地 image ID 是 config digest，两者不同。RepoDigests 只在 push 之后才有。
webhook_built_image_digest() {
    local ref digest
    ref=$(webhook_image_ref) || return 1
    need_cmd docker || return 1
    digest=$(docker image inspect "${ref}" -f '{{range .RepoDigests}}{{println .}}{{end}}' 2>/dev/null \
        | grep -m1 '@sha256:' | sed 's/.*@//' || true)
    [ -n "${digest}" ] || return 1
    printf '%s' "${digest}"
}

#==================== 现状探测 ====================#
# 把“集群里现在是什么状态”一次性落进 state.env；存在与否都不报错。
webhook_detect() {
    local ns="${WV_NAMESPACE}" name="${WV_DEPLOYMENT}"

    state_set FRESH_DEPLOY 0
    state_set DEPLOY_EXISTS 0
    state_set SVC_EXISTS 0
    state_set MWC_EXISTS 0
    state_set TLS_SECRET_EXISTS 0
    state_set APIKEY_SECRET_EXISTS 0

    if kubectl -n "${ns}" get deploy "${name}" >/dev/null 2>&1; then
        state_set DEPLOY_EXISTS 1
        state_set DEPLOY_IMAGE "$(kubectl -n "${ns}" get deploy "${name}" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)"
        state_set DEPLOY_REPLICAS "$(kubectl -n "${ns}" get deploy "${name}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0)"
        state_set DEPLOY_READY "$(kubectl -n "${ns}" get deploy "${name}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)"
        state_set STOCK_API_URL "$(kubectl -n "${ns}" get deploy "${name}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="E2B_API_URL")].value}' 2>/dev/null || true)"
        state_set STOCK_API_TIMEOUT "$(kubectl -n "${ns}" get deploy "${name}" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="E2B_API_TIMEOUT")].value}' 2>/dev/null || true)"
        state_set STOCK_IMAGE_ID "$(webhook_first_image_id 2>/dev/null || true)"
    fi

    kubectl -n "${ns}" get svc "${WV_SERVICE}" >/dev/null 2>&1 && state_set SVC_EXISTS 1
    kubectl get mutatingwebhookconfiguration "${WV_MWC}" >/dev/null 2>&1 && state_set MWC_EXISTS 1
    kubectl -n "${ns}" get secret "${WV_TLS_SECRET}" >/dev/null 2>&1 && state_set TLS_SECRET_EXISTS 1
    kubectl -n "${ns}" get secret "${WV_APIKEY_SECRET}" >/dev/null 2>&1 && state_set APIKEY_SECRET_EXISTS 1

    state_set ENDPOINT_COUNT "$(webhook_endpoint_count 2>/dev/null || echo 0)"
}

webhook_is_fresh() { [ "$(state_get FRESH_DEPLOY)" = "1" ]; }

webhook_pods() {
    # 排除正在删除的 Pod：滚动更新刚结束时旧 Pod 还在 Terminating，
    # 把它们算进来会让镜像核对误判成“仍有旧镜像”。
    kubectl -n "${WV_NAMESPACE}" get pods -l "app.kubernetes.io/name=${WV_DEPLOYMENT}" -o json 2>/dev/null \
        | jq -r '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name' 2>/dev/null || true
}

webhook_pod_count() {
    webhook_pods | grep -c . || true
}

webhook_first_image_id() {
    kubectl -n "${WV_NAMESPACE}" get pods -l "app.kubernetes.io/name=${WV_DEPLOYMENT}" -o json 2>/dev/null \
        | jq -r '[.items[] | select(.metadata.deletionTimestamp == null)][0].status.containerStatuses[0].imageID // ""' 2>/dev/null || true
}

webhook_pod_images() {
    kubectl -n "${WV_NAMESPACE}" get pods -l "app.kubernetes.io/name=${WV_DEPLOYMENT}" -o json 2>/dev/null \
        | jq -r '.items[] | select(.metadata.deletionTimestamp == null)
            | "\(.metadata.name)|\(.spec.containers[0].image)|\(.status.containerStatuses[0].imageID // "")"' 2>/dev/null || true
}

webhook_ready_replicas() {
    local ready
    ready=$(kubectl -n "${WV_NAMESPACE}" get deploy "${WV_DEPLOYMENT}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    printf '%s' "${ready:-0}"
}

webhook_desired_replicas() {
    kubectl -n "${WV_NAMESPACE}" get deploy "${WV_DEPLOYMENT}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo 0
}

webhook_endpoint_count() {
    kubectl -n "${WV_NAMESPACE}" get endpoints "${WV_SERVICE}" \
        -o jsonpath='{range .subsets[*].addresses[*]}{.ip}{"\n"}{end}' 2>/dev/null | grep -c . || true
}

webhook_available_replicas() {
    kubectl -n "${WV_NAMESPACE}" get deploy "${WV_DEPLOYMENT}" -o jsonpath='{.status.availableReplicas}' 2>/dev/null || echo 0
}

#==================== 基线 ====================#
webhook_backup_env_file() {
    local src="${REPO_ROOT}/deploy/.env"
    local dst="${BASELINE_DIR}/deploy.env.bak"
    if [ -f "${src}" ]; then
        cp -f "${src}" "${dst}"
        state_set BASELINE_ENV "${dst}"
        return 0
    fi
    return 1
}

webhook_env_file_force() {
    # webhook_env_file_force KEY VALUE：改 deploy/.env 中 `export KEY=...`。
    # 调用前必须已经 webhook_backup_env_file（05 的 Z3 负责还原）。
    local key="$1" value="$2"
    local file="${REPO_ROOT}/deploy/.env"
    [ -f "${file}" ] || return 1
    if grep -qE "^export ${key}=" "${file}"; then
        sed -i -E "s|^export ${key}=.*|export ${key}=\"${value}\"|" "${file}"
    else
        printf 'export %s="%s"\n' "${key}" "${value}" >> "${file}"
    fi
}

webhook_restore_env_file() {
    local bak
    bak=$(state_get BASELINE_ENV)
    [ -n "${bak}" ] && [ -f "${bak}" ] && cp -f "${bak}" "${REPO_ROOT}/deploy/.env"
}

webhook_save_baseline() {
    local old_image old_replicas image_id
    old_image=$(state_get DEPLOY_IMAGE)
    old_replicas=$(state_get DEPLOY_REPLICAS)
    image_id=$(state_get STOCK_IMAGE_ID)

    printf '%s\n' "${old_image}" > "${BASELINE_DIR}/old_image"
    printf '%s\n' "${old_replicas}" > "${BASELINE_DIR}/old_replicas"
    printf '%s\n' "${image_id}" > "${BASELINE_DIR}/old_image_id"
    webhook_cabundle_sha256 0 > "${BASELINE_DIR}/cabundle.sha256" 2>/dev/null || true
    webhook_backup_env_file || true

    state_set BASELINE_OLD_IMAGE "${old_image}"
    state_set BASELINE_OLD_REPLICAS "${old_replicas}"
    state_set BASELINE_OLD_IMAGE_ID "${image_id}"
    state_set BASELINE_CABUNDLE_SHA "$(cat "${BASELINE_DIR}/cabundle.sha256" 2>/dev/null || true)"
}

webhook_baseline_ok() {
    [ -f "${BASELINE_DIR}/old_image" ] \
        && [ -f "${BASELINE_DIR}/old_replicas" ] \
        && [ -f "${BASELINE_DIR}/deploy.env.bak" ]
}

#==================== 证书 ====================#
webhook_cert_sha256() {
    kubectl -n "${WV_NAMESPACE}" get secret "${WV_TLS_SECRET}" -o jsonpath='{.data.tls\.crt}' 2>/dev/null \
        | base64 -d 2>/dev/null | sha256sum | awk '{print $1}'
}

webhook_cabundle_sha256() {
    local index="${1:-0}"
    kubectl get mutatingwebhookconfiguration "${WV_MWC}" \
        -o jsonpath="{.webhooks[${index}].clientConfig.caBundle}" 2>/dev/null \
        | base64 -d 2>/dev/null | sha256sum | awk '{print $1}'
}

webhook_mwc_webhook_count() {
    kubectl get mutatingwebhookconfiguration "${WV_MWC}" -o jsonpath='{.webhooks}' 2>/dev/null \
        | jq 'length' 2>/dev/null || echo 0
}

# 服务端实际提供的证书摘要（经 port-forward 做一次真实 TLS 握手）。
# 只比对 Secret 与 MWC 的 caBundle 不能证明 Pod 真的在提供这张证书。
webhook_served_cert_sha256() {
    need_cmd openssl || return 1
    local pem
    pem=$(run_with_timeout 10 openssl s_client \
        -connect "127.0.0.1:${WV_PORT}" \
        -servername "${WV_SERVICE}.${WV_NAMESPACE}.svc" </dev/null 2>/dev/null \
        | openssl x509 -outform PEM 2>/dev/null)
    [ -n "${pem}" ] || return 1
    printf '%s\n' "${pem}" | sha256sum | awk '{print $1}'
}

# 所有 webhook Pod 的日志：kubectl logs deploy/... 只会挑一个 Pod，
# 做“密钥未落日志”这类断言时必须逐个 Pod 取。
webhook_logs_all() {
    local tail="${1:-5000}" pod
    for pod in $(webhook_pods); do
        kubectl -n "${WV_NAMESPACE}" logs "${pod}" --all-containers=true --tail="${tail}" 2>/dev/null || true
    done
}

#==================== 镜像 ====================#
webhook_goarch() {
    case "$(uname -m)" in
        x86_64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *) uname -m ;;
    esac
}

webhook_build_image() {
    local arch bin_dir log_file
    arch=$(webhook_goarch)
    bin_dir="${REPO_ROOT}/bin/${arch}"
    log_file="${LOG_DIR}/build-image.log"

    # build.sh 的 ensure_webhook_binary 命中已有二进制就跳过编译，必须先删。
    rm -f "${bin_dir}/e2b-webhook"

    if ! ( cd "${REPO_ROOT}" && ./build.sh -i e2b-webhook ) >"${log_file}" 2>&1; then
        log_error "镜像构建失败，日志: ${log_file}"
        tail -30 "${log_file}" >&2 || true
        return 1
    fi
    if ! docker image inspect e2b-webhook:latest >/dev/null 2>&1; then
        log_error "构建结束但本地没有 e2b-webhook:latest 镜像"
        return 1
    fi
    # 编译产物必须存在，否则镜像里躺的是旧二进制。
    if [ ! -f "${bin_dir}/e2b-webhook" ]; then
        log_error "缺少编译产物 ${bin_dir}/e2b-webhook"
        return 1
    fi

    local ref
    ref=$(webhook_image_ref) || return 1
    docker tag e2b-webhook:latest "${ref}" || return 1
    docker image inspect "${ref}" -f '{{.Id}}' > "${BASELINE_DIR}/built_image_id" 2>/dev/null || true
    state_set BUILT_IMAGE_ID "$(cat "${BASELINE_DIR}/built_image_id" 2>/dev/null || true)"
    state_set IMAGE_REF "${ref}"
    state_set IMAGE_TAG "${WV_IMAGE_TAG}"
    IMAGE_REF="${ref}"
    printf '%s' "${ref}"
}

webhook_push_image() {
    local ref log_file="${LOG_DIR}/push-image.log"
    ref=$(webhook_image_ref) || return 1

    if docker push "${ref}" >"${log_file}" 2>&1; then
        return 0
    fi

    # Harbor 会话过期时补一次 login 再重试（凭据来自 deploy/.env）。
    local env_file="${REPO_ROOT}/deploy/.env" host user pass
    if [ -f "${env_file}" ]; then
        host=$(webhook_registry_url 2>/dev/null | cut -d'/' -f1 || true)
        user=$(grep -E '^export HARBOR_USERNAME=' "${env_file}" | sed -n '1p' | cut -d'=' -f2- | tr -d '"')
        pass=$(grep -E '^export HARBOR_PASSWORD=' "${env_file}" | sed -n '1p' | cut -d'=' -f2- | tr -d '"')
        if [ -n "${host}" ] && [ -n "${user}" ] && [ -n "${pass}" ]; then
            printf '%s' "${pass}" | docker login "${host}" -u "${user}" --password-stdin >>"${log_file}" 2>&1 || true
            if docker push "${ref}" >>"${log_file}" 2>&1; then
                return 0
            fi
        fi
    fi
    log_error "镜像推送失败，日志: ${log_file}"
    tail -10 "${log_file}" >&2 || true
    return 1
}

webhook_set_image() {
    local ref="${1:-$(webhook_image_ref)}"
    kubectl -n "${WV_NAMESPACE}" set image "deploy/${WV_DEPLOYMENT}" "${WV_CONTAINER}=${ref}"
}

webhook_rollout_status() {
    local timeout="${1:-300}"
    kubectl -n "${WV_NAMESPACE}" rollout status "deploy/${WV_DEPLOYMENT}" --timeout="${timeout}s" >/dev/null 2>&1
}

webhook_restart() {
    kubectl -n "${WV_NAMESPACE}" rollout restart "deploy/${WV_DEPLOYMENT}" >/dev/null 2>&1
}

webhook_restore_image() {
    local old_image
    old_image=$(state_get BASELINE_OLD_IMAGE)
    [ -n "${old_image}" ] || old_image=$(cat "${BASELINE_DIR}/old_image" 2>/dev/null || true)
    [ -n "${old_image}" ] || return 1
    webhook_set_image "${old_image}" && webhook_rollout_status 300
}

webhook_restore_replicas() {
    local old_replicas
    old_replicas=$(state_get BASELINE_OLD_REPLICAS)
    [ -n "${old_replicas}" ] || old_replicas=$(cat "${BASELINE_DIR}/old_replicas" 2>/dev/null || true)
    [ -n "${old_replicas}" ] || return 1
    kubectl -n "${WV_NAMESPACE}" scale "deploy/${WV_DEPLOYMENT}" --replicas="${old_replicas}" >/dev/null 2>&1 || return 1
    kubectl -n "${WV_NAMESPACE}" rollout status "deploy/${WV_DEPLOYMENT}" --timeout=180s >/dev/null 2>&1 || true
}

#==================== 首次部署 ====================#
# 仓库里的 helm chart 目录：deploy/helm 优先，缺失时用仓库根目录的 helm/。
webhook_chart_dir() {
    if [ -d "${REPO_ROOT}/deploy/helm/templates" ]; then
        printf '%s' "${REPO_ROOT}/deploy/helm"
        return 0
    fi
    if [ -d "${REPO_ROOT}/helm/templates" ]; then
        printf '%s' "${REPO_ROOT}/helm"
        return 0
    fi
    return 1
}

# 判断 deploy.sh deploy-webhook 在本仓库是否真的可用：
# 可用时输出 "deploy.sh"；不可用时输出不可用原因（调用方据此走等价部署）。
webhook_deploy_method() {
    local env_file="${REPO_ROOT}/deploy/.env" server_ip

    if [ ! -f "${REPO_ROOT}/deploy/deploy.sh" ]; then
        printf 'deploy/deploy.sh 不存在'
        return 0
    fi
    if [ ! -d "${REPO_ROOT}/deploy/helm/templates" ]; then
        printf 'deploy/helm 不存在（chart 实际在仓库根目录 helm/）'
        return 0
    fi
    if [ ! -f "${env_file}" ]; then
        printf 'deploy/.env 不存在'
        return 0
    fi
    # deploy.sh 用 SERVER_IP 拼 REGISTRY_URL，为空时会拼出 ":30443/..." 这种非法镜像名。
    server_ip=$(grep -E '^export SERVER_IP=' "${env_file}" 2>/dev/null | sed -n '1p' | cut -d'"' -f2 || true)
    if [ -z "${server_ip}" ]; then
        printf 'deploy/.env 的 SERVER_IP 为空，deploy.sh 拼不出镜像仓库地址'
        return 0
    fi
    printf 'deploy.sh'
}

# 等价部署：用仓库里同一份 chart 渲染 e2b-webhook 清单并注入证书，
# 与 deploy.sh deploy-webhook 的步骤 2–6 一一对应（只是不经过 deploy.sh）。
webhook_deploy_via_chart() {
    local log_file="${LOG_DIR}/deploy-webhook.log"
    local chart values manifest registry

    chart=$(webhook_chart_dir) || {
        log_error "找不到 helm chart（deploy/helm 或 helm/）"
        return 1
    }
    registry=$(webhook_registry_url) || {
        log_error "无法解析镜像仓库地址（REGISTRY_URL / deploy/.env / 运行中镜像前缀）"
        return 1
    }
    values="${WV_TMP}/webhook-values.yaml"
    manifest="${WV_TMP}/webhook-manifest.yaml"
    : > "${log_file}"

    if ! (
        cd "${REPO_ROOT}" || exit 1
        set -a
        # shellcheck disable=SC1091
        . deploy/.env
        set +a
        export ENABLE_WEBHOOK=true
        export REGISTRY_URL="${registry}"
        export WEBHOOK_DOCKER_IMAGE="e2b-webhook:${WV_IMAGE_TAG}"
        envsubst < "${chart}/values-template.yaml" > "${values}"
    ) >>"${log_file}" 2>&1; then
        log_error "渲染 values.yaml 失败，日志: ${log_file}"
        return 1
    fi

    if ! helm template e2b-api "${chart}" -f "${values}" \
        --show-only templates/e2b-webhook.yaml > "${manifest}" 2>>"${log_file}"; then
        log_error "helm template 失败，日志: ${log_file}"
        return 1
    fi

    if ! kubectl apply -f "${manifest}" >>"${log_file}" 2>&1; then
        log_error "kubectl apply 失败，日志: ${log_file}"
        return 1
    fi

    if ! webhook_setup_certs >>"${log_file}" 2>&1; then
        log_error "证书/Secret/caBundle 配置失败，日志: ${log_file}"
        return 1
    fi
    return 0
}

# 生成自签证书、创建 TLS 与 api-key Secret、把证书注入 MWC 的 caBundle。
webhook_setup_certs() {
    local tls_dir svc fqdn ca_bundle api_key count patch

    svc="${WV_SERVICE}.${WV_NAMESPACE}"
    fqdn="${svc}.svc"
    tls_dir=$(mktemp -d)

    if ! openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout "${tls_dir}/tls.key" -out "${tls_dir}/tls.crt" \
        -subj "/CN=${svc}" \
        -addext "subjectAltName=DNS:${svc},DNS:${fqdn},DNS:${fqdn}.cluster.local" >/dev/null 2>&1; then
        log_error "openssl 生成证书失败"
        rm -rf "${tls_dir}"
        return 1
    fi

    kubectl -n "${WV_NAMESPACE}" delete secret "${WV_TLS_SECRET}" --ignore-not-found=true >/dev/null 2>&1 || true
    if ! kubectl -n "${WV_NAMESPACE}" create secret tls "${WV_TLS_SECRET}" \
        --cert="${tls_dir}/tls.crt" --key="${tls_dir}/tls.key" >/dev/null; then
        log_error "创建 TLS Secret 失败"
        rm -rf "${tls_dir}"
        return 1
    fi

    api_key=$(webhook_api_key 2>/dev/null || true)
    kubectl -n "${WV_NAMESPACE}" delete secret "${WV_APIKEY_SECRET}" --ignore-not-found=true >/dev/null 2>&1 || true
    if [ -n "${api_key}" ]; then
        kubectl -n "${WV_NAMESPACE}" create secret generic "${WV_APIKEY_SECRET}" \
            --from-literal=api-key="${api_key}" >/dev/null
    fi

    ca_bundle=$(kubectl -n "${WV_NAMESPACE}" get secret "${WV_TLS_SECRET}" \
        -o jsonpath='{.data.tls\.crt}' 2>/dev/null || true)
    rm -rf "${tls_dir}"
    if [ -z "${ca_bundle}" ]; then
        log_error "读取不到 ${WV_TLS_SECRET} 的 tls.crt，无法注入 caBundle"
        return 1
    fi

    count=$(kubectl get mutatingwebhookconfiguration "${WV_MWC}" -o jsonpath='{.webhooks}' 2>/dev/null | jq 'length' 2>/dev/null || echo 0)
    if [ "${count:-0}" -lt 1 ]; then
        log_error "MutatingWebhookConfiguration ${WV_MWC} 没有 webhook 条目"
        return 1
    fi
    patch=$(jq -nc --arg cb "${ca_bundle}" --argjson n "${count}" \
        '[range(0; $n) | {op: "replace", path: "/webhooks/\(.)/clientConfig/caBundle", value: $cb}]')
    if ! kubectl patch mutatingwebhookconfiguration "${WV_MWC}" --type=json -p "${patch}" >/dev/null; then
        log_error "注入 caBundle 失败"
        return 1
    fi
    return 0
}

webhook_deploy_fresh() {
    local log_file="${LOG_DIR}/deploy-webhook.log"
    local env_file="${REPO_ROOT}/deploy/.env"
    local method

    if [ ! -f "${env_file}" ]; then
        log_error "缺少 ${env_file}，无法部署 e2b-webhook"
        return 1
    fi

    method=$(webhook_deploy_method)
    if [ "${method}" = "deploy.sh" ]; then
        # deploy.sh 会 source .env，因此必须把开关写进文件；原文件已在基线里备份。
        webhook_env_file_force ENABLE_WEBHOOK true
        # deploy.sh 在本仓库没有执行位，用 bash 显式调用。
        if ! ( cd "${REPO_ROOT}/deploy" && run_with_timeout 1800 bash ./deploy.sh deploy-webhook ) >"${log_file}" 2>&1; then
            log_error "deploy.sh deploy-webhook 失败，日志: ${log_file}"
            tail -30 "${log_file}" >&2 || true
            return 1
        fi
        state_set DEPLOY_METHOD "deploy.sh"
    else
        log_warn "deploy.sh deploy-webhook 不可用：${method}"
        log_warn "改用仓库内同一份 helm chart 渲染部署（等价步骤：apply 清单 + 证书 + caBundle）"
        state_set DEPLOY_METHOD "helm-chart"
        state_set DEPLOY_FALLBACK_REASON "${method}"
        if ! webhook_deploy_via_chart; then
            log_error "chart 渲染部署失败，日志: ${log_file}"
            tail -30 "${log_file}" >&2 || true
            return 1
        fi
    fi

    state_set FRESH_DEPLOY 1
    webhook_detect
    state_set FRESH_DEPLOY 1
    return 0
}

#==================== 运行期 env（降级用例用） ====================#
webhook_set_env() {
    # webhook_set_env KEY=VALUE [KEY=VALUE ...]
    kubectl -n "${WV_NAMESPACE}" set env "deploy/${WV_DEPLOYMENT}" "$@" >/dev/null
}

webhook_restore_stock_env() {
    local api_url timeout
    api_url=$(state_get STOCK_API_URL)
    timeout=$(state_get STOCK_API_TIMEOUT)
    [ -n "${api_url}" ] || return 0
    if [ -n "${timeout}" ]; then
        webhook_set_env "E2B_API_URL=${api_url}" "E2B_API_TIMEOUT=${timeout}"
    else
        webhook_set_env "E2B_API_URL=${api_url}"
    fi
    webhook_rollout_status 180 || true
    webhook_reconnect_admission || true
}

webhook_api_timeout_seconds() {
    local raw
    raw=$(state_get STOCK_API_TIMEOUT)
    [ -n "${raw}" ] || raw="5s"
    case "${raw}" in
        *s) printf '%s' "${raw%s}" ;;
        *ms) printf '%s' $(( ${raw%ms} / 1000 )) ;;
        *) printf '5' ;;
    esac
}

#==================== API Key ====================#
# 凭证只走内存/环境变量，绝不写入 state.env 或日志。
webhook_api_key() {
    if [ -n "${E2B_API_KEY:-}" ]; then
        printf '%s' "${E2B_API_KEY}"
        return 0
    fi
    if kubectl -n "${WV_NAMESPACE}" get secret "${WV_APIKEY_SECRET}" >/dev/null 2>&1; then
        local key
        key=$(kubectl -n "${WV_NAMESPACE}" get secret "${WV_APIKEY_SECRET}" -o jsonpath='{.data.api-key}' 2>/dev/null | base64 -d 2>/dev/null || true)
        if [ -n "${key}" ]; then
            printf '%s' "${key}"
            return 0
        fi
    fi
    if [ -f "${WV_E2B_CONFIG}" ]; then
        local key
        key=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1])).get('teamApiKey',''))" "${WV_E2B_CONFIG}" 2>/dev/null || true)
        if [ -n "${key}" ]; then
            printf '%s' "${key}"
            return 0
        fi
    fi
    return 1
}

#==================== mock API（02 / A22 用） ====================#
MOCK_PID_FILE="${WV_TMP}/mock_api.pid"
MOCK_LOG="${LOG_DIR}/mock-api.log"
MOCK_CLUSTER_REACHABLE=""

mock_api_host() {
    if [ -n "${WV_MOCK_HOST}" ]; then
        printf '%s' "${WV_MOCK_HOST}"
        return 0
    fi
    local ip
    ip=$(k8s_node_internal_ip 0 2>/dev/null || true)
    if [ -z "${ip}" ]; then
        ip="127.0.0.1"
    fi
    printf '%s' "${ip}"
}

mock_api_url() {
    printf 'http://%s:%s' "$(mock_api_host)" "${WV_MOCK_PORT}"
}

mock_api_health_local() {
    curl -sf -m 3 "http://127.0.0.1:${WV_MOCK_PORT}/healthz" -o /dev/null 2>/dev/null
}

mock_api_start() {
    mock_api_stop
    : > "${MOCK_LOG}"
    python3 "${WV_VERIFY_ROOT}/lib/mock_api.py" \
        --host 0.0.0.0 --port "${WV_MOCK_PORT}" \
        --control-port "$(( ${WV_MOCK_PORT} + 1 ))" "$@" >>"${MOCK_LOG}" 2>&1 &
    local pid=$!
    printf '%s' "${pid}" > "${MOCK_PID_FILE}"

    local deadline=$(( $(date +%s) + 15 ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! kill -0 "${pid}" 2>/dev/null; then
            log_error "mock API 启动失败: $(tail -5 "${MOCK_LOG}" 2>/dev/null | tr '\n' ' ')"
            return 1
        fi
        if mock_api_health_local; then
            return 0
        fi
        sleep 0.3
    done
    log_error "mock API 启动超时（日志 ${MOCK_LOG}）"
    return 1
}

mock_api_stop() {
    [ -f "${MOCK_PID_FILE}" ] || return 0
    local pid
    pid=$(cat "${MOCK_PID_FILE}" 2>/dev/null || true)
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
        sleep 0.3
        kill -9 "${pid}" 2>/dev/null || true
    fi
    rm -f "${MOCK_PID_FILE}"
}

mock_api_restart() {
    mock_api_stop
    mock_api_start "$@"
}

mock_api_count() {
    curl -sS -m 5 "http://127.0.0.1:${WV_MOCK_PORT}/count" 2>/dev/null | jq -r '.count // 0' 2>/dev/null || echo 0
}

mock_api_last_headers() {
    curl -sS -m 5 "http://127.0.0.1:${WV_MOCK_PORT}/last-headers" 2>/dev/null || printf '{}'
}

# 最后一次 transform 响应的证据（status/bytes/delay/mode）。
# 用例据此确认“故障注入真的生效”，而不是 webhook 根本没发请求也判降级成功。
mock_api_last_response() {
    curl -sS -m 5 "$(mock_api_control_url)/last-response" 2>/dev/null || printf '{}'
}

# mock 跑在宿主机上，webhook 在 Pod 网络里；宿主机地址不一定通。
# 用一次性 busybox Pod 做一次真实可达性判断，避免把“网络不通”误判成“降级逻辑错”。
mock_api_reachable_from_cluster() {
    if [ -n "${MOCK_CLUSTER_REACHABLE}" ]; then
        [ "${MOCK_CLUSTER_REACHABLE}" = "1" ]
        return
    fi
    local url output
    url=$(mock_api_url)
    if output=$(k8s_probe_run "mock API 可达性 ${url}" "wget -q -T 3 -O - ${url}/healthz" 2>/dev/null); then
        if [ "${output}" = "ok" ]; then
            MOCK_CLUSTER_REACHABLE=1
            return 0
        fi
    fi
    MOCK_CLUSTER_REACHABLE=0
    return 1
}

# 把 webhook 指向 mock 并等滚动完成。
# env 变更会触发滚动更新，滚动后原来的 port-forward 已经指向旧 Pod，
# 必须重建，否则后续直连 /mutate 的请求会全部连接失败（http=000）。
webhook_reconnect_admission() {
    if declare -F admission_stop_port_forward >/dev/null 2>&1; then
        admission_stop_port_forward
    fi
    if declare -F admission_ensure_port_forward >/dev/null 2>&1; then
        admission_ensure_port_forward || return 1
    fi
    return 0
}

#==================== mock 行为切换（控制面） ====================#
# 控制面默认监听 --port+1，只在本机访问；数据面才暴露给集群内的 webhook。
# 这样同一次 env 切换后，可以按用例改 mock 行为，不必为每条用例滚动一次 Deployment。
mock_api_control_url() {
    printf 'http://127.0.0.1:%s' "$(( ${WV_MOCK_PORT} + 1 ))"
}

# mock_api_set_mode <json>：控制面返回 {"ok":true,...} 才算成功。
mock_api_set_mode() {
    local payload="$1" resp
    resp=$(curl -sS -m 5 -X POST -H 'Content-Type: application/json' \
        --data "${payload}" "$(mock_api_control_url)/mode" 2>/dev/null || true)
    printf '%s' "${resp}" | jq -e '.ok == true' >/dev/null 2>&1
}

mock_api_mode_normal()    { mock_api_set_mode '{"mode":"normal"}'; }
mock_api_mode_refuse()    { mock_api_set_mode '{"mode":"refuse"}'; }
mock_api_mode_status()    { mock_api_set_mode "$(jq -nc --argjson v "${1}" '{mode:"status",value:$v}')"; }
mock_api_mode_body()      { mock_api_set_mode "$(jq -nc --arg v "${1}" '{mode:"body",value:$v}')"; }
mock_api_mode_body_file() { mock_api_set_mode "$(jq -nc --arg v "${1}" '{mode:"body_file",value:$v}')"; }
mock_api_mode_delay()     { mock_api_set_mode "$(jq -nc --argjson v "${1}" '{mode:"delay",value:$v}')"; }
mock_api_mode_size()      { mock_api_set_mode "$(jq -nc --argjson v "${1}" '{mode:"size",value:$v}')"; }
mock_api_mode_get() {
    curl -sS -m 5 "$(mock_api_control_url)/mode" 2>/dev/null | jq -r '.mode // ""' 2>/dev/null || true
}

# 把 webhook 指向 mock 并等滚动完成。整轮只需要做一次，之后用 mock_api_mode_* 切行为。
webhook_switch_to_mock() {
    webhook_set_env "E2B_API_URL=$(mock_api_url)" || return 1
    webhook_rollout_status 180 || return 1
    webhook_reconnect_admission
}


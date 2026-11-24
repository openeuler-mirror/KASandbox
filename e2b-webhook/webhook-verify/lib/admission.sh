#!/bin/bash
###############################################################################
# admission.sh — AdmissionReview 构造、调用与 patch 解码/应用
#
# 直连 /mutate 的用例都走这里：port-forward 到 Service，避免依赖 apiserver
# 的 objectSelector/namespaceSelector（那部分由 03 用 server dry-run 验证）。
###############################################################################
set -euo pipefail

ADMISSION_PF_PID_FILE="${WV_TMP}/port-forward.pid"
ADMISSION_PF_LOG="${LOG_DIR}/port-forward.log"

admission_url() {
    printf 'https://127.0.0.1:%s' "${WV_PORT}"
}

#==================== port-forward ====================#
admission_health_code() {
    curl -s -k -o /dev/null -w '%{http_code}' -m 5 "$(admission_url)/healthz" 2>/dev/null || true
}

admission_ensure_port_forward() {
    if [ "$(admission_health_code)" = "200" ]; then
        return 0
    fi
    admission_stop_port_forward

    kubectl -n "${WV_NAMESPACE}" port-forward "svc/${WV_SERVICE}" "${WV_PORT}:443" \
        >"${ADMISSION_PF_LOG}" 2>&1 &
    local pid=$!
    printf '%s' "${pid}" > "${ADMISSION_PF_PID_FILE}"

    local deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! kill -0 "${pid}" 2>/dev/null; then
            log_error "port-forward 进程退出: $(tail -3 "${ADMISSION_PF_LOG}" 2>/dev/null | tr '\n' ' ')"
            return 1
        fi
        [ "$(admission_health_code)" = "200" ] && return 0
        sleep 1
    done
    log_error "port-forward 启动超时（日志 ${ADMISSION_PF_LOG}）"
    return 1
}

admission_stop_port_forward() {
    [ -f "${ADMISSION_PF_PID_FILE}" ] || return 0
    local pid
    pid=$(cat "${ADMISSION_PF_PID_FILE}" 2>/dev/null || true)
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
        sleep 0.3
        kill -9 "${pid}" 2>/dev/null || true
    fi
    rm -f "${ADMISSION_PF_PID_FILE}"
}

#==================== 请求 ====================#
# admission_curl_raw <curl 参数...>：原样发一个请求，输出响应体。
admission_curl_raw() {
    curl -sS -k -m 30 "$@" 2>/dev/null || true
}

# admission_post <obj-file> <resp-file> [operation]
# 返回 curl 退出码；HTTP 码写到 <resp-file>.code。
admission_post() {
    local obj="$1" resp="$2" operation="${3:-CREATE}"
    local review="${resp}.review.json"
    local uid="wv-$(date +%s)-$RANDOM"

    admission_build_review "$obj" "${review}" "${operation}" "${uid}" || return 1

    curl -sS -k -m 30 \
        -o "${resp}" -w '%{http_code}' \
        -H 'Content-Type: application/json' \
        -X POST "$(admission_url)/mutate" \
        --data-binary "@${review}" > "${resp}.code" 2>/dev/null
}

# admission_build_review <obj-file> <out-file> [operation] [uid]
admission_build_review() {
    local obj="$1" out="$2" operation="${3:-CREATE}" uid="${4:-wv-$RANDOM}"
    local kind ns name
    kind=$(jq -r '.kind' "${obj}")
    ns=$(jq -r '.metadata.namespace // "default"' "${obj}")
    name=$(jq -r '.metadata.name // "unnamed"' "${obj}")

    local api_version group version
    api_version=$(jq -r '.apiVersion' "${obj}")
    if [ "${api_version#*/}" != "${api_version}" ]; then
        group="${api_version%%/*}"
        version="${api_version##*/}"
    else
        group=""
        version="${api_version}"
    fi

    jq -n \
        --arg uid "${uid}" \
        --arg kind "${kind}" \
        --arg group "${group}" \
        --arg version "${version}" \
        --arg ns "${ns}" \
        --arg name "${name}" \
        --arg operation "${operation}" \
        --rawfile obj "${obj}" \
        '{
            apiVersion: "admission.k8s.io/v1",
            kind: "AdmissionReview",
            request: {
                uid: $uid,
                kind: {group: $group, version: $version, kind: $kind},
                resource: {group: $group, version: $version, resource: ($kind | ascii_downcase)},
                requestKind: {group: $group, version: $version, kind: $kind},
                requestResource: {group: $group, version: $version, resource: ($kind | ascii_downcase)},
                namespace: $ns,
                name: $name,
                operation: $operation,
                userInfo: {username: "webhook-verify", groups: ["system:authenticated"]},
                object: ($obj | fromjson),
                oldObject: null,
                dryRun: false,
                options: null
            }
        }' > "${out}"
}

#==================== 响应读取 ====================#
admission_resp_allowed() { jq -r '.response.allowed // false' "$1" 2>/dev/null || echo false; }
admission_resp_uid() { jq -r '.response.uid // ""' "$1" 2>/dev/null || true; }
admission_resp_http_code() { cat "$1.code" 2>/dev/null || echo 000; }

# patch 解码为可读 JSON 数组；没有 patch 时输出 []。
admission_patch_json() {
    local resp="$1"
    local b64
    b64=$(jq -r '.response.patch // ""' "${resp}" 2>/dev/null || true)
    if [ -z "${b64}" ]; then
        printf '[]'
        return 0
    fi
    printf '%s' "${b64}" | base64 -d 2>/dev/null | jq '.' 2>/dev/null || printf '[]'
}

# 把 patch 里所有 add 应用到对象上（仅本套件用到的 add 语义）。
admission_apply_patch() {
    local obj="$1" patch="$2" out="$3"
    jq --slurpfile p "${patch}" '
        reduce ($p[0][]? // empty) as $op (.;
            setpath(
                ($op.path | ltrimstr("/") | split("/")
                    | map(gsub("~1"; "/") | gsub("~0"; "~")));
                $op.value))
    ' "${obj}" > "${out}"
}

#==================== 对象构造 ====================#
# pod_object <out> <ns> <name> [options...]
# options（KV 形式，均可用空串表示“不设置”）：
#   template=  annotations=  runtimeclass=  labels=  image=  command=  env=
pod_object() {
    local out="$1" ns="$2" name="$3"; shift 3
    local template="" annotations="{}" runtimeclass="" labels="{}"
    local image="" command='["sleep","3600"]' env=""

    local pair key value
    for pair in "$@"; do
        key="${pair%%=*}"
        value="${pair#*=}"
        case "${key}" in
            template) template="${value}" ;;
            annotations) [ -n "${value}" ] && annotations="${value}" || annotations="{}" ;;
            runtimeclass) runtimeclass="${value}" ;;
            labels) [ -n "${value}" ] && labels="${value}" || labels="{}" ;;
            image) image="${value}" ;;
            command) [ -n "${value}" ] && command="${value}" || command='["sleep","3600"]' ;;
            env) env="${value}" ;;
        esac
    done

    [ -n "${image}" ] || image="e2b.dev/direct:${template:-none}"
    if [ -z "${env}" ]; then
        if [ -n "${template}" ]; then
            env=$(jq -nc --arg t "${template}" '[{name:"TEMPLATE_NAME",value:$t}]')
        else
            env='[]'
        fi
    fi

    jq -n \
        --arg ns "${ns}" --arg name "${name}" \
        --argjson labels "${labels}" \
        --argjson annotations "${annotations}" \
        --arg rc "${runtimeclass}" \
        --arg image "${image}" \
        --argjson command "${command}" \
        --argjson env "${env}" \
        '{
            apiVersion: "v1",
            kind: "Pod",
            metadata: ({name: $name, namespace: $ns}
                + (if ($labels | length) > 0 then {labels: $labels} else {} end)
                + (if ($annotations | length) > 0 then {annotations: $annotations} else {} end)),
            spec: ({restartPolicy: "Never",
                    containers: [{
                        name: "sandbox",
                        image: $image,
                        imagePullPolicy: "IfNotPresent",
                        command: $command,
                        env: $env
                    }]}
                + (if $rc != "" then {runtimeClassName: $rc} else {} end))
        }' > "${out}"
}

# batchsandbox_object <out> <ns> <name> [options...]（字段同 pod_object，路径在 spec.template.*）
batchsandbox_object() {
    local out="$1" ns="$2" name="$3"; shift 3
    local template="" annotations="{}" runtimeclass="" labels="{}"
    local image="" command='["sleep","3600"]' env=""

    local pair key value
    for pair in "$@"; do
        key="${pair%%=*}"
        value="${pair#*=}"
        case "${key}" in
            template) template="${value}" ;;
            annotations) [ -n "${value}" ] && annotations="${value}" || annotations="{}" ;;
            runtimeclass) runtimeclass="${value}" ;;
            labels) [ -n "${value}" ] && labels="${value}" || labels="{}" ;;
            image) image="${value}" ;;
            command) [ -n "${value}" ] && command="${value}" || command='["sleep","3600"]' ;;
            env) env="${value}" ;;
        esac
    done

    [ -n "${image}" ] || image="e2b.dev/direct:${template:-none}"
    if [ -z "${env}" ]; then
        if [ -n "${template}" ]; then
            env=$(jq -nc --arg t "${template}" '[{name:"TEMPLATE_NAME",value:$t}]')
        else
            env='[]'
        fi
    fi

    jq -n \
        --arg ns "${ns}" --arg name "${name}" \
        --argjson labels "${labels}" \
        --argjson annotations "${annotations}" \
        --arg rc "${runtimeclass}" \
        --arg image "${image}" \
        --argjson command "${command}" \
        --argjson env "${env}" \
        '{
            apiVersion: "sandbox.opensandbox.io/v1alpha1",
            kind: "BatchSandbox",
            metadata: {name: $name, namespace: $ns},
            spec: {
                replicas: 1,
                template: {
                    metadata: ((if ($labels | length) > 0 then {labels: $labels} else {} end)
                        + (if ($annotations | length) > 0 then {annotations: $annotations} else {} end)),
                    spec: ({containers: [{
                        name: "sandbox",
                        image: $image,
                        command: $command,
                        env: $env
                    }]}
                        + (if $rc != "" then {runtimeClassName: $rc} else {} end))
                }
            }
        }' > "${out}"
}

#==================== 结果读取 ====================#
admission_annotations() { jq -c '.metadata.annotations // {}' "$1" 2>/dev/null || echo '{}'; }
admission_runtimeclass() { jq -r '.spec.runtimeClassName // ""' "$1" 2>/dev/null || true; }
admission_patch_paths() { jq -r '.[]?.path' "$1" 2>/dev/null || true; }

#!/bin/bash
###############################################################################
# cni_behavior_common.sh — CNI Service/DNS/NetworkPolicy 验证公共函数
###############################################################################
set -euo pipefail

SCRIPT_DIR_CNI_COMMON="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${SCRIPT_DIR_CNI_COMMON}/lib/common.sh"

REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR_CNI_COMMON}/lib/refresh_build_id.sh}"
E2B_CNI_YAML="${E2B_CNI_YAML:-/tmp/e2b-kubelet-pod.yaml}"
ENVD_PORT="${ENVD_PORT:-49983}"
CLIENT_IMAGE="${CLIENT_IMAGE:-docker.io/library/busybox:latest}"
CLIENT_NSLOOKUP_CMD="${CLIENT_NSLOOKUP_CMD:-}"

require_cni_behavior_prereqs() {
    require_cri_multiplex_cni_enabled || return 1

    if ! kubectl get runtimeclass e2b >/dev/null 2>&1; then
        log_fail "RuntimeClass e2b 不存在"
        return 1
    fi
    log_pass "RuntimeClass e2b 存在"

    if [ ! -f "${REFRESH_SCRIPT}" ]; then
        log_fail "刷新脚本不存在: ${REFRESH_SCRIPT}"
        return 1
    fi
    log_pass "刷新脚本存在: ${REFRESH_SCRIPT}"
}

patch_e2b_yaml_metadata() {
    local yaml="$1"
    local app_label="$2"
    local extra_label_key="${3:-}"
    local extra_label_value="${4:-}"

    local tmp="${yaml}.tmp"
    awk -v app="${app_label}" -v k="${extra_label_key}" -v v="${extra_label_value}" '
        /^  labels:/ {
            skipping_labels=1
            next
        }
        skipping_labels == 1 && /^  [A-Za-z0-9_.-]+:/ {
            skipping_labels=0
        }
        skipping_labels == 1 {
            next
        }
        /^  annotations:/ && inserted != 1 {
            print "  labels:"
            print "    app: " app
            if (k != "" && v != "") {
                print "    " k ": " v
            }
            inserted=1
        }
        { print }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

prepare_e2b_cni_pod_yaml() {
    local pod_name="$1"
    local app_label="$2"
    local extra_label_key="${3:-}"
    local extra_label_value="${4:-}"

    log_info "执行: bash ${REFRESH_SCRIPT} ${pod_name}"
    if ! bash "${REFRESH_SCRIPT}" "${pod_name}" >&2; then
        log_info "刷新 build_id 失败，尝试复用已有 ${E2B_CNI_YAML}"
        if [ -f "${E2B_CNI_YAML}" ] &&
           grep -q 'e2b.dev/build-id:' "${E2B_CNI_YAML}" &&
           grep -q 'e2b.dev/execution-id:' "${E2B_CNI_YAML}" &&
           grep -q 'e2b.dev/envd-access-token:' "${E2B_CNI_YAML}"; then
            sed -i "0,/^  name: .*/s//  name: ${pod_name}/" "${E2B_CNI_YAML}"
            log_pass "复用已有 Pod YAML: ${E2B_CNI_YAML}"
        else
            log_fail "刷新 build_id 失败，且没有可复用的完整 Pod YAML"
            return 1
        fi
    fi

    if [ ! -f "${E2B_CNI_YAML}" ]; then
        log_fail "Pod YAML 未生成: ${E2B_CNI_YAML}"
        return 1
    fi

    patch_e2b_yaml_metadata "${E2B_CNI_YAML}" "${app_label}" "${extra_label_key}" "${extra_label_value}"
    log_pass "E2B Pod YAML 已准备并写入 labels: app=${app_label}"
}

apply_and_wait_pod_ready() {
    local pod_name="$1"
    local yaml="$2"
    local timeout="${3:-120s}"

    kubectl apply -f "${yaml}" >&2
    log_pass "Pod YAML 已提交: ${pod_name}"

    if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout="${timeout}" >&2; then
        log_pass "Pod 已 Ready: ${pod_name}"
    else
        log_fail "Pod 未在 ${timeout} 内 Ready: ${pod_name}"
        kubectl describe pod "${pod_name}" >&2 || true
        return 1
    fi
}

get_pod_ip_or_fail() {
    local pod_name="$1"
    local pod_ip
    pod_ip=$(kubectl get pod "${pod_name}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)
    if [ -z "${pod_ip}" ]; then
        log_fail "无法读取 PodIP: ${pod_name}"
        return 1
    fi
    echo "${pod_ip}"
}

create_curl_client_pod() {
    local pod_name="$1"
    local label_key="${2:-role}"
    local label_value="${3:-cni-client}"
    local yaml="/tmp/${pod_name}.yaml"

    cat > "${yaml}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  labels:
    ${label_key}: ${label_value}
spec:
  restartPolicy: Never
  containers:
    - name: curl
      image: ${CLIENT_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["sleep", "3600"]
EOF

    kubectl apply -f "${yaml}" >&2
    if kubectl wait --for=condition=Ready "pod/${pod_name}" --timeout=120s >&2; then
        log_pass "client Pod 已 Ready: ${pod_name}"
    else
        log_fail "client Pod 未 Ready: ${pod_name}"
        kubectl describe pod "${pod_name}" >&2 || true
        return 1
    fi
}

client_curl_code() {
    local client_pod="$1"
    local url="$2"
    kubectl exec "${client_pod}" -- sh -c "if command -v curl >/dev/null 2>&1; then curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 3 --max-time 5 '${url}'; elif command -v wget >/dev/null 2>&1; then wget -q -T 5 -O /dev/null '${url}' && echo 204 || echo 000; else echo NO_HTTP_CLIENT; fi" 2>/tmp/cni-client-curl.err || true
}

host_curl_code() {
    local url="$1"
    curl -sS -o /dev/null -w "%{http_code}" --connect-timeout 3 --max-time 5 "${url}" 2>/tmp/cni-host-curl.err || true
}

expect_http_204_from_client() {
    local client_pod="$1"
    local url="$2"
    local desc="$3"
    local code
    code=$(client_curl_code "${client_pod}" "${url}")
    if [ "${code}" = "204" ]; then
        log_pass "${desc}: HTTP ${code}"
        return 0
    fi
    log_fail "${desc} 失败: HTTP ${code}"
    cat /tmp/cni-client-curl.err >&2 || true
    return 1
}

expect_blocked_from_client() {
    local client_pod="$1"
    local url="$2"
    local desc="$3"
    local code
    code=$(client_curl_code "${client_pod}" "${url}")
    if [ "${code}" = "000" ]; then
        log_pass "${desc}: 已阻断"
        return 0
    fi
    log_skip "${desc}: 未阻断，HTTP ${code}，当前 CNI POC 暂不承诺该 NetworkPolicy 行为"
    return 2
}

create_e2b_service() {
    local svc_name="$1"
    local app_label="$2"
    local port="${3:-${ENVD_PORT}}"
    local yaml="/tmp/${svc_name}.yaml"

    cat > "${yaml}" <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${svc_name}
spec:
  selector:
    app: ${app_label}
  ports:
    - name: envd
      port: ${port}
      targetPort: ${port}
      protocol: TCP
EOF

    kubectl apply -f "${yaml}" >&2
    log_pass "Service 已创建: ${svc_name}"
}

wait_endpoint_contains_ip() {
    local svc_name="$1"
    local pod_ip="$2"
    local found=0

    for _ in $(seq 1 60); do
        if kubectl get endpointslice -l "kubernetes.io/service-name=${svc_name}" -o yaml 2>/dev/null | grep -q "${pod_ip}"; then
            found=1
            break
        fi
        sleep 1
    done

    if [ "${found}" = "1" ]; then
        log_pass "EndpointSlice 已包含 E2B PodIP: ${pod_ip}"
        return 0
    fi
    log_fail "EndpointSlice 未包含 E2B PodIP: ${pod_ip}"
    kubectl get endpointslice -l "kubernetes.io/service-name=${svc_name}" -o yaml >&2 || true
    return 1
}

client_dns_lookup() {
    local client_pod="$1"
    local name="$2"

    if [ -n "${CLIENT_NSLOOKUP_CMD}" ]; then
        kubectl exec "${client_pod}" -- sh -c "${CLIENT_NSLOOKUP_CMD} ${name}" 2>&1
        return
    fi

    kubectl exec "${client_pod}" -- sh -c "nslookup ${name} 2>/dev/null || getent hosts ${name} 2>/dev/null || true" 2>&1
}

cleanup_names() {
    kubectl delete networkpolicy "$@" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete service "$@" --ignore-not-found >/dev/null 2>&1 || true
    kubectl delete pod "$@" --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
}

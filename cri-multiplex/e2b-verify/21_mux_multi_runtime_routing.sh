#!/bin/bash
###############################################################################
# 21_mux_multi_runtime_routing.sh -- cri-multiplex 多 Runtime 路由验证
#
# 验证目标：
#   1. 无 RuntimeClass 的普通 Pod 路由到 containerd 默认 runtime。
#   2. Android Pod 路由到 Android engine。
#   3. E2B Pod 与普通 Pod 同时存在时，ListPodSandbox/ListContainers 合并返回。
#   4. Exec 按 containerID 路由，不在 E2B / containerd 间串路由。
#   5. Attach 按 E2B containerID 路由到 E2B stream server。
#      Android engine 当前不支持 Exec/Attach，仅验证创建、状态和列表路由。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "21 -- cri-multiplex 多 Runtime 路由验证"

DEFAULT_POD="${DEFAULT_POD:-mux-default-runtime-21}"
ANDROID_POD="${ANDROID_POD:-mux-android-runtime-21}"
E2B_POD="${E2B_POD:-mux-e2b-runtime-21}"
ANDROID_RUNTIMECLASS="${ANDROID_RUNTIMECLASS:-android}"
ANDROID_RUNTIME_HANDLER="${ANDROID_RUNTIME_HANDLER:-android}"
ANDROID_ADB_PORT="${ANDROID_ADB_PORT:-6520}"
ANDROID_BASE_INSTANCE_NUM="${ANDROID_BASE_INSTANCE_NUM:-1}"
POD_YAML="${POD_YAML:-/tmp/e2b-kubelet-pod.yaml}"
E2B_WORK_YAML="${E2B_WORK_YAML:-/tmp/e2b-mux-routing-21.yaml}"
ANDROID_WORK_YAML="${ANDROID_WORK_YAML:-/tmp/e2b-mux-android-21.yaml}"
REFRESH_SCRIPT="${REFRESH_SCRIPT:-${SCRIPT_DIR}/lib/refresh_build_id.sh}"
CLIENT_IMAGE="${CLIENT_IMAGE:-docker.io/library/busybox:latest}"

cleanup() {
    kubectl delete pod "${DEFAULT_POD}" "${ANDROID_POD}" "${E2B_POD}" \
        --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true
    rm -f "${E2B_WORK_YAML}" "${ANDROID_WORK_YAML}" "/tmp/${DEFAULT_POD}.yaml" "/tmp/${ANDROID_POD}.yaml" || true
}
trap cleanup EXIT

assert_log_contains() {
    local pattern="$1"
    local desc="$2"

    if grep -aqE "${pattern}" /tmp/cri-multiplex.log 2>/dev/null; then
        log_pass "${desc}"
        return 0
    fi
    log_fail "${desc}"
    tail -n 120 /tmp/cri-multiplex.log >&2 || true
    return 1
}

create_android_pod_yaml() {
    local pod="$1"
    local yaml="$2"

    cat > "${yaml}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  annotations:
    android.dev/adb-port: "${ANDROID_ADB_PORT}"
    android.dev/base-instance-num: "${ANDROID_BASE_INSTANCE_NUM}"
spec:
  runtimeClassName: ${ANDROID_RUNTIMECLASS}
  restartPolicy: Never
  containers:
    - name: android
      image: android.dev/cvd:local
      imagePullPolicy: IfNotPresent
EOF
}

make_e2b_attach_yaml() {
    local yaml="$1"
    local tmp="${yaml}.tmp"

    awk '
        /^  containers:/ {
            in_containers=1
            print
            next
        }
        in_containers == 1 && /^  - name:/ && first_container_seen != 1 {
            first_container_seen=1
            in_first_container=1
            print
            next
        }
        in_first_container == 1 && /^  - name:/ {
            if (inserted != 1) {
                print "    stdin: true"
                print "    tty: true"
                print "    command: [\"sh\"]"
                inserted=1
            }
            in_first_container=0
            print
            next
        }
        in_first_container == 1 && /^    (stdin|tty|command):/ {
            next
        }
        in_first_container == 1 && /^    imagePullPolicy:/ {
            print
            if (inserted != 1) {
                print "    stdin: true"
                print "    tty: true"
                print "    command: [\"sh\"]"
                inserted=1
            }
            next
        }
        { print }
        END {
            if (first_container_seen != 1 || inserted != 1) {
                exit 1
            }
        }
    ' "${yaml}" > "${tmp}"
    mv "${tmp}" "${yaml}"
}

assert_kubectl_exec_output() {
    local pod="$1"
    local expected="$2"
    shift 2

    local output
    output=$(kubectl_exec_output_with_retry "${pod}" 45 "$@" 2>&1) || true
    if echo "${output}" | grep -q "${expected}"; then
        log_pass "kubectl exec 路由正确: ${pod}"
        return 0
    fi
    log_fail "kubectl exec 输出不匹配: pod=${pod}, output=${output}"
    return 1
}

assert_attach_output() {
    local pod="$1"
    local expected="$2"

    local output rc
    set +e
    output=$(timeout 12 sh -c "printf 'echo ${expected}\nexit\n' | kubectl attach -i '${pod}' -c app" 2>&1)
    rc=$?
    set -e

    log_info "kubectl attach ${pod} 退出码: ${rc}"
    log_info "kubectl attach ${pod} 输出:\n${output}"
    if echo "${output}" | grep -q "${expected}"; then
        log_pass "kubectl attach 路由正确: ${pod}"
        return 0
    fi
    log_fail "kubectl attach 输出不匹配: pod=${pod}, output=${output}"
    return 1
}

log_step "1.1 前置检查"
require_refresh_script "${REFRESH_SCRIPT}" || exit 1
if ! require_cri_multiplex_cni_enabled_quiet; then
    log_info "cri-multiplex 未处于 CNI+Android 模式，准备按 21 号用例自身启动"
    start_cni_android_multiplex "启动 cri-multiplex CNI+Android runtime 模式" || exit 1
    require_cri_multiplex_cni_enabled || exit 1
fi
if ! kubectl get runtimeclass e2b >/dev/null 2>&1; then
    log_fail "RuntimeClass e2b 不存在"
    exit 1
fi
log_pass "RuntimeClass e2b 存在"

log_step "1.2 清理旧资源"
cleanup
: > /tmp/cri-multiplex.log 2>/dev/null || true
log_pass "旧资源已清理"

log_step "2.1 创建无 RuntimeClass 普通 Pod"
prepare_default_busybox_pod_yaml "${DEFAULT_POD}" "/tmp/${DEFAULT_POD}.yaml"
kubectl apply -f "/tmp/${DEFAULT_POD}.yaml" >&2
wait_pod_ready "${DEFAULT_POD}" 120s || exit 1
assert_log_contains "RunPodSandbox routing: handler=\"\" -> container" "无 RuntimeClass Pod 已路由到 containerd" || exit 1

log_step "2.2 创建 Android Pod"
kubectl apply -f - >&2 <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: ${ANDROID_RUNTIMECLASS}
handler: ${ANDROID_RUNTIME_HANDLER}
EOF
create_android_pod_yaml "${ANDROID_POD}" "${ANDROID_WORK_YAML}"
kubectl apply -f "${ANDROID_WORK_YAML}" >&2
wait_pod_ready "${ANDROID_POD}" 240s || exit 1
assert_log_contains "RunPodSandbox routing: handler=\"${ANDROID_RUNTIME_HANDLER}\" -> android" "RuntimeClass=${ANDROID_RUNTIMECLASS} Pod 已路由到 Android" || exit 1

log_step "2.3 创建 E2B Pod"
if ! E2B_YAML_COUNT=0 E2B_BASE_POD_YAML="${POD_YAML}" prepare_e2b_pod_yaml "${E2B_POD}" "${E2B_WORK_YAML}"; then
    exit 1
fi
make_e2b_attach_yaml "${E2B_WORK_YAML}"
kubectl apply -f "${E2B_WORK_YAML}" >&2
wait_pod_ready "${E2B_POD}" 120s || exit 1
assert_log_contains "RunPodSandbox routing: handler=\"e2b\" -> e2b" "RuntimeClass=e2b Pod 已路由到 E2B" || exit 1

log_step "3.1 验证 ListPodSandbox 多后端合并"
pods_output=$(${CRICTL} pods 2>&1 || true)
if echo "${pods_output}" | grep -q "${DEFAULT_POD}" &&
   echo "${pods_output}" | grep -q "${ANDROID_POD}" &&
   echo "${pods_output}" | grep -q "${E2B_POD}"; then
    log_pass "ListPodSandbox 同时包含 containerd 默认、Android 和 E2B Pod"
else
    log_fail "ListPodSandbox 合并结果缺失: ${pods_output}"
    exit 1
fi

log_step "3.2 验证 ListContainers 多后端合并"
DEFAULT_CID=$(pod_container_id "${DEFAULT_POD}")
ANDROID_CID=$(pod_container_id "${ANDROID_POD}")
E2B_UID=$(kubectl get pod "${E2B_POD}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
E2B_CID=$(pod_container_id "${E2B_POD}")
if [ -z "${E2B_CID}" ]; then
    E2B_CID="${E2B_UID}-c"
fi
CONTAINERD_CID=$(crictl --runtime-endpoint "unix://${CONTAINERD_SOCKET}" ps -q 2>/dev/null | head -1 || true)
containers_output=""
for _ in $(seq 1 20); do
    containers_output=$(grpc_call "runtime.v1.RuntimeService/ListContainers" 2>&1 || true)
    if [ -n "${CONTAINERD_CID}" ] &&
       echo "${containers_output}" | grep -q "${CONTAINERD_CID:0:12}" &&
       echo "${containers_output}" | grep -q "${ANDROID_POD}" &&
       echo "${containers_output}" | grep -q "${E2B_POD}"; then
        break
    fi
    sleep 1
done
if [ -n "${DEFAULT_CID}" ] && [ -n "${CONTAINERD_CID}" ] &&
   [ -n "${ANDROID_CID}" ] && [ -n "${E2B_CID}" ] &&
   echo "${containers_output}" | grep -q "${CONTAINERD_CID:0:12}" &&
   echo "${containers_output}" | grep -q "${ANDROID_POD}" &&
   echo "${containers_output}" | grep -q "${E2B_POD}"; then
    log_pass "ListContainers 同时包含 containerd 默认、Android 和 E2B Container"
else
    log_fail "ListContainers 合并结果缺失: pod_default=${DEFAULT_CID} containerd=${CONTAINERD_CID} android=${ANDROID_CID} e2b=${E2B_CID} output=${containers_output}"
    exit 1
fi

log_step "3.3 验证 Android PodSandboxStatus 路由"
ANDROID_UID=$(kubectl get pod "${ANDROID_POD}" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
ANDROID_STATUS=$(${CRICTL} inspectp "${ANDROID_UID}" 2>&1 || true)
if [ -n "${ANDROID_UID}" ] &&
   echo "${ANDROID_STATUS}" | grep -q '"android.dev/adb-url"' &&
   echo "${ANDROID_STATUS}" | grep -q '"android.dev/cvd-state"'; then
    log_pass "Android PodSandboxStatus 已按 sandboxID 路由到 Android engine"
else
    log_fail "Android PodSandboxStatus 路由或 annotations 异常: uid=${ANDROID_UID} output=${ANDROID_STATUS}"
    exit 1
fi

log_step "4.1 验证 Exec 按 containerID 路由"
assert_kubectl_exec_output "${DEFAULT_POD}" "default_exec_ok" echo "default_exec_ok" || exit 1
assert_kubectl_exec_output "${E2B_POD}" "e2b_exec_ok" echo "e2b_exec_ok" || exit 1

log_step "4.2 验证 Attach 按 containerID 路由"
assert_attach_output "${E2B_POD}" "e2b_attach_ok" || exit 1

log_step "5.1 清理资源"
cleanup
log_pass "资源删除请求已提交"

print_summary
[ "${FAIL_COUNT}" -eq 0 ]

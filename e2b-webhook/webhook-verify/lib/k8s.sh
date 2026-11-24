#!/bin/bash
###############################################################################
# k8s.sh — 集群探测、fixture 渲染、等待与证据收集
#
# 只封装 kubectl，不含任何 webhook 业务断言（那属于 lib/admission.sh）。
###############################################################################
set -euo pipefail

#==================== 探测 ====================#
k8s_cluster_reachable() {
    run_with_timeout 20 kubectl cluster-info >/dev/null 2>&1
}

k8s_ready_nodes() {
    kubectl get nodes --no-headers 2>/dev/null | awk '$2 == "Ready" {c++} END {print c+0}'
}

k8s_ns_exists() {
    kubectl get namespace "$1" >/dev/null 2>&1
}

k8s_ensure_ns() {
    local ns="$1"
    if k8s_ns_exists "${ns}"; then
        return 0
    fi
    kubectl create namespace "${ns}" >/dev/null 2>&1
    log_info "已创建 namespace ${ns}"
}

k8s_crd_exists() {
    kubectl get crd "$1" >/dev/null 2>&1
}

k8s_runtimeclass_exists() {
    kubectl get runtimeclass "$1" >/dev/null 2>&1
}

k8s_node_internal_ip() {
    local index="${1:-0}"
    kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' 2>/dev/null \
        | sed -n "$((index + 1))p"
}

k8s_context() {
    kubectl config current-context 2>/dev/null || true
}

#==================== 资源操作 ====================#
# k8s_apply <file>：返回 apply 输出（stderr 由调用方决定）。
k8s_apply() {
    kubectl apply -f "$1"
}

# k8s_server_dry_run <file>：只走 apiserver 的准入 + 校验，不落盘，输出对象 JSON。
k8s_server_dry_run() {
    kubectl create --dry-run=server -o json -f "$1"
}

k8s_delete_file() {
    kubectl delete -f "$1" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
}

k8s_delete_pod() {
    local ns="$1" name="$2"
    kubectl -n "${ns}" delete pod "${name}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
}

#==================== 等待 ====================#
k8s_wait_pod_ready() {
    local ns="$1" name="$2" timeout="${3:-${WV_E2E_TIMEOUT}}"
    kubectl -n "${ns}" wait --for=condition=Ready "pod/${name}" --timeout="${timeout}s" >/dev/null 2>&1
}

k8s_wait_pod_gone() {
    local ns="$1" name="$2" timeout="${3:-120}"
    local deadline=$(( $(date +%s) + timeout ))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! kubectl -n "${ns}" get pod "${name}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

k8s_wait_deployment_ready() {
    local ns="$1" name="$2" replicas="$3" timeout="${4:-300}"
    kubectl -n "${ns}" wait --for=condition=Available "deployment/${name}" --timeout="${timeout}s" >/dev/null 2>&1 || return 1
    local ready
    ready=$(kubectl -n "${ns}" get deployment "${name}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    [ "${ready:-0}" -ge "${replicas}" ]
}

#==================== 读取 ====================#
k8s_pod_exists() {
    kubectl -n "$1" get pod "$2" >/dev/null 2>&1
}

k8s_pod_json() {
    kubectl -n "$1" get pod "$2" -o json 2>/dev/null
}

k8s_pod_uid() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.metadata.uid}' 2>/dev/null || true
}

# 该 Pod 对应的 CRI sandbox id（crictl 的 pod labels 里有 pod 名/命名空间）。
# 用它判断“Pod 是否被重建”，比统计全节点 sandbox 总数更精确——
# 全节点计数会被其它用例/后台回收的沙箱带动，属于噪声。
k8s_cri_sandbox_id() {
    local ns="$1" pod="$2"
    crictl --runtime-endpoint "unix://${WV_CRI_SOCKET}" pods -o json 2>/dev/null \
        | jq -r --arg ns "${ns}" --arg pod "${pod}" \
            '.items[]? | select(.labels["io.kubernetes.pod.namespace"] == $ns and .labels["io.kubernetes.pod.name"] == $pod) | .id' 2>/dev/null \
        | awk 'NR==1' || true
}

k8s_pod_annotation() {
    # 注解 key 里含 "."（如 e2b.dev/template-id）时 jsonpath 的下标写法取不到值，
    # 用 jq 按字符串 key 精确读取。
    kubectl -n "$1" get pod "$2" -o json 2>/dev/null \
        | jq -r --arg k "$3" '.metadata.annotations[$k] // ""' 2>/dev/null || true
}

k8s_pod_annotations_json() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.metadata.annotations}' 2>/dev/null || printf '{}'
}

k8s_pod_runtimeclass() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null || true
}

k8s_pod_phase() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null || true
}

k8s_pod_node() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true
}

k8s_pod_ip() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.status.podIP}' 2>/dev/null || true
}

k8s_pod_scheduled() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].status}' 2>/dev/null || true
}

k8s_pod_ready() {
    kubectl -n "$1" get pod "$2" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

k8s_pod_list_by_label() {
    local ns="$1" selector="$2"
    kubectl -n "${ns}" get pods -l "${selector}" -o name 2>/dev/null | sed 's|^pod/||'
}

k8s_pod_count_by_label() {
    local ns="$1" selector="$2"
    kubectl -n "${ns}" get pods -l "${selector}" --no-headers 2>/dev/null | wc -l | tr -d ' '
}

#==================== 证据收集 ====================#
# 失败时把 describe / events / yaml / logs 落到 artifacts，便于事后归因。
k8s_collect_pod_artifacts() {
    local ns="$1" name="$2" tag="${3:-pod}"
    local dir="${ARTIFACT_DIR}/${tag}"
    mkdir -p "${dir}"
    kubectl -n "${ns}" describe pod "${name}" > "${dir}/describe.txt" 2>&1 || true
    kubectl -n "${ns}" get pod "${name}" -o yaml > "${dir}/pod.yaml" 2>&1 || true
    kubectl -n "${ns}" get events --field-selector "involvedObject.name=${name}" \
        > "${dir}/events.txt" 2>&1 || true
    kubectl -n "${ns}" logs "${name}" --all-containers=true --tail=500 \
        > "${dir}/logs.txt" 2>&1 || true
    printf '%s' "${dir}"
}

k8s_collect_webhook_artifacts() {
    local tag="${1:-webhook}"
    local dir="${ARTIFACT_DIR}/${tag}"
    mkdir -p "${dir}"
    kubectl -n "${WV_NAMESPACE}" get deploy,rs,pods -l "app.kubernetes.io/name=${WV_DEPLOYMENT}" -o wide \
        > "${dir}/deployment.txt" 2>&1 || true
    kubectl -n "${WV_NAMESPACE}" describe deploy "${WV_DEPLOYMENT}" > "${dir}/describe.txt" 2>&1 || true
    kubectl -n "${WV_NAMESPACE}" logs "deploy/${WV_DEPLOYMENT}" --all-containers=true --tail=500 \
        > "${dir}/logs.txt" 2>&1 || true
    kubectl -n "${WV_NAMESPACE}" get events --field-selector "involvedObject.name=${WV_DEPLOYMENT}" \
        > "${dir}/events.txt" 2>&1 || true
    printf '%s' "${dir}"
}

#==================== fixture 渲染 ====================#
# k8s_render <模板> <输出> KEY=VALUE...
#
# 模板占位符写作 __KEY__，值可含换行（YAML 片段）。渲染后删除空行，
# 这样“空 annotations / 空 runtimeClassName”对应的整块内容会自然消失，
# 不会留下 `annotations:` 这种半截 key。
k8s_render() {
    local template="$1" out="$2"; shift 2
    python3 - "${template}" "${out}" "$@" <<'PY'
import sys

template, out = sys.argv[1], sys.argv[2]
text = open(template, encoding="utf-8").read()
for pair in sys.argv[3:]:
    key, _, value = pair.partition("=")
    text = text.replace("__%s__" % key, value)

lines = [line.rstrip() for line in text.splitlines()]
lines = [line for line in lines if line.strip()]
with open(out, "w", encoding="utf-8") as handle:
    handle.write("\n".join(lines) + "\n")
PY
}

# 生成 YAML 片段：缩进 + 每行内容（值为空时返回空串，渲染时整块消失）。
yaml_block() {
    local indent="$1"; shift
    local content="$*"
    [ -z "${content}" ] && return 0
    printf '%s' "${content}" | sed "s/^/${indent}/"
}

#==================== 集群内探针 ====================#
# k8s_probe_run <描述> <shell 命令>
#
# 在测试 namespace 起一个一次性 Pod 跑命令，返回其 stdout。
# 用途：验证“Pod 网络里能不能访问 mock/envd”，宿主机网络不可靠时用。
k8s_probe_run() {
    local desc="$1" cmd="$2"
    local image="${WV_PROBE_IMAGE:-}"
    if [ -z "${image}" ]; then
        image="$(webhook_registry_url 2>/dev/null || true)"
        image="${image:+${image}/busybox:latest}"
        image="${image:-busybox:latest}"
    fi

    local name="wv-probe-$(date +%s)-$RANDOM"
    k8s_ensure_ns "${WV_TEST_NS}" || true

    if ! run_with_timeout 60 kubectl run "${name}" -n "${WV_TEST_NS}" \
        --image="${image}" --image-pull-policy=IfNotPresent --restart=Never \
        --command -- sh -c "${cmd}" >"${ARTIFACT_DIR}/probe-run.log" 2>&1; then
        log_warn "探针 Pod 启动失败（${desc}，image=${image}）: $(tail -3 "${ARTIFACT_DIR}/probe-run.log" 2>/dev/null | tr '\n' ' ')"
        kubectl -n "${WV_TEST_NS}" delete pod "${name}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
        return 1
    fi

    local deadline=$(( $(date +%s) + 60 ))
    local phase=""
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        phase=$(kubectl -n "${WV_TEST_NS}" get pod "${name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        case "${phase}" in
            Succeeded|Failed) break ;;
        esac
        sleep 1
    done

    local output
    output=$(kubectl -n "${WV_TEST_NS}" logs "${name}" 2>/dev/null || true)
    kubectl -n "${WV_TEST_NS}" delete pod "${name}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true

    if [ "${phase}" != "Succeeded" ]; then
        log_warn "探针 Pod 未成功（${desc}，phase=${phase}）"
        return 1
    fi
    printf '%s' "${output}"
}

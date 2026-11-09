#!/bin/bash
###############################################################################
# 05_rollback_cleanup.sh — 回滚与清理
#
# 用例：
#   Z1 恢复旧镜像（set image 回基线）
#   Z2 恢复副本数
#   Z3 恢复 deploy/.env 与运行期 E2B_API_URL
#   Z4 删除测试资源（Pod/CR/namespace）
#   Z5 清理辅助进程（port-forward/mock）；--cleanup-fresh 删除本轮新部署
#   Z6 最终一致性（服务端证书与 caBundle、/healthz、副本）
#
# 参数：
#   --emergency   run_all 在异常退出时调用：尽量恢复，不因集群异常而失败
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
. "${SCRIPT_DIR}/lib/k8s.sh"
. "${SCRIPT_DIR}/lib/webhook.sh"
. "${SCRIPT_DIR}/lib/admission.sh"
. "${SCRIPT_DIR}/lib/provider.sh"

EMERGENCY=0
while [ $# -gt 0 ]; do
    case "$1" in
        --emergency) EMERGENCY=1 ;;
        --help|-h) sed -n '2,15p' "${BASH_SOURCE[0]}"; exit 0 ;;
        *) log_error "未知参数: $1"; exit 1 ;;
    esac
    shift
done

log_section "05 回滚与清理"

WORK="${WV_TMP}/05"
mkdir -p "${WORK}"

# 应急模式下 state.env 可能不是本进程写的，但文件是共享的，直接读即可。
CLUSTER_OK=0
k8s_cluster_reachable && CLUSTER_OK=1
if [ "${CLUSTER_OK}" != "1" ]; then
    log_warn "集群不可达，Z1/Z2/Z6 只能 SKIP"
fi

#==================== Z1 恢复旧镜像 ====================#
log_step "Z1 恢复旧镜像"
old_image=$(state_get BASELINE_OLD_IMAGE)
[ -n "${old_image}" ] || old_image=$(cat "${BASELINE_DIR}/old_image" 2>/dev/null || true)
# 注意：DEPLOY_IMAGE 是“本轮开始前”的值，不能用来判断当前是否已恢复，
# 必须读集群里的实时镜像。
current_image=$(kubectl -n "${WV_NAMESPACE}" get deploy "${WV_DEPLOYMENT}" \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || true)

if [ "${WV_RESTORE_ON_EXIT}" != "1" ]; then
    skip Z1 "WV_RESTORE_ON_EXIT=0，保留当前镜像"
elif [ "${CLUSTER_OK}" != "1" ]; then
    skip Z1 "集群不可达"
elif [ -z "${old_image}" ]; then
    skip Z1 "无旧镜像基线（本轮为首次部署）"
else
    z1_ok=1
    if [ "${current_image}" != "${old_image}" ]; then
        webhook_restore_image || z1_ok=0
    fi
    if [ "${z1_ok}" = "1" ]; then
        # 不能只看 set image 的返回码：要确认所有 Pod 实际跑的就是旧镜像。
        bad=0
        pods=0
        while IFS='|' read -r pod image image_id; do
            [ -z "${pod}" ] && continue
            pods=$((pods + 1))
            [ "${image}" = "${old_image}" ] || { bad=$((bad + 1)); log_warn "${pod} 仍运行 ${image}"; }
        done <<< "$(webhook_pod_images)"
        ready=$(webhook_ready_replicas)
        if [ "${bad}" = "0" ] && [ "${pods}" -ge 1 ] && [ "${ready:-0}" -ge 1 ]; then
            pass Z1 "已恢复旧镜像 ${old_image}（${pods} 个 Pod 镜像一致，ready=${ready}）"
        else
            fail Z1 "镜像未完全恢复：${bad} 个 Pod 不是 ${old_image}，pods=${pods} ready=${ready}"
            k8s_collect_webhook_artifacts "z1-webhook" >/dev/null 2>&1 || true
        fi
    else
        fail Z1 "恢复旧镜像失败（${old_image}）"
        k8s_collect_webhook_artifacts "z1-webhook" >/dev/null 2>&1 || true
    fi
fi

#==================== Z2 恢复副本数 ====================#
log_step "Z2 恢复副本数"
old_replicas=$(state_get BASELINE_OLD_REPLICAS)
[ -n "${old_replicas}" ] || old_replicas=$(cat "${BASELINE_DIR}/old_replicas" 2>/dev/null || true)

if [ "${WV_RESTORE_ON_EXIT}" != "1" ]; then
    skip Z2 "WV_RESTORE_ON_EXIT=0，保留当前副本数"
elif [ "${CLUSTER_OK}" != "1" ]; then
    skip Z2 "集群不可达"
elif [ -z "${old_replicas}" ]; then
    skip Z2 "无副本数基线"
else
    if [ "$(webhook_desired_replicas)" != "${old_replicas}" ]; then
        webhook_restore_replicas || log_warn "恢复副本数命令失败，继续核对最终状态"
    fi
    # 恢复动作是否真的生效，要看 spec/Ready/Endpoints 三个实际状态。
    desired=$(webhook_desired_replicas)
    ready=$(webhook_ready_replicas)
    endpoints=$(webhook_endpoint_count)
    if [ "${desired}" = "${old_replicas}" ] && [ "${ready:-0}" -ge "${old_replicas}" ] \
        && [ "${endpoints:-0}" -ge "${old_replicas}" ]; then
        pass Z2 "副本数已恢复 ${old_replicas}（ready=${ready}，endpoints=${endpoints}）"
    else
        fail Z2 "副本数未恢复：desired=${desired} ready=${ready} endpoints=${endpoints}，期望 ${old_replicas}"
    fi
fi

#==================== Z3 恢复 .env 与运行期 env ====================#
log_step "Z3 恢复 deploy/.env 与 env 注入"
if [ "${WV_RESTORE_ON_EXIT}" != "1" ]; then
    skip Z3 "WV_RESTORE_ON_EXIT=0，保留 .env"
else
    z3_ok=1

    bak=$(state_get BASELINE_ENV)
    if [ -n "${bak}" ] && [ -f "${bak}" ]; then
        if ! diff -q "${bak}" "${REPO_ROOT}/deploy/.env" >/dev/null 2>&1; then
            webhook_restore_env_file || z3_ok=0
        fi
        remaining=$(diff "${bak}" "${REPO_ROOT}/deploy/.env" 2>/dev/null | wc -l | tr -d ' ' || true)
        [ "${remaining}" = "0" ] || z3_ok=0
    fi

    if [ "${CLUSTER_OK}" = "1" ] && [ "$(state_get DEPLOY_EXISTS)" = "1" ]; then
        stock_url=$(state_get STOCK_API_URL)
        live_url=$(kubectl -n "${WV_NAMESPACE}" get deploy "${WV_DEPLOYMENT}" \
            -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="E2B_API_URL")].value}' 2>/dev/null || true)
        if [ -n "${stock_url}" ] && [ "${live_url}" != "${stock_url}" ]; then
            webhook_restore_stock_env || z3_ok=0
            live_after=$(kubectl -n "${WV_NAMESPACE}" get deploy "${WV_DEPLOYMENT}" \
                -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="E2B_API_URL")].value}' 2>/dev/null || true)
            [ "${live_after}" = "${stock_url}" ] || z3_ok=0
        fi
    fi

    if [ "${z3_ok}" = "1" ]; then
        pass Z3 "deploy/.env 与 E2B_API_URL 均已还原"
    else
        fail Z3 "还原不完整（deploy/.env 或 E2B_API_URL）"
    fi
fi

#==================== Z4 删除测试资源 ====================#
log_step "Z4 删除测试资源"
if [ "${WV_KEEP}" = "1" ]; then
    skip Z4 "WV_KEEP=1，保留测试资源"
elif [ "${CLUSTER_OK}" != "1" ]; then
    skip Z4 "集群不可达"
else
    k8s_delete_pod "${WV_TEST_NS}" "${WV_E2E_POD}"
    for f in "${WV_TMP}/04/batchsandbox.yaml" "${WV_TMP}/03/bs.yaml"; do
        if [ -f "${f}" ]; then
            k8s_delete_file "${f}"
        fi
    done

    pods_left=$(kubectl -n "${WV_TEST_NS}" get pods --no-headers 2>/dev/null | wc -l | tr -d ' ' || true)
    if [ "${pods_left:-0}" -eq 0 ]; then
        kubectl delete namespace "${WV_TEST_NS}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
        pass Z4 "测试 Pod/CR 已删除，namespace ${WV_TEST_NS} 已回收"
    else
        fail Z4 "仍有 ${pods_left} 个 Pod 残留在 ${WV_TEST_NS}"
    fi
fi

#==================== Z5 清理辅助进程 / 本轮新部署 ====================#
log_step "Z5 清理辅助进程"
# 先记下 pid：stop 函数会删 pid 文件，之后再看文件就永远“干净”了。
pf_pid=$(cat "${WV_TMP}/port-forward.pid" 2>/dev/null || true)
mock_pid=$(cat "${WV_TMP}/mock_api.pid" 2>/dev/null || true)
admission_stop_port_forward
mock_api_stop

stale=0
for pid in "${pf_pid}" "${mock_pid}"; do
    if [ -n "${pid}" ] && kill -0 "${pid}" 2>/dev/null; then
        stale=$((stale + 1))
        log_warn "辅助进程 ${pid} 仍存活"
    fi
done

# 端口真正关闭才算清理完成（pid 文件被删并不意味着进程已退出）。
port_busy=1
for _ in 1 2 3 4 5; do
    port_busy=0
    if port_listening "${WV_PORT}"; then port_busy=1; fi
    if port_listening "${WV_MOCK_PORT}"; then port_busy=1; fi
    [ "${port_busy}" = "0" ] && break
    sleep 0.3
done
if [ "${port_busy}" = "1" ]; then
    stale=$((stale + 1))
    log_warn "端口 ${WV_PORT}/${WV_MOCK_PORT} 仍有监听"
fi
rm -f "${WV_TMP}/port-forward.pid" "${WV_TMP}/mock_api.pid"

fresh=0
if [ "${WV_CLEANUP_FRESH:-0}" = "1" ] && [ "$(state_get FRESH_DEPLOY)" = "1" ] && [ "${CLUSTER_OK}" = "1" ]; then
    log_warn "--cleanup-fresh：删除本轮新部署的 webhook 资源"
    kubectl -n "${WV_NAMESPACE}" delete deploy "${WV_DEPLOYMENT}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    kubectl -n "${WV_NAMESPACE}" delete svc "${WV_SERVICE}" --ignore-not-found=true --wait=false >/dev/null 2>&1 || true
    kubectl delete mutatingwebhookconfiguration "${WV_MWC}" --ignore-not-found=true >/dev/null 2>&1 || true
    kubectl -n "${WV_NAMESPACE}" delete secret "${WV_TLS_SECRET}" "${WV_APIKEY_SECRET}" --ignore-not-found=true >/dev/null 2>&1 || true
    fresh=1
fi

if [ "${stale}" != "0" ]; then
    fail Z5 "仍有 ${stale} 项辅助进程/端口残留"
elif [ "${fresh}" = "1" ]; then
    pass Z5 "辅助进程已停止且端口已关闭，本轮新部署已删除"
else
    pass Z5 "port-forward / mock 均已停止且端口已关闭"
fi

#==================== Z6 最终一致性 ====================#
log_step "Z6 最终一致性"
if [ "${CLUSTER_OK}" != "1" ]; then
    skip Z6 "集群不可达"
elif [ "$(state_get MWC_EXISTS)" != "1" ] || [ "${WV_CLEANUP_FRESH:-0}" = "1" ]; then
    skip Z6 "webhook 资源已删除或不完整，跳过一致性检查"
else
    cert_sha=$(webhook_cert_sha256 2>/dev/null || true)
    cb0=$(webhook_cabundle_sha256 0 2>/dev/null || true)
    ready=$(webhook_ready_replicas)
    desired=$(webhook_desired_replicas)
    healthz=""
    served_sha=""
    if admission_ensure_port_forward >/dev/null 2>&1; then
        healthz=$(admission_health_code)
        served_sha=$(webhook_served_cert_sha256 2>/dev/null || true)
        admission_stop_port_forward
    fi

    if [ -n "${cert_sha}" ] && [ "${cert_sha}" = "${cb0}" ] \
        && [ -n "${served_sha}" ] && [ "${served_sha}" = "${cb0}" ] \
        && [ "${healthz}" = "200" ] && [ "${ready:-0}" -ge "${desired:-1}" ]; then
        pass Z6 "Secret/服务端实际证书与 caBundle 一致，/healthz 200，${ready}/${desired} 副本 Ready"
    else
        fail Z6 "cert=${cert_sha:0:12} served=${served_sha:0:12} caBundle=${cb0:0:12} healthz=${healthz} ready=${ready}/${desired}"
        k8s_collect_webhook_artifacts "z6-webhook" >/dev/null 2>&1 || true
    fi
fi

#==================== 收尾 ====================#
if [ "${EMERGENCY}" = "1" ]; then
    log_warn "本轮为应急恢复（--emergency），结果仅供参考"
fi

finish "05 回滚与清理"

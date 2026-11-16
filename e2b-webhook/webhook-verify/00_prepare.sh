#!/bin/bash
###############################################################################
# 00_prepare.sh — 环境检查、构建/部署/替换、基线
#
# 用例：
#   P1 工具齐全（kubectl/curl/jq/python3/openssl，部署时另需 docker）
#   P2 集群可达（至少 1 个 Ready 节点）
#   P3 webhook 现状 + 依赖一致性（Deployment/Service/MWC/Secret/Endpoints）
#   P4 构建镜像（唯一 tag；构建前删旧二进制，避免 build.sh 跳过编译）
#   P5 推送镜像
#   P6 部署或替换（未部署走 deploy.sh，已部署只 set image）
#   P7 确实换了新镜像（spec.image 为新 tag，且 imageID 与基线不同）
#   P8 基线已保存 + /healthz 200
#   P9 单元测试（go vet / build / test，可跳）
#
# 参数：
#   --skip-deploy   已部署，只探测 + 保存基线（不构建不重启）
#   --check-only    只做 P1/P2/P3/P8
#   --deploy-only   做到 P8 就结束（不跑 P9）
#   --skip-unit     跳过 P9
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
. "${SCRIPT_DIR}/lib/k8s.sh"
. "${SCRIPT_DIR}/lib/webhook.sh"
. "${SCRIPT_DIR}/lib/admission.sh"
. "${SCRIPT_DIR}/lib/provider.sh"

SKIP_DEPLOY=0
CHECK_ONLY=0
DEPLOY_ONLY=0

while [ $# -gt 0 ]; do
    case "$1" in
        --skip-deploy) SKIP_DEPLOY=1 ;;
        --check-only) CHECK_ONLY=1; SKIP_DEPLOY=1 ;;
        --deploy-only) DEPLOY_ONLY=1 ;;
        --skip-unit) WV_UNIT=0 ;;
        --help|-h)
            sed -n '2,21p' "${BASH_SOURCE[0]}"
            exit 0
            ;;
        *) log_error "未知参数: $1"; exit 1 ;;
    esac
    shift
done

log_section "00 环境检查 / 部署 / 基线"

#==================== P1 工具齐全 ====================#
log_step "P1 工具检查"
missing=()
for cmd in kubectl curl jq python3 openssl; do
    need_cmd "${cmd}" || missing+=("${cmd}")
done
# 只有在需要构建/部署时才要求 docker。
if [ "${SKIP_DEPLOY}" = "0" ]; then
    need_cmd docker || missing+=("docker")
fi
if [ ${#missing[@]} -eq 0 ]; then
    tool_desc="kubectl / curl / jq / python3 / openssl"
    if [ "${SKIP_DEPLOY}" = "0" ]; then
        tool_desc="${tool_desc} / docker"
    fi
    pass P1 "${tool_desc} 均可用"
else
    fail P1 "缺少命令: ${missing[*]}"
fi

#==================== P2 集群可达 ====================#
log_step "P2 集群可达性"
if ! k8s_cluster_reachable; then
    fail P2 "kubectl cluster-info 失败（KUBECONFIG=${KUBECONFIG}）"
    finish "00 环境检查"
fi
ready_nodes=$(k8s_ready_nodes)
if [ "${ready_nodes:-0}" -ge 1 ]; then
    pass P2 "集群可达（context=$(k8s_context)，Ready 节点 ${ready_nodes} 个）"
else
    fail P2 "集群可达但没有 Ready 节点"
fi

#==================== P3 webhook 现状 ====================#
log_step "P3 webhook 现状"
webhook_detect
state_set WV_TMP "${WV_TMP}"

# 基线必须在任何改动之前落盘（镜像、副本、.env、caBundle）。
webhook_backup_env_file >/dev/null 2>&1 || true
if [ "$(state_get DEPLOY_EXISTS)" = "1" ]; then
    webhook_save_baseline
    state_set BASELINE_SAVED 1
else
    state_set BASELINE_SAVED 0
fi

if [ "$(state_get DEPLOY_EXISTS)" != "1" ] && [ "${SKIP_DEPLOY}" = "1" ]; then
    unsupported P3 "webhook 未部署且指定了 --skip-deploy，后续用例无法执行"
    print_summary "00 环境检查"
    exit "$(summary_exit_code)"
fi

if [ "$(state_get DEPLOY_EXISTS)" = "1" ]; then
    # “已部署”不能只看 Deployment：Service/MWC/TLS Secret 缺任何一个，
    # 准入都不会真正生效，必须在这里暴露出来。
    missing_deps=""
    [ "$(state_get SVC_EXISTS)" = "1" ] || missing_deps="${missing_deps} service"
    [ "$(state_get MWC_EXISTS)" = "1" ] || missing_deps="${missing_deps} mwc"
    [ "$(state_get TLS_SECRET_EXISTS)" = "1" ] || missing_deps="${missing_deps} tls-secret"
    if [ -z "${missing_deps}" ]; then
        pass P3 "已部署且依赖齐全: image=$(state_get DEPLOY_IMAGE) replicas=$(state_get DEPLOY_REPLICAS) ready=$(state_get DEPLOY_READY) endpoints=$(state_get ENDPOINT_COUNT)"
    else
        fail P3 "Deployment 存在但依赖缺失:${missing_deps}（image=$(state_get DEPLOY_IMAGE) ready=$(state_get DEPLOY_READY)）"
    fi
else
    pass P3 "未部署（svc=$(state_get SVC_EXISTS) mwc=$(state_get MWC_EXISTS) tls=$(state_get TLS_SECRET_EXISTS)），本轮将完整部署"
fi

#==================== check-only 提前收尾 ====================#
if [ "${CHECK_ONLY}" = "1" ]; then
    log_step "P8 基线 + 健康检查（--check-only）"

    healthz_ok=0
    if admission_ensure_port_forward; then
        if [ "$(admission_health_code)" = "200" ]; then
            healthz_ok=1
        fi
        admission_stop_port_forward
    fi

    baseline_ok=0
    webhook_baseline_ok && baseline_ok=1

    if [ "${baseline_ok}" = "1" ] && [ "${healthz_ok}" = "1" ]; then
        pass P8 "基线完整（old_image=$(cat "${BASELINE_DIR}/old_image" 2>/dev/null)）+ /healthz 200"
    elif [ "${baseline_ok}" = "0" ] && [ "$(state_get DEPLOY_EXISTS)" != "1" ] && [ "${healthz_ok}" = "0" ]; then
        skip P8 "未部署，没有基线，也没有 /healthz"
    elif [ "${healthz_ok}" = "1" ]; then
        fail P8 "基线文件不完整（${BASELINE_DIR}）"
    else
        fail P8 "/healthz 不可用，基线虽在但实例异常"
    fi

    finish "00 环境检查（--check-only）"
fi

#==================== P4 构建镜像 ====================#
if [ "${SKIP_DEPLOY}" = "1" ]; then
    skip P4 "跳过构建（--skip-deploy）"
    skip P5 "跳过推送（--skip-deploy）"
    skip P6 "跳过部署/替换（--skip-deploy）"
    skip P7 "跳过镜像核对（--skip-deploy）"
else
    log_step "P4 构建镜像（唯一 tag: ${WV_IMAGE_TAG}）"
    build_start=$(now_ms)
    if ref=$(webhook_build_image); then
        binary_size=$(ls -lh "${REPO_ROOT}/bin/$(webhook_goarch)/e2b-webhook" 2>/dev/null | awk '{print $5}')
        pass P4 "构建成功: ${ref}（$(format_ms $(( $(now_ms) - build_start )))，二进制 ${binary_size:-未知}）"
    else
        fail P4 "镜像构建失败（日志 ${LOG_DIR}/build-image.log）"
        finish "00 部署失败"
    fi

    #==================== P5 推送镜像 ====================#
    log_step "P5 推送镜像"
    if webhook_push_image; then
        pass P5 "已推送 ${ref}"
    else
        fail P5 "推送失败（日志 ${LOG_DIR}/push-image.log）"
        finish "00 部署失败"
    fi

    #==================== P6 部署或替换 ====================#
    log_step "P6 部署或替换"
    if [ "$(state_get DEPLOY_EXISTS)" = "1" ]; then
        # 已部署：只换镜像。完整 deploy-webhook 会重建 TLS Secret 与 caBundle，
        # 滚动窗口内证书与 caBundle 不匹配，会产生与代码无关的假失败。
        log_info "实例已存在，仅 set image（保留证书与 caBundle）"
        if webhook_set_image "${ref}" && webhook_rollout_status 300; then
            pass P6 "set image 完成并 rollout 成功"
        else
            fail P6 "set image / rollout 失败"
            k8s_collect_webhook_artifacts >/dev/null 2>&1 || true
        fi
    else
        log_info "实例不存在，走 deploy.sh deploy-webhook（含证书、api-key Secret、MWC）"
        if webhook_deploy_fresh && webhook_set_image "${ref}" && webhook_rollout_status 300; then
            pass P6 "deploy-webhook 完成并已切到唯一 tag"
        else
            fail P6 "部署失败（日志 ${LOG_DIR}/deploy-webhook.log）"
            k8s_collect_webhook_artifacts >/dev/null 2>&1 || true
        fi
    fi

    #==================== P7 确实换了新镜像 ====================#
    log_step "P7 核对镜像"
    desired=$(webhook_desired_replicas)
    ready=$(webhook_ready_replicas)
    pod_count=$(webhook_pod_count)
    images=$(webhook_pod_images)
    bad_image=0
    bad_digest=0
    old_image_id=$(state_get BASELINE_OLD_IMAGE_ID)
    built_digest=$(webhook_built_image_digest 2>/dev/null || true)
    while IFS='|' read -r pod image image_id; do
        [ -z "${pod}" ] && continue
        if [ "${image}" != "${ref}" ]; then
            bad_image=$((bad_image + 1))
            log_warn "${pod} 镜像为 ${image}，期望 ${ref}"
        fi
        # 判定“跑的就是刚构建的镜像”看 registry manifest digest，
        # 而不是“与旧镜像不同”：同一份代码重编出来的摘要本来就可能相同。
        if [ -n "${built_digest}" ] && [ "${image_id##*@}" != "${built_digest}" ]; then
            bad_digest=$((bad_digest + 1))
            log_warn "${pod} 的 imageID=${image_id}，构建产物摘要是 ${built_digest}"
        fi
    done <<< "${images}"

    # desired=0 只可能是 Deployment 不存在（kubectl 取不到 spec.replicas 时的兜底），
    # 这种情况必须判失败，否则“0 个副本”会被误判成全量更新。
    if [ "${desired:-0}" -lt 1 ] || [ "${pod_count:-0}" -lt "${desired:-0}" ]; then
        fail P7 "Deployment 不存在或 Pod 不足（desired=${desired:-0} pods=${pod_count:-0} ready=${ready:-0}）"
        k8s_collect_webhook_artifacts >/dev/null 2>&1 || true
    elif [ -z "${built_digest}" ]; then
        fail P7 "拿不到构建产物摘要（docker image inspect ${ref} 的 RepoDigests 为空）"
    elif [ "${bad_image}" -eq 0 ] && [ "${bad_digest}" -eq 0 ] && [ "${ready:-0}" -ge "${desired:-1}" ]; then
        if [ -n "${old_image_id}" ] && [ "${old_image_id##*@}" = "${built_digest}" ]; then
            pass P7 "全部 ${ready} 个副本运行构建产物 ${ref}（digest ${built_digest:0:19}…，构建可复现，与旧镜像内容一致）"
        else
            pass P7 "全部 ${ready} 个副本运行构建产物 ${ref}（digest ${built_digest:0:19}…，已不同于基线）"
        fi
    else
        fail P7 "镜像未完全更新（tag 不符 ${bad_image} 个，digest 不符 ${bad_digest} 个，ready ${ready}/${desired}）"
        k8s_collect_webhook_artifacts >/dev/null 2>&1 || true
    fi
fi

#==================== P8 基线已保存 ====================#
log_step "P8 基线 + 健康检查"
healthz_ok=0
if admission_ensure_port_forward; then
    [ "$(admission_health_code)" = "200" ] && healthz_ok=1
    admission_stop_port_forward
fi

baseline_ok=0
webhook_baseline_ok && baseline_ok=1

if [ "${baseline_ok}" = "1" ] && [ "${healthz_ok}" = "1" ]; then
    pass P8 "基线完整（old_image=$(cat "${BASELINE_DIR}/old_image" 2>/dev/null) replicas=$(cat "${BASELINE_DIR}/old_replicas" 2>/dev/null)）+ /healthz 200"
elif [ "${baseline_ok}" = "0" ] && [ "$(state_get FRESH_DEPLOY)" = "1" ] && [ "${healthz_ok}" = "1" ]; then
    # 本轮是首次部署：开始时没有 Deployment，因此没有旧镜像/副本可存基线。
    # 不能用 DEPLOY_EXISTS 判断——部署成功后它已经被 webhook_detect 改成 1。
    pass P8 "首次部署（无旧镜像可回滚）+ /healthz 200"
elif [ "${healthz_ok}" = "1" ]; then
    fail P8 "基线文件不完整（${BASELINE_DIR}）"
else
    fail P8 "/healthz 不可用"
    k8s_collect_webhook_artifacts >/dev/null 2>&1 || true
fi

#==================== 测试资源 ====================#
if k8s_ensure_ns "${WV_TEST_NS}"; then
    log_info "测试 namespace 就绪: ${WV_TEST_NS}"
else
    log_warn "测试 namespace ${WV_TEST_NS} 创建失败"
fi

#==================== P9 单元测试 ====================#
if [ "${DEPLOY_ONLY}" = "1" ]; then
    skip P9 "跳过单元测试（--deploy-only）"
elif [ "${WV_UNIT}" != "1" ]; then
    skip P9 "跳过单元测试（--skip-unit / WV_UNIT=0）"
elif ! need_cmd go; then
    skip P9 "未安装 go，跳过单元测试"
else
    log_step "P9 单元测试（go vet / build / test）"
    unit_log="${LOG_DIR}/unit-tests.log"
    unit_ok=1
    (
        cd "${REPO_ROOT}/e2b-webhook"
        export GOWORK=off
        go vet ./... && go build ./... && go test ./...
    ) >"${unit_log}" 2>&1 || unit_ok=0

    if [ "${unit_ok}" = "1" ]; then
        pass P9 "go vet / go build / go test 全部通过"
    else
        fail P9 "单元测试失败（日志 ${unit_log}）"
        tail -20 "${unit_log}" >&2 || true
    fi
fi

finish "00 环境检查 / 部署 / 基线"

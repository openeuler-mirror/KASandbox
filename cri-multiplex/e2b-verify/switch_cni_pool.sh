#!/bin/bash
###############################################################################
# switch_cni_pool.sh — 切换 cri-multiplex 的 CNI 预热池模式
#
# 用法：
#   bash switch_cni_pool.sh on  [size]   # 开启预热池（默认容量 200）
#   bash switch_cni_pool.sh off          # 关闭预热池
#   bash switch_cni_pool.sh status       # 查看当前模式
#
# 切换会强制重启 cri-multiplex（保留 hide-sandbox-label 等既有参数），
# 启动时自动清理上一轮遗留的预热池 entry。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

mode="${1:-}"
size="${2:-200}"

case "${mode}" in
    on)
        if ! [[ "${size}" =~ ^[0-9]+$ ]] || [ "${size}" -le 0 ]; then
            echo "用法: bash switch_cni_pool.sh on [size>0]" >&2
            exit 1
        fi
        export CNI_POOL_ENABLED=1
        export CNI_POOL_SIZE="${size}"
        echo "[INFO]  切换到池化模式：-cni-pool-enabled -cni-pool-size ${size}"
        ;;
    off)
        export CNI_POOL_ENABLED=0
        export CNI_POOL_SIZE=0
        echo "[INFO]  切换到非池化模式"
        ;;
    status)
        cmdline="$(cri_multiplex_cmdline 2>/dev/null || true)"
        if [ -z "${cmdline}" ]; then
            echo "[INFO]  cri-multiplex 未运行"
            exit 0
        fi
        if echo "${cmdline}" | grep -q -- "-cni-pool-enabled"; then
            pool_size="$(echo "${cmdline}" | grep -oE -- '-cni-pool-size [0-9]+' | awk '{print $2}')"
            echo "[INFO]  当前为池化模式：-cni-pool-enabled -cni-pool-size ${pool_size:-未知}"
        else
            echo "[INFO]  当前为非池化模式（未传 -cni-pool-enabled）"
        fi
        exit 0
        ;;
    *)
        echo "用法: bash switch_cni_pool.sh on [size] | off | status" >&2
        exit 1
        ;;
esac

# 保留既有部署参数并强制重启
export HIDE_SANDBOX_LABEL="${HIDE_SANDBOX_LABEL:-flux-sandbox.io/direct=true}"
export E2B_FORCE_RESTART=1

"${SCRIPT_DIR}/01_start_multiplex.sh"

cmdline="$(cri_multiplex_cmdline)"
if [ "${mode}" = "on" ]; then
    if echo "${cmdline}" | grep -q -- "-cni-pool-enabled" && echo "${cmdline}" | grep -q -- "-cni-pool-size ${size}"; then
        log_pass "已切换到池化模式：-cni-pool-enabled -cni-pool-size ${size}（预热池开始后台灌满，可观察 /tmp/cri-multiplex.log 的 'CNI pool warm' 日志）"
    else
        log_fail "切换失败，启动参数不符合预期: ${cmdline}"
        exit 1
    fi
else
    if echo "${cmdline}" | grep -q -- "-cni-pool-enabled"; then
        log_fail "切换失败，启动参数仍包含 -cni-pool-enabled: ${cmdline}"
        exit 1
    fi
    log_pass "已切换到非池化模式"
fi

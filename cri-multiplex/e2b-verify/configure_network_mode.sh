#!/bin/bash
###############################################################################
# configure_network_mode.sh — 切换 cri-multiplex E2B 网络模式
#
# 用法:
#   bash e2b-verify/configure_network_mode.sh non-cni
#   bash e2b-verify/configure_network_mode.sh cni
#
# non-cni: 不启用 -e2b-cni-enabled，适合 02/04/05/06 crictl 直连用例。
# cni:     启用 -e2b-cni-enabled，适合 07/08/09/10/11 kubelet/CNI 用例。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

mode="${1:-}"
case "${mode}" in
    non-cni|nocni|off|0)
        log_info "切换 cri-multiplex 到非 CNI 模式"
        E2B_CNI_ENABLED=0 "${SCRIPT_DIR}/01_start_multiplex.sh"
        ;;
    cni|on|1)
        log_info "切换 cri-multiplex 到 CNI 模式"
        E2B_CNI_ENABLED=1 "${SCRIPT_DIR}/01_start_multiplex.sh"
        ;;
    *)
        echo "用法: $0 {non-cni|cni}" >&2
        exit 1
        ;;
esac

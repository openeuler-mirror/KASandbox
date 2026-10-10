#!/bin/bash
###############################################################################
# prepare_egress_proxy_deps.sh — per-sandbox 代理 / expose-ports 特性环境预置
#
# 为 26 / 35 / 37 / 39 号用例准备节点级依赖（幂等，可重复执行）：
#   1. 宿主侧文件：
#      - /opt/mitmproxy/mitmdump              mitmproxy standalone 二进制
#                                             （SANDBOX_PROXY_BINARY 默认值，
#                                              经 DS hostPath 挂载进 orchestrator pod）
#      - /opt/opensandbox-egress/addon.py     代理 addon（SANDBOX_PROXY_ADDON 默认值，
#                                             从 cri-multiplex 仓库根目录复制）
#      - /var/lib/cri-multiplex/egress-ca/    测试 CA（CN=e2b-egress-test-ca，35 号
#                                             用例的真源；CA_AUTO 场景由 orchestrator
#                                             自行生成代际化 CA，此处仅保证目录存在）
#      - /var/log/e2b35-proxy/                代理日志目录（DS hostPath 挂载）
#   2. template-manager DaemonSet hostPath 挂载校验/补齐：
#      /opt/mitmproxy、/opt/opensandbox-egress、/var/lib/cri-multiplex/egress-ca、
#      /var/log/e2b35-proxy（缺失时 PATCH_DS=1 默认自动 kubectl patch 并等滚动）
#   3. orchestrator 镜像特性 marker 检查（SANDBOX_EGRESS_PROXY_MODE 字符串，
#      缺失说明镜像过旧，仅告警不中断——35/37/39 预检会 fail-fast）
#   4. guest 模板镜像（curl + 测试 CA 内置在 /etc/ssl/certs/e2b-egress-test-ca.pem）
#      在 harbor 基础镜像中预制，不在本脚本范围内；末尾打印人工核查方法。
#
# 用法:
#   sudo bash prepare_egress_proxy_deps.sh              # 全量预置（缺失才下载/生成/patch）
#   PATCH_DS=0 bash prepare_egress_proxy_deps.sh        # 只校验 DS 挂载，不自动 patch
#   FORCE_MITMDUMP=1 bash prepare_egress_proxy_deps.sh  # 强制重新下载 mitmdump
#   MITMPROXY_VERSION=11.1.3 bash prepare_egress_proxy_deps.sh
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

log_section "per-sandbox 代理 / expose-ports 环境依赖预置"

MITMPROXY_VERSION="${MITMPROXY_VERSION:-11.1.3}"
MITMDUMP_DST="${PROXY_BINARY_HOST:-/opt/mitmproxy/mitmdump}"
ADDON_DST="${PROXY_ADDON_HOST:-/opt/opensandbox-egress/addon.py}"
ADDON_SRC="${ADDON_SRC:-${MULTIPLEX_DIR}/addon.py}"
CA_DIR_HOST="${CA_DIR_HOST:-/var/lib/cri-multiplex/egress-ca}"
PROXY_LOG_DIR_HOST="${PROXY_LOG_DIR_HOST:-/var/log/e2b35-proxy}"
PATCH_DS="${PATCH_DS:-1}"
FORCE_MITMDUMP="${FORCE_MITMDUMP:-0}"
ORCH_NS="${ORCH_NS:-e2b}"
ORCH_DS="${ORCH_DS:-template-manager}"

FAIL_COUNT=0

#==================== 1. mitmdump standalone ====================#
log_step "1.1 mitmdump standalone（${MITMDUMP_DST}，版本 ${MITMPROXY_VERSION}）"
if [ -x "${MITMDUMP_DST}" ] && [ "${FORCE_MITMDUMP}" != "1" ]; then
    log_pass "mitmdump 已存在（$(${MITMDUMP_DST} --version 2>/dev/null | head -1)）"
else
    arch="$(uname -m)"
    case "${arch}" in
        x86_64|amd64)  dl_arch="x86_64" ;;
        aarch64|arm64) dl_arch="aarch64" ;;
        *) log_fail "不支持的架构: ${arch}"; FAIL_COUNT=$((FAIL_COUNT+1)); dl_arch="" ;;
    esac
    if [ -n "${dl_arch}" ]; then
        url="https://downloads.mitmproxy.org/${MITMPROXY_VERSION}/mitmproxy-${MITMPROXY_VERSION}-linux-${dl_arch}.tar.gz"
        tmp_tgz="$(mktemp /tmp/mitmproxy-XXXXXX.tar.gz)"
        log_info "下载 ${url}"
        if curl -fSL --retry 3 -o "${tmp_tgz}" "${url}"; then
            mkdir -p "$(dirname "${MITMDUMP_DST}")"
            tar -xzf "${tmp_tgz}" -C "$(dirname "${MITMDUMP_DST}")" mitmdump
            chmod +x "${MITMDUMP_DST}"
            log_pass "mitmdump 安装完成（$(${MITMDUMP_DST} --version 2>/dev/null | head -1)）"
        else
            log_fail "mitmdump 下载失败: ${url}"
            FAIL_COUNT=$((FAIL_COUNT+1))
        fi
        rm -f "${tmp_tgz}"
    fi
fi

#==================== 2. addon.py ====================#
log_step "1.2 addon.py（${ADDON_DST}）"
if [ ! -f "${ADDON_SRC}" ]; then
    log_fail "仓库 addon.py 不存在: ${ADDON_SRC}"
    FAIL_COUNT=$((FAIL_COUNT+1))
elif [ -f "${ADDON_DST}" ] && cmp -s "${ADDON_SRC}" "${ADDON_DST}"; then
    log_pass "addon.py 已与仓库版本一致"
else
    mkdir -p "$(dirname "${ADDON_DST}")"
    cp -f "${ADDON_SRC}" "${ADDON_DST}"
    log_pass "addon.py 已从仓库同步（${ADDON_SRC} -> ${ADDON_DST}）"
fi

#==================== 3. 测试 CA / 日志目录 ====================#
log_step "1.3 测试 CA（${CA_DIR_HOST}）与代理日志目录"
mkdir -p "${CA_DIR_HOST}" "${PROXY_LOG_DIR_HOST}"
if [ -s "${CA_DIR_HOST}/mitmproxy-ca.pem" ] && [ -s "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" ]; then
    log_pass "测试 CA 已存在（CN=$(openssl x509 -in "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" -noout -subject 2>/dev/null | sed 's/.*CN *= *//')）"
else
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
        -keyout "${CA_DIR_HOST}/mitmproxy-ca.key" -out "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" \
        -subj "/CN=e2b-egress-test-ca" >/dev/null 2>&1 \
        || { log_fail "openssl 生成测试 CA 失败"; FAIL_COUNT=$((FAIL_COUNT+1)); }
    if [ -s "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" ]; then
        cat "${CA_DIR_HOST}/mitmproxy-ca-cert.pem" "${CA_DIR_HOST}/mitmproxy-ca.key" \
            > "${CA_DIR_HOST}/mitmproxy-ca.pem"
        chmod 600 "${CA_DIR_HOST}/mitmproxy-ca.key" "${CA_DIR_HOST}/mitmproxy-ca.pem"
        log_pass "测试 CA 已生成（CN=e2b-egress-test-ca，${CA_DIR_HOST}）"
    fi
fi

#==================== 4. DaemonSet hostPath 挂载 ====================#
log_step "2.1 ${ORCH_DS} DaemonSet hostPath 挂载校验"
REQUIRED_MOUNTS=(
    "/opt/mitmproxy|mitmproxy-bin|/opt/mitmproxy"
    "/opt/opensandbox-egress|opensandbox-egress|/opt/opensandbox-egress"
    "/var/lib/cri-multiplex/egress-ca|egress-ca|/var/lib/cri-multiplex/egress-ca"
    "/var/log/e2b35-proxy|egress-proxy-log|/var/log/e2b35-proxy"
)

ds_mounts_json=""
if command -v kubectl >/dev/null 2>&1; then
    ds_mounts_json=$(kubectl -n "${ORCH_NS}" get ds "${ORCH_DS}" -o json 2>/dev/null || true)
fi

if [ -z "${ds_mounts_json}" ]; then
    log_fail "无法读取 ${ORCH_NS}/${ORCH_DS} DaemonSet（kubectl 不可用或 DS 不存在）"
    FAIL_COUNT=$((FAIL_COUNT+1))
else
    missing_mounts=()
    for spec in "${REQUIRED_MOUNTS[@]}"; do
        IFS='|' read -r mount_path vol_name host_path <<< "${spec}"
        if python3 -c "
import json,sys
d=json.loads(sys.stdin.read())
spec=d['spec']['template']['spec']
vols={v['name']:v for v in spec.get('volumes',[])}
mounts={m['mountPath'] for c in spec['containers'] if c['name']=='template-manager' for m in c.get('volumeMounts',[])}
ok='${vol_name}' in vols and vols['${vol_name}'].get('hostPath',{}).get('path')=='${host_path}' and '${mount_path}' in mounts
sys.exit(0 if ok else 1)
" <<< "${ds_mounts_json}"; then
            log_pass "挂载已就绪: ${mount_path} (hostPath ${host_path})"
        else
            log_info "挂载缺失: ${mount_path}"
            missing_mounts+=("${spec}")
        fi
    done

    if [ ${#missing_mounts[@]} -gt 0 ]; then
        if [ "${PATCH_DS}" = "1" ]; then
            log_info "自动 patch ${ORCH_DS} 补齐缺失挂载 ..."
            vol_json="[]"; mnt_json="[]"
            for spec in "${missing_mounts[@]}"; do
                IFS='|' read -r mount_path vol_name host_path <<< "${spec}"
                vol_json=$(python3 -c "
import json,sys
v=json.loads('${vol_json}')
v.append({'name':'${vol_name}','hostPath':{'path':'${host_path}','type':'DirectoryOrCreate'}})
print(json.dumps(v))")
                mnt_json=$(python3 -c "
import json,sys
m=json.loads('${mnt_json}')
m.append({'name':'${vol_name}','mountPath':'${mount_path}'})
print(json.dumps(m))")
            done
            patch=$(python3 -c "
import json
print(json.dumps({'spec':{'template':{'spec':{
  'volumes': json.loads('''${vol_json}'''),
  'containers':[{'name':'template-manager','volumeMounts': json.loads('''${mnt_json}''')}]
}}}}))")
            if kubectl -n "${ORCH_NS}" patch ds "${ORCH_DS}" --type=strategic -p "${patch}"; then
                log_info "等待 ${ORCH_DS} 滚动完成 ..."
                kubectl -n "${ORCH_NS}" rollout status "ds/${ORCH_DS}" --timeout=300s \
                    && log_pass "DaemonSet 挂载补齐完成" \
                    || { log_fail "DaemonSet 滚动超时"; FAIL_COUNT=$((FAIL_COUNT+1)); }
            else
                log_fail "kubectl patch DaemonSet 失败"
                FAIL_COUNT=$((FAIL_COUNT+1))
            fi
        else
            log_fail "存在缺失挂载且 PATCH_DS=0，请手工补齐: ${missing_mounts[*]}"
            FAIL_COUNT=$((FAIL_COUNT+1))
        fi
    fi
fi

#==================== 5. orchestrator 镜像 marker ====================#
log_step "3.1 orchestrator 镜像特性 marker 检查"
tm_pod_name=$(kubectl -n "${ORCH_NS}" get pods -o name 2>/dev/null | grep "${ORCH_DS}" | head -1 || true)
if [ -n "${tm_pod_name}" ]; then
    mark=$(kubectl -n "${ORCH_NS}" exec "${tm_pod_name#pod/}" -- sh -c \
        'grep -ac "SANDBOX_EGRESS_PROXY_MODE" /usr/bin/orchestrator 2>/dev/null || echo 0' \
        2>/dev/null | tr -d '[:space:]' || true)
    if [ "${mark:-0}" -ge 1 ]; then
        log_pass "orchestrator 镜像含 per-sandbox 代理特性"
    else
        log_info "WARN: orchestrator 镜像不含 SANDBOX_EGRESS_PROXY_MODE marker，35/37/39 预检将失败，请更新 DaemonSet 镜像"
    fi
else
    log_info "WARN: 未找到 ${ORCH_DS} pod，跳过镜像 marker 检查"
fi

#==================== 6. guest 模板镜像提示 ====================#
log_step "4.1 guest 模板镜像（人工核查项）"
log_info "35 号用例要求模板镜像内置 curl 与测试 CA（guest 内路径 /etc/ssl/certs/e2b-egress-test-ca.pem）。"
log_info "关键约束：guest 内置 CA 的内容（指纹）必须与 ${CA_DIR_HOST}/mitmproxy-ca-cert.pem 一致，"
log_info "否则 MITM 签发证书不被 guest 信任（35 断言5 会失败）。核查方法：起任意沙箱后 exec"
log_info "  'openssl x509 -in /etc/ssl/certs/e2b-egress-test-ca.pem -noout -fingerprint -sha256'"
log_info "并与宿主侧 'openssl x509 -in ${CA_DIR_HOST}/mitmproxy-ca-cert.pem -noout -fingerprint -sha256' 对比。"
log_info "若不一致：或用 confdir 的 CA 重建模板镜像（harbor 基础镜像，BUILD_IMAGE_NAME 默认"
log_info "ubuntu:22.04-custom），或把镜像内 CA 对应的密钥材料放回 ${CA_DIR_HOST}（三件套："
log_info "mitmproxy-ca.pem / mitmproxy-ca-cert.pem / mitmproxy-ca.key）。"

#==================== 汇总 ====================#
echo ""
if [ "${FAIL_COUNT}" -eq 0 ]; then
    log_pass "环境依赖预置完成"
    exit 0
else
    log_fail "环境依赖预置存在 ${FAIL_COUNT} 项失败，请按上方日志处理"
    exit 1
fi

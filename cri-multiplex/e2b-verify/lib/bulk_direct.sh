#!/bin/bash
###############################################################################
# lib/bulk_direct.sh — direct sandbox 并发创建/清理/CNI 耗时统计的公共函数
#
# 供 24（不带预热池）/25（带预热池）并发性能脚本使用。
# 依赖 lib/common.sh 已 source（log_*、prepare_direct_pod_json、run_pod_sandbox、
# grpc_call、cleanup_pod）。
#
# 约定：
#   BASE_POD_JSON   — 调用方设置的基准 Pod JSON 路径
#   BATCH_POD_IDS   — 本批次创建成功的 pod id 列表（函数内维护）
#   BATCH_ELAPSED_MS — 本批次并发调用阶段耗时（毫秒）
###############################################################################

BASE_POD_JSON="${BASE_POD_JSON:-${POD_JSON:-}}"
BATCH_POD_IDS=()
BATCH_ELAPSED_MS=0

now_ms() {
    date +%s%3N
}

fmt_ms() {
    local ms="$1"
    printf '%d.%03ds' "$((ms / 1000))" "$((ms % 1000))"
}

add_direct_label_to_pod_json() {
    local pod_json="$1"
    python3 - "${pod_json}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    pod = json.load(source)
labels = pod.setdefault("labels", {})
labels["flux-sandbox.io/direct"] = "true"
labels["flux-sandbox.io/runtime"] = "e2b"
with open(path, "w", encoding="utf-8") as target:
    json.dump(pod, target, indent=2, ensure_ascii=True)
    target.write("\n")
PY
}

# 单个沙箱的 RunPodSandbox 调用：成功时把 pod_id 写入 outf，过程日志写入 logf
#（不直接输出，避免并发刷屏/污染捕获）。pod_json 已在批量准备阶段生成完毕。
runp_direct_sandbox_one() {
    local pod_json="$1"
    local outf="$2"
    local logf="$3"
    : > "${outf}"
    {
        export POD_JSON="${pod_json}"
        local pod_id
        pod_id="$(run_pod_sandbox)" || exit 1
        printf '%s' "${pod_id}" > "${outf}"
    } > "${logf}" 2>&1
}

# 并发批量创建：
#   阶段 1（不计时）：串行预生成全部 Pod JSON 并加 direct label；
#   阶段 2（计时）：所有沙箱同时调用 RunPodSandbox，
#   耗时 = 第一个沙箱开始调用到最后一个沙箱调用结束的时间。
prepare_direct_sandbox_batch() {
    local prefix="$1"
    local count="$2"
    local idx
    local start_ms
    local end_ms
    local result_dir
    result_dir="$(mktemp -d /tmp/e2b-bulk-result-XXXXXX)"

    # 阶段 1：预生成全部 Pod JSON
    local -a pod_jsons=()
    for idx in $(seq 1 "${count}"); do
        if ! prepare_direct_pod_json "${prefix}-${idx}" "${BASE_POD_JSON}"; then
            rm -rf "${result_dir}"
            return 1
        fi
        if ! add_direct_label_to_pod_json "${POD_JSON}"; then
            rm -f "${POD_JSON}" || true
            rm -rf "${result_dir}"
            return 1
        fi
        pod_jsons+=("${POD_JSON}")
    done

    # 阶段 2：所有沙箱同时调用 RunPodSandbox
    BATCH_POD_IDS=()
    start_ms=$(now_ms)
    for idx in $(seq 1 "${count}"); do
        runp_direct_sandbox_one "${pod_jsons[$((idx - 1))]}" \
            "${result_dir}/${idx}.out" "${result_dir}/${idx}.log" &
    done
    wait
    end_ms=$(now_ms)

    local failed=0
    for idx in $(seq 1 "${count}"); do
        local outf="${result_dir}/${idx}.out"
        if [ -s "${outf}" ]; then
            BATCH_POD_IDS+=("$(cat "${outf}")")
        else
            failed=$((failed + 1))
            log_info "sandbox ${prefix}-${idx} 创建失败，日志："
            sed 's/^/    /' "${result_dir}/${idx}.log" >&2 || true
        fi
        rm -f "${pod_jsons[$((idx - 1))]}" || true
    done
    rm -rf "${result_dir}"
    BATCH_ELAPSED_MS=$((end_ms - start_ms))
    export BATCH_ELAPSED_MS
    if [ "${failed}" -gt 0 ]; then
        log_fail "并发创建 ${count} 个 direct sandbox：${failed} 个失败"
        return 1
    fi
}

cleanup_direct_sandbox_batch() {
    local pod_id
    local deadline
    local remaining
    local now
    local start_ms
    local end_ms
    local timed_out=0

    if [ "${#BATCH_POD_IDS[@]}" -eq 0 ]; then
        return 0
    fi

    start_ms=$(now_ms)
    for pod_id in "${BATCH_POD_IDS[@]}"; do
        grpc_call "runtime.v1.RuntimeService/RemovePodSandbox" "{\"pod_sandbox_id\": \"${pod_id}\"}" >/dev/null 2>&1 || true
    done

    deadline=$(( $(date +%s) + 120 ))
    while true; do
        remaining=0
        for pod_id in "${BATCH_POD_IDS[@]}"; do
            if ${CRICTL} inspectp "${pod_id}" >/dev/null 2>&1; then
                remaining=$((remaining + 1))
            fi
        done
        if [ "${remaining}" -eq 0 ]; then
            break
        fi
        now=$(date +%s)
        if [ "${now}" -ge "${deadline}" ]; then
            log_fail "批量清理超时，仍有 ${remaining} 个沙箱未消失"
            timed_out=1
            break
        fi
        sleep 1
    done

    end_ms=$(now_ms)
    if [ "${timed_out}" -eq 1 ]; then
        return 1
    fi
    BATCH_POD_IDS=()
    log_pass "批量沙箱清理完成，耗时 $(fmt_ms "$((end_ms - start_ms))")"
    return 0
}

# 从 cri-multiplex 日志统计某一批次沙箱的 CNI ADD 耗时分布
# 用法: report_cni_add_stats <prefix>（prefix 如 bulk24-100，匹配 sandbox=e2b<prefix>-*）
report_cni_add_stats() {
    local prefix="$1"
    python3 - "/tmp/cri-multiplex.log" "e2b${prefix}-" <<'PY' >&2
import re
import sys

log_path, id_prefix = sys.argv[1], sys.argv[2]
pat = re.compile(r"CNI ADD: sandbox=(\S+).*?\bcni_add_ms=(\d+)")
vals = []
try:
    with open(log_path, encoding="utf-8", errors="replace") as f:
        for line in f:
            m = pat.search(line)
            if m and m.group(1).startswith(id_prefix):
                vals.append(int(m.group(2)))
except OSError as exc:
    print(f"[WARN]  读取 cri-multiplex 日志失败: {exc}")
    sys.exit(1)

if not vals:
    print(f"[INFO]  未找到 sandbox 前缀 {id_prefix} 的 CNI ADD 耗时日志")
    sys.exit(1)

vals.sort()
n = len(vals)

def pct(p):
    k = (n - 1) * p
    lo = int(k)
    hi = min(lo + 1, n - 1)
    return vals[lo] + (vals[hi] - vals[lo]) * (k - lo)

print(
    f"[STAT]  CNI ADD 耗时(ms) prefix={id_prefix}* count={n} "
    f"avg={sum(vals)/n:.1f} p50={pct(0.50):.0f} p90={pct(0.90):.0f} "
    f"p99={pct(0.99):.0f} min={vals[0]} max={vals[-1]}"
)
PY
}

# 等待 CNI 预热池填满：解析 cri-multiplex 日志最后一条
# "CNI pool warm: ... ready=N/M"，直到 N == M。仅带预热池的并发脚本
#（25）在批次开始前调用，保证并发创建全部命中池（cni_add_ms≈0）。
wait_cni_pool_full() {
    local timeout_s="${1:-900}"
    local deadline=$(( $(date +%s) + timeout_s ))
    local line ready cur total
    while true; do
        line=$(grep -a 'CNI pool warm:' /tmp/cri-multiplex.log 2>/dev/null | tail -1 || true)
        ready=$(echo "${line}" | grep -oE 'ready=[0-9]+/[0-9]+' | tail -1 || true)
        if [ -n "${ready}" ]; then
            cur="${ready#ready=}"
            cur="${cur%/*}"
            total="${ready##*/}"
            if [ "${cur}" = "${total}" ] && [ "${total}" != "0" ]; then
                log_pass "CNI 预热池已就绪：${ready}"
                return 0
            fi
            log_info "CNI 预热中：${ready}"
        else
            log_info "CNI 预热中：尚无 warm 记录"
        fi
        if [ "$(date +%s)" -ge "${deadline}" ]; then
            log_fail "等待 CNI 预热池超时（${timeout_s}s），最后状态: ${ready:-无}"
            return 1
        fi
        sleep 5
    done
}

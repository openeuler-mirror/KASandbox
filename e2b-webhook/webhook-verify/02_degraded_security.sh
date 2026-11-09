#!/bin/bash
###############################################################################
# 02_degraded_security.sh — 后端异常降级 + TLS/日志/非法请求
#
# 用例：
#   D1 API 500（allowed，只补 runtimeClassName）
#   D2 API 超时（超时值 + 3s 内返回，只补 runtimeClassName）
#   D3 连接拒绝（指向未监听端口）
#   D4 非法 JSON（API 返回非 JSON）
#   D5 超大响应（1MB，不崩、不被 OOM，只补 runtimeClassName）
#   D6 API Key 透传（X-API-Key；全部副本日志不打印）
#   A22 注解合并超 200KB（从 01 移入，复用本模块的 mock）
#   D7 TLS 与 MWC（Secret/服务端证书/caBundle 一致、明文不 200、failurePolicy/timeout/v1）
#   D8 非法请求（非 JSON 400、GET /mutate 405、未知路径 404）
#   D9 证书 SAN/有效期/链 + TLS 版本（checkhost/checkend/verify、TLS1.2 成功、TLS1.1 被拒）
#   D10 MWC 静态契约（两个 webhook 的 rules/operations/sideEffects/matchPolicy/service）
#
# 做法：把 webhook 的 E2B_API_URL 指向本机 mock（整轮只切换一次），再通过
# mock 控制面按用例改行为，制造 500 / 超时 / 拒连 / 非法 JSON / 超大响应；
# 断言“只补 runtimeClass”，并用 mock 的 last-response 证明故障响应真的发出；
# trap 兜底恢复原 env，失败也不留脏配置。
###############################################################################
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"
. "${SCRIPT_DIR}/lib/k8s.sh"
. "${SCRIPT_DIR}/lib/webhook.sh"
. "${SCRIPT_DIR}/lib/admission.sh"
. "${SCRIPT_DIR}/lib/provider.sh"

log_section "02 降级与安全"

# 独立运行（--only degraded）时 state.env 可能还没有现状信息，
# 而恢复 env 依赖 STOCK_API_URL，因此这里必须自己探测一次。
webhook_detect

WORK="${WV_TMP}/02"
mkdir -p "${WORK}"
TEMPLATE="${WV_E2E_TEMPLATE}"
RESP=""; PATCH=""; APPLIED=""
MOCK_OK=0
MOCK_ENV_ON=0
MOCK_SWITCH_FAILED=0

cleanup_02() {
    local rc=$?
    set +e
    [ "${MOCK_ENV_ON}" = "1" ] && webhook_restore_stock_env >/dev/null 2>&1
    mock_api_stop
    admission_stop_port_forward
    return "${rc}"
}
trap cleanup_02 EXIT

if ! admission_ensure_port_forward; then
    fail D8 "port-forward 失败，无法访问 /mutate"
    finish "02 降级与安全"
fi

API_TIMEOUT=$(webhook_api_timeout_seconds)
log_info "webhook E2B_API_TIMEOUT=${API_TIMEOUT}s（来自 Deployment env，默认 5s）"

#==================== 辅助 ====================#
mutate_once() {
    local name="$1" obj="$2"
    local prefix="${WORK}/${name}"
    RESP="${prefix}.resp"; PATCH="${prefix}.patch"; APPLIED="${prefix}.applied"
    admission_post "${obj}" "${RESP}" CREATE || return 1
    [ -s "${RESP}" ] || return 1
    admission_patch_json "${RESP}" > "${PATCH}" || return 1
    admission_apply_patch "${obj}" "${PATCH}" "${APPLIED}" || return 1
}

only_runtimeclass_patch() {
    jq -e 'length == 1 and .[0].path == "/spec/runtimeClassName"' "${PATCH}" >/dev/null 2>&1
}

denied_annotations() {
    provider_annotation_count "$(provider_active)" "$(admission_annotations "${APPLIED}")"
}

build_probe_pod() {
    pod_object "${WORK}/pod.json" "${WV_TEST_NS}" "wv-degraded" "template=${TEMPLATE}"
}

# mock 可用性：env 只切一次，之后每条用例只改 mock 行为。
mock_case_ready() {
    local id="$1"
    if [ "${MOCK_OK}" = "1" ]; then
        return 0
    fi
    if [ "${MOCK_SWITCH_FAILED}" = "1" ]; then
        fail "${id}" "无法把 E2B_API_URL 切到 mock（$(mock_api_url)）"
    else
        unsupported "${id}" "mock API 从集群内不可达（$(mock_api_url)）"
    fi
    return 1
}

# degraded_case <id> <描述> <mock 响应断言> <mock 模式命令...>
# 例：degraded_case D1 "transform API 500" '.status == 500' mock_api_mode_status 500
#     mock 响应断言传 "-" 表示不校验；否则必须证明故障响应真的由 mock 发出。
degraded_case() {
    local id="$1" desc="$2" expect="$3"; shift 3
    mock_case_ready "${id}" || return 0
    if ! "$@"; then
        fail "${id}" "设置 mock 行为失败（$*）"
        return 0
    fi
    build_probe_pod

    local start
    start=$(now_ms)
    if ! mutate_once "${id}" "${WORK}/pod.json"; then
        fail "${id}" "${desc}：准入请求失败（http=$(admission_resp_http_code "${RESP}" 2>/dev/null || echo '?')）"
    else
        local elapsed=$(( $(now_ms) - start ))
        local code annos served expect_ok=1
        code=$(admission_resp_http_code "${RESP}")
        annos=$(denied_annotations)
        served=$(mock_api_last_response)
        if [ "${expect}" != "-" ]; then
            printf '%s' "${served}" | jq -e "${expect}" >/dev/null 2>&1 || expect_ok=0
        fi
        if [ "${code}" = "200" ] && [ "$(admission_resp_allowed "${RESP}")" = "true" ] \
            && only_runtimeclass_patch && [ "${annos}" = "0" ] && [ "${expect_ok}" = "1" ]; then
            pass "${id}" "${desc}：allowed，只补 runtimeClassName（${elapsed}ms；mock ${served}）"
        else
            fail "${id}" "${desc}：http=${code} allowed=$(admission_resp_allowed "${RESP}" 2>/dev/null || echo '?') patch=$(jq -c . "${PATCH}" 2>/dev/null) 注解=${annos} mock=${served}"
        fi
    fi
}

#==================== mock 可用性 ====================#
mock_api_start || true
if mock_api_reachable_from_cluster; then
    MOCK_OK=1
    log_info "mock API 集群内可达: $(mock_api_url)"
    # 整轮只切换一次 E2B_API_URL；D1–D6 通过控制面改 mock 行为，不再逐条滚动。
    # 先置位：切换过程中 env 可能已经改掉，失败也必须让 trap 负责恢复。
    MOCK_ENV_ON=1
    if webhook_switch_to_mock; then
        MOCK_OK=1
    else
        MOCK_SWITCH_FAILED=1
        MOCK_OK=0
    fi
else
    log_warn "mock API 从集群内不可达（$(mock_api_url)），D1/D2/D4/D5/D6 将记 UNSUPPORTED"
    log_warn "  提示：设置 WV_MOCK_HOST 为本机在集群网络内的地址"
fi

#==================== D1 API 500 ====================#
log_step "D1 API 500"
degraded_case D1 "transform API 500" '.status == 500' mock_api_mode_status 500

#==================== D2 API 超时 ====================#
log_step "D2 API 超时"
if mock_case_ready D2; then
    if mock_api_mode_delay 7; then
        build_probe_pod
        start=$(now_ms)
        if mutate_once d2 "${WORK}/pod.json"; then
            elapsed=$(( $(now_ms) - start ))
            limit=$(( (API_TIMEOUT + 3) * 1000 ))
            annos=$(denied_annotations)
            served=$(mock_api_last_response)
            if [ "${elapsed}" -le "${limit}" ] && only_runtimeclass_patch && [ "${annos}" = "0" ] \
                && printf '%s' "${served}" | jq -e '.delay >= 7' >/dev/null 2>&1; then
                pass D2 "超时后在 ${elapsed}ms 内返回（限制 $((API_TIMEOUT + 3))s），只补 runtimeClassName；mock 延迟 $(printf '%s' "${served}" | jq -r '.delay')s 已生效"
            else
                fail D2 "耗时 ${elapsed}ms（限制 ${limit}ms）patch=$(jq -c . "${PATCH}" 2>/dev/null) 注解=${annos} mock=${served}"
            fi
        else
            fail D2 "超时场景准入请求失败"
        fi
    else
        fail D2 "设置 mock 行为（延迟 7s）失败"
    fi
fi

#==================== D3 连接拒绝 ====================#
log_step "D3 连接拒绝"
if mock_case_ready D3; then
    # 关掉 mock 的数据面监听（控制面仍在 18081），webhook 连同一个地址会得到
    # connection refused —— 与“指向未监听端口”的语义完全一致，但不用改 env。
    if mock_api_mode_refuse; then
        if curl -sS -m 2 "http://127.0.0.1:${WV_MOCK_PORT}/healthz" >/dev/null 2>&1; then
            fail D3 "mock 数据面仍在监听，connection refused 故障未生效"
        else
            build_probe_pod
            if mutate_once d3 "${WORK}/pod.json"; then
                if [ "$(admission_resp_http_code "${RESP}")" = "200" ] && only_runtimeclass_patch; then
                    pass D3 "mock 数据面已关闭（连接必被拒绝）：allowed，只补 runtimeClassName"
                else
                    fail D3 "http=$(admission_resp_http_code "${RESP}") patch=$(jq -c . "${PATCH}" 2>/dev/null)"
                fi
            else
                fail D3 "拒连场景准入请求失败"
            fi
        fi
        mock_api_mode_normal || log_warn "恢复 mock 数据面失败"
    else
        fail D3 "设置 mock 行为（refuse）失败"
    fi
fi

#==================== D4 非法 JSON ====================#
log_step "D4 非法 JSON 响应"
degraded_case D4 "API 返回非 JSON" '.status == 200 and .bytes == 8 and .body_head == "not-json"' mock_api_mode_body 'not-json'

#==================== D5 超大响应 ====================#
log_step "D5 超大响应"
if mock_case_ready D5; then
    if mock_api_mode_size 1048576; then
        build_probe_pod
        if mutate_once d5 "${WORK}/pod.json"; then
            code=$(admission_resp_http_code "${RESP}")
            healthz=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/healthz" 2>/dev/null || true)
            annos=$(denied_annotations)
            served=$(mock_api_last_response)
            served_bytes=$(printf '%s' "${served}" | jq -r '.bytes // 0' 2>/dev/null || echo 0)
            if [ "${code}" = "200" ] && [ "${healthz}" = "200" ] \
                && only_runtimeclass_patch && [ "${annos}" = "0" ] && [ "${served_bytes:-0}" -ge 1048576 ]; then
                pass D5 "mock 实际返回 ${served_bytes}B；webhook 未崩（/healthz 200），allowed 且只补 runtimeClassName，注解 0 个"
            else
                fail D5 "http=${code} /healthz=${healthz} patch=$(jq -c . "${PATCH}" 2>/dev/null) 注解=${annos} mock 响应=${served_bytes}B（疑似被 200KB 上限或 OOM 影响）"
            fi
        else
            fail D5 "超大响应场景准入失败"
        fi
    else
        fail D5 "设置 mock 行为（1MB body）失败"
    fi
fi

#==================== D6 API Key 透传 ====================#
log_step "D6 API Key 透传"
if mock_case_ready D6; then
    api_key=$(webhook_api_key 2>/dev/null || true)
    if [ -z "${api_key}" ]; then
        skip D6 "未取得 API Key（Secret ${WV_APIKEY_SECRET} / ${WV_E2B_CONFIG} 均不可用）"
    elif mock_api_mode_normal; then
        count_before=$(mock_api_count)
        build_probe_pod
        if ! mutate_once d6 "${WORK}/pod.json"; then
            fail D6 "准入请求失败（http=$(admission_resp_http_code "${RESP}" 2>/dev/null || echo '?')）"
        else
            count_after=$(mock_api_count)
            served=$(mock_api_last_response)
            headers=$(mock_api_last_headers)
            seen=$(printf '%s' "${headers}" | jq -r '.["x-api-key"] // ""' 2>/dev/null || true)
            # 日志必须覆盖所有副本：kubectl logs deploy/... 只会挑一个 Pod。
            logs=$(webhook_logs_all 5000)
            leaked=$(printf '%s' "${logs}" | grep -F -c -- "${api_key}" || true)
            logs_ok=1
            [ -n "${logs}" ] || logs_ok=0
            if [ "${count_after:-0}" -gt "${count_before:-0}" ] \
                && printf '%s' "${served}" | jq -e '.status == 200' >/dev/null 2>&1 \
                && [ "${seen}" = "${api_key}" ] && [ "${leaked}" = "0" ] && [ "${logs_ok}" = "1" ]; then
                pass D6 "本次准入让 transform 计数 ${count_before}→${count_after}，X-API-Key 一致（长度 ${#api_key}），全部副本日志中 0 次出现"
            else
                reason=""
                [ "${count_after:-0}" -le "${count_before:-0}" ] && reason="${reason}mock 未收到本次 transform 请求；"
                [ "${seen}" != "${api_key}" ] && reason="${reason}请求头缺失或不一致；"
                [ "${leaked}" != "0" ] && reason="${reason}日志中出现 API Key 明文 ${leaked} 行；"
                [ "${logs_ok}" != "1" ] && reason="${reason}读不到 webhook 日志，无法证明未泄漏；"
                fail D6 "${reason}（before=${count_before} after=${count_after} mock=${served}）"
            fi
        fi
    else
        fail D6 "重置 mock 行为失败"
    fi
fi

#==================== A22 注解合并超 200KB ====================#
# 从 01 移入：本模块已经处于 mock 环境，A22 直接复用，避免再次切换/恢复。
log_step "A22 注解合并超 200KB"
if mock_case_ready A22; then
    python3 - "${WORK}/a22-big-body.json" <<'PYBODY'
import json, sys
json.dump({"sandbox": {"teamId": "x" * (210 * 1024)}}, open(sys.argv[1], "w"))
PYBODY
    if ! mock_api_mode_body_file "${WORK}/a22-big-body.json"; then
        fail A22 "设置 mock 模式（210KB body）失败"
    else
        pod_object "${WORK}/a22.json" "${WV_TEST_NS}" "wv-a22" "template=${TEMPLATE}"
        if ! mutate_once a22 "${WORK}/a22.json"; then
            fail A22 "准入请求失败（http=$(admission_resp_http_code "${RESP}" 2>/dev/null || echo '?')）"
        else
            code=$(admission_resp_http_code "${RESP}")
            ops=$(jq 'length' "${PATCH}" 2>/dev/null || echo 0)
            annos=$(denied_annotations)
            rc=$(admission_runtimeclass "${APPLIED}")
            served_bytes=$(mock_api_last_response | jq -r '.bytes // 0' 2>/dev/null || echo 0)
            if [ "${code}" = "200" ] && [ "$(admission_resp_allowed "${RESP}")" = "true" ] \
                && only_runtimeclass_patch && [ "${annos}" = "0" ] && [ "${served_bytes:-0}" -gt 204800 ]; then
                pass A22 "mock 实际返回 ${served_bytes}B（>200KB）；合并超限时只保留 runtimeClassName（patch=1，注解=0），未返回 5xx"
            else
                fail A22 "http=${code} patch=${ops} runtimeClass='${rc}' 注解=${annos} mock 响应=${served_bytes}B（应只保留 runtimeClassName，且故障体 >200KB）"
            fi
        fi
    fi
fi

# mock 用例（D1–D6 / A22）结束：恢复真实 API（整轮只滚动这一次）。
if [ "${MOCK_ENV_ON}" = "1" ]; then
    if webhook_restore_stock_env; then
        MOCK_ENV_ON=0
    else
        log_warn "恢复 E2B_API_URL 失败，05 会再次尝试"
    fi
fi

#==================== D7 TLS 与 MWC ====================#
log_step "D7 TLS 与 MWC"
if [ "$(state_get MWC_EXISTS)" != "1" ]; then
    skip D7 "MutatingWebhookConfiguration ${WV_MWC} 不存在"
elif [ "$(state_get TLS_SECRET_EXISTS)" != "1" ]; then
    skip D7 "TLS Secret ${WV_TLS_SECRET} 不存在"
else
    cert_sha=$(webhook_cert_sha256)
    mwc_count=$(webhook_mwc_webhook_count)
    cb0=$(webhook_cabundle_sha256 0)
    cb1=""
    if [ "${mwc_count:-0}" -gt 1 ]; then
        cb1=$(webhook_cabundle_sha256 1)
    fi
    # 真实 TLS 握手：证明 Pod 提供的就是 caBundle 里那张证书，
    # 而不只是“Secret 与 MWC 配置碰巧一致”。
    admission_ensure_port_forward >/dev/null 2>&1 || true
    served_sha=$(webhook_served_cert_sha256 2>/dev/null || true)
    plain_code=$(curl -s -o /dev/null -w '%{http_code}' -m 10 "http://127.0.0.1:${WV_PORT}/healthz" 2>/dev/null || true)

    policies=$(kubectl get mutatingwebhookconfiguration "${WV_MWC}" -o json 2>/dev/null \
        | jq -c '[.webhooks[] | {name: .name, failurePolicy: .failurePolicy, timeout: .timeoutSeconds, versions: .admissionReviewVersions}]')

    pod_policy=$(printf '%s' "${policies}" | jq -r '.[] | select(.name | startswith("pod")) | .failurePolicy')
    bs_policy=$(printf '%s' "${policies}" | jq -r '.[] | select(.name | startswith("batchsandbox")) | .failurePolicy')
    timeouts_ok=$(printf '%s' "${policies}" | jq '[.[] | select(.timeout == 5)] | length')
    versions_ok=$(printf '%s' "${policies}" | jq '[.[] | select(.versions == ["v1"])] | length')

    if [ -n "${cert_sha}" ] && [ "${cert_sha}" = "${cb0}" ] \
        && { [ -z "${cb1}" ] || [ "${cert_sha}" = "${cb1}" ]; } \
        && [ -n "${served_sha}" ] && [ "${served_sha}" = "${cb0}" ]; then
        tls_ok=1
    else
        tls_ok=0
    fi

    if [ "${tls_ok}" = "1" ] && [ "${plain_code}" != "200" ] \
        && [ "${pod_policy}" = "Fail" ] && [ "${bs_policy}" = "Ignore" ] \
        && [ "${timeouts_ok}" = "2" ] && [ "${versions_ok}" = "2" ]; then
        pass D7 "Secret、服务端实际证书与 caBundle 三者一致；明文请求返回 ${plain_code}；pod=Fail / batchsandbox=Ignore / 5s / v1"
    else
        artifact_save "d7-mwc.json" "${policies}" >/dev/null
        fail D7 "cert=${cert_sha:0:12} served=${served_sha:0:12} caBundle0=${cb0:0:12} caBundle1=${cb1:0:12} webhooks=${mwc_count} 明文=${plain_code} pod=${pod_policy} bs=${bs_policy} 5s=${timeouts_ok}/2 v1=${versions_ok}/2（证据 ${ARTIFACT_DIR}/d7-mwc.json）"
    fi
fi

#==================== D8 非法请求 ====================#
log_step "D8 非法请求"
bad_body_code=$(printf 'this is not json' | curl -s -k -o /dev/null -w '%{http_code}' -m 10 \
    -H 'Content-Type: application/json' -X POST "$(admission_url)/mutate" --data-binary @- 2>/dev/null || true)
get_mutate_code=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/mutate" 2>/dev/null || true)
unknown_code=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/no-such-path" 2>/dev/null || true)

d8_ok=1
[ "${bad_body_code}" = "400" ] || d8_ok=0
[ "${get_mutate_code}" = "405" ] || d8_ok=0
[ "${unknown_code}" = "404" ] || d8_ok=0

if [ "${d8_ok}" = "1" ]; then
    pass D8 "非 JSON body=400，GET /mutate=405，未知路径=404，均无 5xx"
elif printf '%s %s %s' "${bad_body_code}" "${get_mutate_code}" "${unknown_code}" | grep -q '5[0-9][0-9]'; then
    fail D8 "出现 5xx：非 JSON=${bad_body_code} GET /mutate=${get_mutate_code} 未知路径=${unknown_code}"
else
    fail D8 "状态码不符合预期：非 JSON=${bad_body_code}（期望 400）GET /mutate=${get_mutate_code}（期望 405）未知路径=${unknown_code}（期望 404）"
fi

#==================== D9 证书 SAN/有效期/链 + TLS 版本 ====================#
log_step "D9 证书 SAN/有效期/链/TLS 版本"
if [ "$(state_get MWC_EXISTS)" != "1" ]; then
    skip D9 "MutatingWebhookConfiguration ${WV_MWC} 不存在"
elif [ "$(state_get TLS_SECRET_EXISTS)" != "1" ]; then
    skip D9 "TLS Secret ${WV_TLS_SECRET} 不存在"
elif ! need_cmd openssl; then
    skip D9 "未安装 openssl"
else
    cert_pem="${WORK}/d9-cert.pem"
    ca_pem="${WORK}/d9-ca.pem"
    kubectl -n "${WV_NAMESPACE}" get secret "${WV_TLS_SECRET}" \
        -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d > "${cert_pem}" 2>/dev/null || true
    kubectl get mutatingwebhookconfiguration "${WV_MWC}" \
        -o jsonpath='{.webhooks[0].clientConfig.caBundle}' 2>/dev/null \
        | base64 -d > "${ca_pem}" 2>/dev/null || true

    if [ ! -s "${cert_pem}" ] || [ ! -s "${ca_pem}" ]; then
        fail D9 "无法读取 Secret 证书或 MWC caBundle（cert=$(wc -c < "${cert_pem}" 2>/dev/null || echo 0)B ca=$(wc -c < "${ca_pem}" 2>/dev/null || echo 0)B）"
    else
        host="${WV_SERVICE}.${WV_NAMESPACE}.svc"
        not_after=$(openssl x509 -in "${cert_pem}" -noout -enddate 2>/dev/null | cut -d= -f2)

        san_ok=0
        if openssl x509 -in "${cert_pem}" -noout -checkhost "${host}" 2>/dev/null \
            | grep -q 'does match certificate'; then
            san_ok=1
        fi

        expiry_ok=0
        openssl x509 -in "${cert_pem}" -noout -checkend 604800 >/dev/null 2>&1 && expiry_ok=1

        chain_ok=0
        if openssl verify -CAfile "${ca_pem}" "${cert_pem}" 2>/dev/null | grep -q ': OK'; then
            chain_ok=1
        fi

        admission_ensure_port_forward >/dev/null 2>&1 || true
        expected_sha=$(webhook_cert_sha256)
        served12=""
        if run_with_timeout 10 openssl s_client -connect "127.0.0.1:${WV_PORT}" \
            -servername "${host}" -tls1_2 </dev/null 2>/dev/null \
            | openssl x509 -outform PEM 2>/dev/null | sha256sum | awk '{print $1}' > "${WORK}/d9-served12.sha"; then
            served12=$(cat "${WORK}/d9-served12.sha" 2>/dev/null || true)
        fi
        tls12_ok=0
        if [ -n "${served12}" ] && [ "${served12}" = "${expected_sha}" ]; then
            tls12_ok=1
        fi

        tls11_rejected=0
        if ! run_with_timeout 10 openssl s_client -connect "127.0.0.1:${WV_PORT}" \
            -servername "${host}" -tls1_1 </dev/null >"${WORK}/d9-tls11.log" 2>&1; then
            tls11_rejected=1
        fi

        if [ "${san_ok}" = "1" ] && [ "${expiry_ok}" = "1" ] && [ "${chain_ok}" = "1" ] \
            && [ "${tls12_ok}" = "1" ] && [ "${tls11_rejected}" = "1" ]; then
            pass D9 "SAN 含 ${host}；有效期至 ${not_after}（>7 天）；caBundle 可验证证书链；TLS1.2 握手成功且证书与 Secret 一致（${expected_sha:0:12}）；TLS1.1 握手被拒"
        else
            fail D9 "san=${san_ok} expiry=${expiry_ok}（至 ${not_after:-?}）chain=${chain_ok} tls1.2=${tls12_ok}（served=${served12:0:12} secret=${expected_sha:0:12}）tls1.1_rejected=${tls11_rejected}；TLS1.1 日志 ${WORK}/d9-tls11.log"
        fi
    fi
fi

#==================== D10 MWC 静态契约 ====================#
log_step "D10 MWC 静态契约"
if [ "$(state_get MWC_EXISTS)" != "1" ]; then
    skip D10 "MutatingWebhookConfiguration ${WV_MWC} 不存在"
else
    mwc_json=$(kubectl get mutatingwebhookconfiguration "${WV_MWC}" -o json 2>/dev/null || true)
    contract=$(printf '%s' "${mwc_json}" | jq -c --arg svc "${WV_SERVICE}" --arg ns "${WV_NAMESPACE}" '
        ([.webhooks[] | select(.name | startswith("pod"))]) as $p
        | ([.webhooks[] | select(.name | startswith("batchsandbox"))]) as $b
        | [
            {check: "pod-webhook-count", ok: (($p | length) == 1)},
            {check: "batchsandbox-webhook-count", ok: (($b | length) == 1)},
            {check: "pod-rules-count", ok: (((($p[0].rules // []) | length)) == 1)},
            {check: "batchsandbox-rules-count", ok: (((($b[0].rules // []) | length)) == 1)},
            {check: "pod-apiGroups", ok: ((($p[0].rules[0].apiGroups // []) == [""]))},
            {check: "pod-apiVersions", ok: ((($p[0].rules[0].apiVersions // []) == ["v1"]))},
            {check: "pod-resources", ok: ((($p[0].rules[0].resources // []) == ["pods"]))},
            {check: "pod-operations", ok: ((($p[0].rules[0].operations // []) == ["CREATE"]))},
            {check: "pod-scope", ok: ($p[0].rules[0].scope == "*")},
            {check: "batchsandbox-apiGroups", ok: ((($b[0].rules[0].apiGroups // []) == ["sandbox.opensandbox.io"]))},
            {check: "batchsandbox-apiVersions", ok: ((($b[0].rules[0].apiVersions // []) == ["v1alpha1"]))},
            {check: "batchsandbox-resources", ok: ((($b[0].rules[0].resources // []) == ["batchsandboxes"]))},
            {check: "batchsandbox-operations", ok: ((($b[0].rules[0].operations // []) == ["CREATE"]))},
            {check: "batchsandbox-scope", ok: ($b[0].rules[0].scope == "*")},
            {check: "pod-sideEffects", ok: ($p[0].sideEffects == "None")},
            {check: "batchsandbox-sideEffects", ok: ($b[0].sideEffects == "None")},
            {check: "pod-matchPolicy", ok: ($p[0].matchPolicy == "Equivalent")},
            {check: "batchsandbox-matchPolicy", ok: ($b[0].matchPolicy == "Equivalent")},
            {check: "pod-service-name", ok: ($p[0].clientConfig.service.name == $svc)},
            {check: "pod-service-namespace", ok: ($p[0].clientConfig.service.namespace == $ns)},
            {check: "pod-service-path", ok: ($p[0].clientConfig.service.path == "/mutate")},
            {check: "pod-service-port", ok: ($p[0].clientConfig.service.port == 443)},
            {check: "batchsandbox-service-name", ok: ($b[0].clientConfig.service.name == $svc)},
            {check: "batchsandbox-service-namespace", ok: ($b[0].clientConfig.service.namespace == $ns)},
            {check: "batchsandbox-service-path", ok: ($b[0].clientConfig.service.path == "/mutate")},
            {check: "batchsandbox-service-port", ok: ($b[0].clientConfig.service.port == 443)}
        ]' 2>/dev/null || true)

    if [ -z "${contract}" ]; then
        fail D10 "无法解析 MWC ${WV_MWC}"
    else
        total=$(printf '%s' "${contract}" | jq 'length')
        failed=$(printf '%s' "${contract}" | jq -r '.[] | select(.ok == false) | .check' | tr '\n' ',' | sed 's/,$//')
        if [ -z "${failed}" ]; then
            pass D10 "两个 webhook 共 ${total} 项静态契约符合预期（rules/operations=CREATE、sideEffects=None、matchPolicy=Equivalent、service=${WV_SERVICE}.${WV_NAMESPACE}:443/mutate）"
        else
            artifact_save "d10-mwc.json" "${mwc_json}" >/dev/null
            fail D10 "不符合的项: ${failed}（证据 ${ARTIFACT_DIR}/d10-mwc.json）"
        fi
    fi
fi

#==================== 收尾健康检查 ====================#
healthz=$(curl -s -k -o /dev/null -w '%{http_code}' -m 10 "$(admission_url)/healthz" 2>/dev/null || true)
if [ "${healthz}" != "200" ]; then
    log_warn "02 结束后 /healthz 返回 ${healthz}"
    k8s_collect_webhook_artifacts "webhook-after-02" >/dev/null 2>&1 || true
fi

finish "02 降级与安全"

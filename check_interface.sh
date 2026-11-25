#!/usr/bin/env bash
# =============================================================================
# 接口门禁检查（cri-multiplex 专用）
#
# 用法：
#   BASE=<ref> TARGET=<ref> ./check_interface.sh
#   ./check_interface.sh [BASE] [TARGET]
#
# 优先读环境变量 BASE / TARGET（流水线里用 BASE=... TARGET=... 传入），
# 没有环境变量时读两个位置参数，两者都没有时默认 HEAD^ / HEAD。
# BASE / TARGET 是当前仓库里的任意 git ref（分支、tag、commit）。
#
# 背景：e2b 编排栈（packages/*）已从本仓移除，接口门禁只覆盖 KASandbox
# 自有的 cri-multiplex：proto 定义（cri-multiplex/proto）与其生成代码
# （cri-multiplex/pkg 下的 *.pb.go）。
#
# 检查规则：
#   1. 兼容性：proto 变更必须向后兼容 —— 字段号/字段名不得改绑或复用，
#      不得删除字段、枚举值、RPC 或整个文件（除非字段号已声明 reserved）。
#      纯新增（字段/RPC/文件）算通过。
#   2. 同步性：*.pb.go 不得脱离 proto 单独变更（疑似手改生成代码 → 失败）。
#   3. 再生成：proto 变更后必须用 protoc 重新生成 Go 代码。本机装有
#      protoc + protoc-gen-go + protoc-gen-go-grpc 时自动校验产物一致；
#      未安装则告警跳过（兼容性检查不受影响）。
#
# 退出码：
#   0  通过（含无相关变更）
#   1  接口不兼容，或生成代码漂移/手改
#   2  参数、工具或执行出错
# =============================================================================
set -euo pipefail

# ---------------------------------------------------------------------------
# 颜色与基础输出
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    CYAN='\033[0;36m'
    NC='\033[0m'
else
    RED=''
    GREEN=''
    YELLOW=''
    CYAN=''
    NC=''
fi

info() { printf '%b\n' "${CYAN}$*${NC}"; }
ok()   { printf '%b\n' "${GREEN}$*${NC}"; }
warn() { printf '%b\n' "${YELLOW}$*${NC}"; }
err()  { printf '%b\n' "${RED}$*${NC}"; }
die()  { err "$@"; exit 2; }

# ---------------------------------------------------------------------------
# 1. 解析参数，拿到两个 commit
# ---------------------------------------------------------------------------
BASE="${BASE:-${1:-HEAD^}}"
TARGET="${TARGET:-${2:-HEAD}}"

git rev-parse --verify --quiet "${BASE}^{commit}" > /dev/null \
    || die "BASE 不是有效的 commit: ${BASE}"
git rev-parse --verify --quiet "${TARGET}^{commit}" > /dev/null \
    || die "TARGET 不是有效的 commit: ${TARGET}"

BASE=$(git rev-parse "${BASE}^{commit}")
TARGET=$(git rev-parse "${TARGET}^{commit}")

info "接口门禁（cri-multiplex）: ${BASE:0:9}..${TARGET:0:9}"

# ---------------------------------------------------------------------------
# 2. 收集变更文件
# ---------------------------------------------------------------------------
CHANGED=$(git diff --name-only "${BASE}" "${TARGET}" -- cri-multiplex/proto cri-multiplex/pkg || true)

PROTOS_CHANGED=$(grep -E '^cri-multiplex/proto/.*\.proto$' <<< "${CHANGED}" || true)
PB_CHANGED=$(grep -E '^cri-multiplex/pkg/.*\.pb\.go$' <<< "${CHANGED}" || true)

if [ -z "${CHANGED}" ]; then
    ok "cri-multiplex 接口无变更，通过"
    exit 0
fi

# ---------------------------------------------------------------------------
# 3. 同步性：生成代码不得脱离 proto 单独变更
# ---------------------------------------------------------------------------
if [ -n "${PB_CHANGED}" ] && [ -z "${PROTOS_CHANGED}" ]; then
    err "以下生成代码发生变更，但 proto 来源未变（疑似手改，禁止）:"
    sed 's/^/  /' <<< "${PB_CHANGED}"
    err "请修改 cri-multiplex/proto 下对应 .proto 后重新生成"
    exit 1
fi

# ---------------------------------------------------------------------------
# 4. 兼容性：字段号/名映射、删除、RPC 检查
# ---------------------------------------------------------------------------
COMPAT_PY=$(mktemp /tmp/cri-compat-XXXXXX.py)
trap 'rm -f "${COMPAT_PY}"' EXIT

cat > "${COMPAT_PY}" <<'PYEOF'
import re
import subprocess
import sys

def read_at(ref, path):
    r = subprocess.run(
        ["git", "show", f"{ref}:{path}"],
        capture_output=True, text=True,
    )
    return r.stdout if r.returncode == 0 else ""

def parse(text):
    """提取字段/枚举值 号<->名 映射、reserved 号集合、rpc 名集合、service 集合"""
    fields_num2name = {}
    fields_name2num = {}
    reserved = set()
    rpcs = set()
    services = set()
    for line in text.splitlines():
        m = re.match(
            r"^\s*(?:optional\s+|repeated\s+|required\s+)?[\w.]+\s+(\w+)\s*=\s*(\d+)\s*;",
            line,
        )
        if m:
            name, num = m.group(1), int(m.group(2))
            fields_num2name[num] = name
            fields_name2num[name] = num
            continue
        m = re.match(r"^\s*reserved\s+(.+);", line)
        if m:
            for part in m.group(1).split(","):
                part = part.strip().strip('"')
                if "to" in part:
                    lo, hi = part.split("to")
                    reserved.update(range(int(lo), int(hi) + 1))
                elif part.isdigit():
                    reserved.add(int(part))
            continue
        m = re.match(r"^\s*rpc\s+(\w+)\s*\(", line)
        if m:
            rpcs.add(m.group(1))
            continue
        m = re.match(r"^\s*service\s+(\w+)", line)
        if m:
            services.add(m.group(1))
    return fields_num2name, fields_name2num, reserved, rpcs, services

problems = []
for path in sys.argv[3:]:
    base_txt = read_at(sys.argv[1], path)
    tgt_txt = read_at(sys.argv[2], path)
    if base_txt and not tgt_txt:
        problems.append(f"[删除文件] {path}: 整个 proto 文件被删除")
        continue
    b = parse(base_txt)
    t = parse(tgt_txt)
    b_n2m, b_m2n, b_res, b_rpcs, b_svc = b
    t_n2m, t_m2n, t_res, t_rpcs, t_svc = t
    tag = path.split("/", 2)[-1]
    for num, name in b_n2m.items():
        if num in t_n2m and t_n2m[num] != name:
            problems.append(
                f"[字段号复用] {tag}: 号 {num} 由 {name} 改为 {t_n2m[num]}"
            )
        elif num not in t_n2m and num not in t_res:
            problems.append(
                f"[删除字段] {tag}: {name} = {num} 被删除且未声明 reserved"
            )
    for name, num in b_m2n.items():
        if name in t_m2n and t_m2n[name] != num:
            problems.append(
                f"[字段号变更] {tag}: {name} 号 {num} 改为 {t_m2n[name]}"
            )
    for svc in b_svc:
        if svc not in t_svc:
            problems.append(f"[删除服务] {tag}: service {svc} 被删除")
    for rpc in b_rpcs:
        if rpc not in t_rpcs:
            problems.append(f"[删除RPC] {tag}: rpc {rpc} 被删除")

if problems:
    print("\n".join(problems))
    sys.exit(1)
sys.exit(0)
PYEOF

if [ -n "${PROTOS_CHANGED}" ]; then
    info "检查 proto 兼容性..."
    if ! command -v python3 > /dev/null 2>&1; then
        die "兼容性检查需要 python3，但未找到"
    fi
    # shellcheck disable=SC2086
    if ! python3 "${COMPAT_PY}" "${BASE}" "${TARGET}" ${PROTOS_CHANGED}; then
        err "接口不兼容变更:"
        # shellcheck disable=SC2086
        python3 "${COMPAT_PY}" "${BASE}" "${TARGET}" ${PROTOS_CHANGED} | sed 's/^/  /' || true
        exit 1
    fi
    ok "proto 兼容性通过（新增字段/RPC 允许，破坏性的改绑/删除禁止）"
fi

# orchestrator.proto 来自 e2b-infra 上游，单独提醒
if grep -qx "cri-multiplex/proto/orchestrator.proto" <<< "${PROTOS_CHANGED}"; then
    warn "注意: orchestrator.proto 拷贝自 e2b-infra 上游，变更须与上游 orchestrator.proto 保持同步"
fi

# ---------------------------------------------------------------------------
# 5. 再生成校验（protoc 可用时）：变更的 proto 必须能再生出已提交的产物
# ---------------------------------------------------------------------------
if [ -n "${PROTOS_CHANGED}" ]; then
    if command -v protoc > /dev/null 2>&1 \
        && command -v protoc-gen-go > /dev/null 2>&1 \
        && command -v protoc-gen-go-grpc > /dev/null 2>&1; then
        info "校验生成代码与 proto 同步（protoc 再生成比对）..."
        TMPDIR_CRI=$(mktemp -d /tmp/cri-regen-XXXXXX)
        trap 'rm -rf "${TMPDIR_CRI}"; rm -f "${COMPAT_PY}"' EXIT
        cp -a cri-multiplex "${TMPDIR_CRI}/"

        # 只对有 go_package 的 proto 再生成；admin.proto 依赖 orchestrator.proto，
        # 一次性传入全部变更文件，protoc 自动处理 import
        REGEN_FILES=""
        EXPECTED_DIRS=""
        for p in ${PROTOS_CHANGED}; do
            if grep -q "option go_package" "${p}"; then
                REGEN_FILES="${REGEN_FILES} ${p#cri-multiplex/}"
                gp=$(sed -n 's/option go_package = "\([^";]*\).*/\1/p' "${p}")
                gp=${gp#"github.com/cri-multiplex/"}
                gp=${gp%%";"*}
                EXPECTED_DIRS="${EXPECTED_DIRS} ${gp}"
            fi
        done

        if [ -n "${REGEN_FILES}" ]; then
            pushd "${TMPDIR_CRI}/cri-multiplex" > /dev/null
            # shellcheck disable=SC2086
            protoc --experimental_allow_proto3_optional \
                -I proto -I /usr/include \
                --go_out=. --go_opt=module=github.com/cri-multiplex \
                --go-grpc_out=. --go-grpc_opt=module=github.com/cri-multiplex \
                ${REGEN_FILES}
            popd > /dev/null

            drift=0
            for d in ${EXPECTED_DIRS}; do
                while IFS= read -r f; do
                    rel="${f#"${TMPDIR_CRI}/cri-multiplex/"}"
                    if ! git show "${TARGET}:cri-multiplex/${rel}" > /dev/null 2>&1; then
                        err "  生成产物在提交中不存在: cri-multiplex/${rel}"
                        drift=1
                        continue
                    fi
                    if ! diff -q "${f}" <(git show "${TARGET}:cri-multiplex/${rel}") > /dev/null 2>&1; then
                        err "  生成代码与 proto 不同步: cri-multiplex/${rel}"
                        drift=1
                    fi
                done < <(find "${TMPDIR_CRI}/cri-multiplex/${d}" -name "*.pb.go" -type f)
            done
            if [ "${drift}" -ne 0 ]; then
                err "请在 cri-multiplex/ 下重新生成并提交（命令见 cri-multiplex/README.md 与 AGENTS.md）"
                exit 1
            fi
            ok "生成代码与 proto 同步"
        else
            warn "变更的 proto 均无 go_package，跳过再生成校验"
        fi
    else
        warn "未找到 protoc/protoc-gen-go/protoc-gen-go-grpc，跳过再生成校验（仅完成兼容性检查）"
    fi
fi

ok "cri-multiplex 接口门禁通过"
exit 0

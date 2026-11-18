#!/usr/bin/env bash
# =============================================================================
# 接口门禁检查
#
# 用法：
#   BASE=<ref> TARGET=<ref> ./门禁-v2.sh
#   ./门禁-v2.sh <BASE> <TARGET>
#
#   优先读环境变量 BASE / TARGET（流水线里用 BASE=... TARGET=... 传入），
#   没有环境变量时读两个位置参数，两者都没有时默认 HEAD^ / HEAD。
#   BASE / TARGET 是当前仓库里的任意 git ref（分支、tag、commit）。
#   只做比较，不 clone，也不改当前工作区。
#
# 检查步骤：
#   1. 解析参数，拿到两个 commit
#   2. 准备 buf / oasdiff，没有就自动 go install
#   3. 找出两个 commit 之间改了哪些文件
#   4. proto：用 buf 比较，接口新增、删除、改坏都算失败
#   5. OpenAPI：用 oasdiff 比较，接口增删改都算失败（只改 tag 不算）
#   6. 生成文件：生成文件变了、但来源（proto/spec）没变，算疑似手改
#   7. 生成 proto：用 buf generate 重放，忽略注释/版本行后比对
#   8. 生成代码：重新生成一遍，和提交的生成物不一致就算失败
#   9. 汇总结果；失败时打印文件、行号、原因
#       每个阶段开始打印标题，结束打印通过/不通过/异常和耗时
#
# 使用的工具：
#   buf      proto 编译和接口兼容性检查
#   oasdiff  OpenAPI 接口变更检查
#   protoc-gen-go / protoc-gen-connect-go / protoc-gen-go-grpc
#            proto 生成插件，按目录锁定版本
#   go       安装上面的工具、执行 go generate
#   git      读文件内容、做生成检查用的临时 worktree
#
# 退出码：
#   0  通过
#   1  接口有变更，或生成代码没同步
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
bad()  { printf '%b\n' "${RED}$*${NC}" >&2; }
die()  { bad "错误: $*"; exit 2; }

# 脚本中会用到工具缓存目录里的绝对路径。
abspath() {
    case "$1" in
        /*) printf '%s' "$1" ;;
        *)  printf '%s/%s' "$PWD" "${1#./}" ;;
    esac
}

# ---------------------------------------------------------------------------
# 耗时统计
#
# 每个步骤结束时打印一行 "[耗时] <步骤>: <时长>"，方便定位门禁慢在哪。
# 优先用 bash 5 的 EPOCHREALTIME（微秒精度），否则回退 date。
# ---------------------------------------------------------------------------
now_us() {
    local t="${EPOCHREALTIME:-}"

    if [ -n "$t" ]; then
        t="${t/,/.}"
        printf '%s' "$(( ${t%.*} * 1000000 + 10#${t#*.} ))"
    elif date +%s%N >/dev/null 2>&1; then
        printf '%s' "$(( $(date +%s%N) / 1000 ))"
    else
        printf '%s' "$(( $(date +%s) * 1000000 ))"
    fi
}

fmt_duration() {
    local us="$1"

    if [ "$us" -ge 1000000 ]; then
        printf '%d.%02ds' "$((us / 1000000))" "$(((us % 1000000) / 10000))"
    elif [ "$us" -ge 1000 ]; then
        printf '%d.%02dms' "$((us / 1000))" "$(((us % 1000) / 10))"
    else
        printf '%dus' "$us"
    fi
}

# timed <标签> <命令...>：执行命令并打印其耗时，命令的退出码原样返回。
timed() {
    local label="$1"
    shift
    local t0 t1 rc=0

    t0="$(now_us)"
    "$@" || rc=$?
    t1="$(now_us)"
    info "[耗时] ${label}: $(fmt_duration "$((t1 - t0))")"
    return "$rc"
}

TOTAL_T0="$(now_us)"
print_total() {
    info "[耗时] 总耗时: $(fmt_duration "$(( $(now_us) - TOTAL_T0 ))")"
}

# ---------------------------------------------------------------------------
# 阶段输出：开始打印标题，结束打印结果
# ---------------------------------------------------------------------------
STAGE_T0=0
STAGE_F0=0
STAGE_E0=0

stage_begin() {
    STAGE_T0="$(now_us)"
    STAGE_F0="${#FAILURES[@]}"
    STAGE_E0="${#ERRORS[@]}"
    printf '\n'
    info "==================== $1 ===================="
}

stage_end() {
    local us df de

    us="$(( $(now_us) - STAGE_T0 ))"
    df=$(( ${#FAILURES[@]} - STAGE_F0 ))
    de=$(( ${#ERRORS[@]} - STAGE_E0 ))

    if [ "$df" -eq 0 ] && [ "$de" -eq 0 ]; then
        ok "结果: 通过（$(fmt_duration "$us")）"
    elif [ "$de" -gt 0 ]; then
        bad "结果: 异常（失败 $df，错误 $de，$(fmt_duration "$us")）"
    else
        bad "结果: 不通过（失败 $df，$(fmt_duration "$us")）"
    fi
}

stage() {
    local label="$1"
    shift
    local rc=0

    stage_begin "$label"
    "$@" || rc=$?
    stage_end
    return "$rc"
}

FAILURES=()
ERRORS=()

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/api-gate.XXXXXX")"
GEN_WORKTREE=""

remove_worktree() {
    local wt="$1"

    if [ -z "$wt" ] || [ ! -d "$wt" ]; then
        return 0
    fi

    git worktree remove --force "$wt" >/dev/null 2>&1 || {
        rm -rf "$wt"
        git worktree prune >/dev/null 2>&1 || true
    }
}

cleanup() {
    remove_worktree "$GEN_WORKTREE"
    rm -rf "$TMP_DIR"
}
trap cleanup EXIT

# oasdiff 的 tag 是接口分组元数据，不属于接口契约；仅改 tag 不应触发门禁。
# severity-levels 文件只接受 "<check-id> <LEVEL>"，不要写注释。
OASDIFF_SEVERITY_FILE="$TMP_DIR/oasdiff-severity.txt"
cat > "$OASDIFF_SEVERITY_FILE" <<'EOF'
api-tag-added NONE
api-tag-removed NONE
EOF

# ---------------------------------------------------------------------------
# 工具安装
#
# 容器环境不预装 buf/oasdiff。脚本统一用 go install 安装到缓存目录：
#   - go install 自带版本校验，不依赖 GitHub Release 的下载速度和资产命名；
#   - 缓存按版本命名，已存在且 --version 通过时直接复用，不重复编译。
# 因此容器内必须已安装 Go。安装失败属于工具错误，脚本会直接以退出码 2
# 终止，不能继续执行后续检查，否则缺失工具会被误判为“无接口变更”。
# ---------------------------------------------------------------------------
BUF_VERSION="v1.75.0"
OASDIFF_VERSION="v1.35.0"
BUF_PKG="github.com/bufbuild/buf/cmd/buf"
OASDIFF_PKG="github.com/oasdiff/oasdiff"

# proto 生成插件：按仓库已提交生成物的版本锁定
PROTO_GO_VERSION="v1.28.1"
PROTO_GO_LEGACY_VERSION="v1.36.6"
PROTO_GRPC_VERSION="v1.6.1"
PROTO_CONNECT_VERSION="v1.21.1"
PROTO_GO_V2_VERSION="v1.36.11"
PROTO_GRPC_V2_VERSION="v1.6.2"

# cri-multiplex 的 proto 没有 buf 配置，按文件单独建临时模块生成。
# 文件|源 proto|输出目录|protoc-gen-go 版本|protoc-gen-go-grpc 版本
CRI_PROTO_JOBS=(
    "admin.proto|admin.proto|cri-multiplex/pkg/admin|$PROTO_GO_VERSION|$PROTO_GRPC_VERSION"
    "admin_v2.proto|admin_v2.proto|cri-multiplex/pkg/admin/v2|$PROTO_GO_V2_VERSION|$PROTO_GRPC_VERSION"
    "orchestrator.proto|orchestrator.proto|cri-multiplex/pkg/orchestrator|$PROTO_GO_VERSION|$PROTO_GRPC_VERSION"
    "envd_process.proto|envd/process.proto|cri-multiplex/pkg/envd/process|$PROTO_GO_V2_VERSION|$PROTO_GRPC_V2_VERSION"
)

if [ -n "${XDG_CACHE_HOME:-}" ] && mkdir -p "${XDG_CACHE_HOME}/api-gate" 2>/dev/null; then
    TOOL_DIR="${XDG_CACHE_HOME}/api-gate"
elif [ -n "${HOME:-}" ] && mkdir -p "${HOME}/.cache/api-gate" 2>/dev/null; then
    TOOL_DIR="${HOME}/.cache/api-gate"
else
    TOOL_DIR="${TMPDIR:-/tmp}/api-gate-tools"
fi
mkdir -p "$TOOL_DIR" 2>/dev/null || die "无法创建工具缓存目录: $TOOL_DIR"
PROTO_PLUGIN_DIR="$TOOL_DIR/proto-plugins"

RESOLVED_TOOL=""

install_tool() {
    local name="$1"
    local pkg="$2"
    local version="$3"
    local dest="$TOOL_DIR/$name-${version#v}"
    local built="$TOOL_DIR/$name"
    local log="$TOOL_DIR/.install-${name}.log"

    info "[install] 未找到 $name，使用 go install 安装 ${version} ..."
    if GOBIN="$TOOL_DIR" GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
        go install "${pkg}@${version}" >"$log" 2>&1 \
        && [ -x "$built" ] \
        && mv -f "$built" "$dest"; then
        rm -f "$log"
        return 0
    fi

    bad "[install] $name 安装失败（${pkg}@${version}）："
    tail -20 "$log" 2>/dev/null | sed 's/^/    /' >&2 || true
    rm -f "$built"
    return 1
}

# 检查缓存的工具是否真的可执行。
tool_works() {
    local path="$1"

    [ -x "$path" ] || return 1
    "$path" --version >/dev/null 2>&1
}

resolve_tool() {
    local name="$1"
    local pkg="$2"
    local version="$3"
    local cached="$TOOL_DIR/${name}-${version#v}"

    if command -v "$name" >/dev/null 2>&1; then
        RESOLVED_TOOL="$(abspath "$(command -v "$name")")"
        return 0
    fi

    if tool_works "$cached"; then
        RESOLVED_TOOL="$cached"
        return 0
    fi

    install_tool "$name" "$pkg" "$version" || return 1

    if tool_works "$cached"; then
        RESOLVED_TOOL="$cached"
        return 0
    fi
    return 1
}

ensure_tools() {
    if ! command -v go >/dev/null 2>&1; then
        bad "未找到 go。buf / oasdiff 安装与生成代码检查都需要 Go，请在容器中安装。"
        return 2
    fi

    if ! resolve_tool buf "$BUF_PKG" "$BUF_VERSION"; then
        cat >&2 <<EOF
${RED}buf 安装失败（${BUF_PKG}@${BUF_VERSION}）。${NC}
工具缓存目录: ${TOOL_DIR}
请检查 Go 模块代理（GOPROXY）与网络后重试。
EOF
        return 2
    fi
    BUF_BIN="$RESOLVED_TOOL"

    if ! resolve_tool oasdiff "$OASDIFF_PKG" "$OASDIFF_VERSION"; then
        cat >&2 <<EOF
${RED}oasdiff 安装失败（${OASDIFF_PKG}@${OASDIFF_VERSION}）。${NC}
工具缓存目录: ${TOOL_DIR}
请检查 Go 模块代理（GOPROXY）与网络后重试。
EOF
        return 2
    fi
    OASDIFF_BIN="$RESOLVED_TOOL"
}

# ---------------------------------------------------------------------------
# proto 检查
# ---------------------------------------------------------------------------

# 为一个 proto 目录生成 buf 模块。
# 只复制该目录下的直接 .proto 文件，避免把子目录中同名 package（例如
# cri-multiplex/proto 与 cri-multiplex/proto/envd 都声明 package process）
# 混进同一个模块导致 buf 编译失败。
stage_proto_module() {
    local ref="$1"
    local dir="$2"
    local dest="$3"
    local f

    mkdir -p "$dest"
    # 语言/代码生成类 option（go_package、java_package 等）不属于 RPC 契约，
    # 用 except 排除，避免只调整生成路径时误报接口变更。
    cat > "$dest/buf.yaml" <<'EOF'
version: v1
breaking:
  use:
    - FILE
  except:
    - FILE_SAME_CC_ENABLE_ARENAS
    - FILE_SAME_CC_GENERIC_SERVICES
    - FILE_SAME_CSHARP_NAMESPACE
    - FILE_SAME_GO_PACKAGE
    - FILE_SAME_JAVA_GENERIC_SERVICES
    - FILE_SAME_JAVA_MULTIPLE_FILES
    - FILE_SAME_JAVA_OUTER_CLASSNAME
    - FILE_SAME_JAVA_PACKAGE
    - FILE_SAME_OBJC_CLASS_PREFIX
    - FILE_SAME_OPTIMIZE_FOR
    - FILE_SAME_PHP_CLASS_PREFIX
    - FILE_SAME_PHP_METADATA_NAMESPACE
    - FILE_SAME_PHP_NAMESPACE
    - FILE_SAME_PY_GENERIC_SERVICES
    - FILE_SAME_RUBY_PACKAGE
    - FILE_SAME_SWIFT_PREFIX
EOF

    while IFS= read -r f; do
        [ -n "$f" ] || continue
        [ "$(dirname "$f")" = "$dir" ] || continue
        case "$f" in
            */vendor/*|vendor/*) continue ;;
        esac
        git show "${ref}:${f}" > "$dest/$(basename "$f")"
    done < <(git ls-tree -r --name-only "$ref" | grep '\.proto$' || true)
}

count_protos() {
    find "$1" -maxdepth 1 -type f -name '*.proto' | wc -l | tr -d ' '
}

# 把 buf breaking 的输出整理成 "文件:行 + 原因"，方便直接定位。
# buf 的错误格式是 "<file>:<line>:<col>:<message>"；整文件新增/删除时文件名
# 显示为 <input>，真实文件名在 message 的引号里。
report_proto_findings() {
    local dir="$1"
    local output="$2"
    local line file rest pos msg quoted

    printf '%s\n' "$output" | while IFS= read -r line; do
        [ -n "$line" ] || continue
        case "$line" in
            *:*:*:*)
                file="${line%%:*}"
                rest="${line#*:}"
                pos="${rest%%:*}"
                rest="${rest#*:}"
                msg="${rest#*:}"

                if [ "$file" = "<input>" ]; then
                    quoted="$(printf '%s' "$msg" | sed -n 's/.*"\([^"]*\)".*/\1/p')"
                    if [ -n "$quoted" ]; then
                        file="$quoted"
                    fi
                fi

                printf '    文件: %s/%s:%s\n' "$dir" "$file" "$pos"
                printf '    原因: %s\n' "$msg"
                ;;
            *)
                printf '    %s\n' "$line"
                ;;
        esac
    done
}

check_proto_module() {
    local dir="$1"
    local index="${2:-0}"
    local slug="m${index}"

    local base_dir="$TMP_DIR/proto/base/$slug"
    local head_dir="$TMP_DIR/proto/head/$slug"

    stage_proto_module "$BASE_SHA" "$dir" "$base_dir"
    stage_proto_module "$TARGET_SHA" "$dir" "$head_dir"

    local base_count head_count
    base_count="$(count_protos "$base_dir")"
    head_count="$(count_protos "$head_dir")"

    if [ "$base_count" -eq 0 ] && [ "$head_count" -eq 0 ]; then
        return 0
    fi

    if [ "$base_count" -eq 0 ]; then
        FAILURES+=("proto 新增接口模块: $dir")
        bad "✗ [$dir] 新增 proto 接口模块（BASE 中无 proto 文件）"
        printf '    TARGET 中新增 %s 个 proto 文件\n' "$head_count"
        return 0
    fi

    if [ "$head_count" -eq 0 ]; then
        FAILURES+=("proto 删除接口模块: $dir")
        bad "✗ [$dir] 删除 proto 接口模块（TARGET 中已无 proto 文件）"
        printf '    BASE 中曾有 %s 个 proto 文件\n' "$base_count"
        return 0
    fi

    # 先分别编译两侧模块。buf 对编译错误也返回 100，必须先用 buf build
    # 区分“解析失败”和“真正的 breaking change”。
    local build_out
    if ! build_out="$(cd "$base_dir" && "$BUF_BIN" build 2>&1)"; then
        ERRORS+=("buf build 失败: BASE/$dir")
        bad "✗ [$dir] BASE 版本 proto 无法编译："
        printf '%s\n' "$build_out" | sed 's/^/    /'
        return 0
    fi
    if ! build_out="$(cd "$head_dir" && "$BUF_BIN" build 2>&1)"; then
        ERRORS+=("buf build 失败: TARGET/$dir")
        bad "✗ [$dir] TARGET 版本 proto 无法编译："
        printf '%s\n' "$build_out" | sed 's/^/    /'
        return 0
    fi

    # 正向：TARGET vs BASE，检查删除 / 不兼容修改。
    local normal_out normal_rc
    normal_out="$(cd "$head_dir" && "$BUF_BIN" breaking --against "$base_dir" 2>&1)" && normal_rc=0 || normal_rc=$?

    # 反向：BASE vs TARGET，检查 TARGET 新增的 service/rpc/message/field/enum 等。
    # 单纯新增在正向检查中是“兼容变更”，不会报错；反向检查会把它当作
    # BASE 缺失该定义，从而报出 deleted。
    local reverse_out reverse_rc
    reverse_out="$(cd "$base_dir" && "$BUF_BIN" breaking --against "$head_dir" 2>&1)" && reverse_rc=0 || reverse_rc=$?

    if [ "$normal_rc" -ne 0 ] && [ "$normal_rc" -ne 100 ]; then
        ERRORS+=("buf breaking 执行失败: $dir（正向，exit $normal_rc）")
        bad "✗ [$dir] buf breaking 执行失败（正向，exit $normal_rc）："
        report_proto_findings "$dir" "$normal_out"
        return 0
    fi
    if [ "$reverse_rc" -ne 0 ] && [ "$reverse_rc" -ne 100 ]; then
        ERRORS+=("buf breaking 执行失败: $dir（反向，exit $reverse_rc）")
        bad "✗ [$dir] buf breaking 执行失败（反向，exit $reverse_rc）："
        report_proto_findings "$dir" "$reverse_out"
        return 0
    fi

    local normal_break=0 reverse_break=0
    [ "$normal_rc" -eq 100 ] && normal_break=1
    [ "$reverse_rc" -eq 100 ] && reverse_break=1

    if [ "$normal_break" -eq 0 ] && [ "$reverse_break" -eq 0 ]; then
        ok "✓ [$dir] 无 proto 接口变更"
        return 0
    fi

    # 正向有差异说明存在删除/不兼容修改，直接报正向结果；
    # 只有正向无差异、反向有差异时，才是纯新增。
    if [ "$normal_break" -eq 1 ]; then
        FAILURES+=("proto 接口删除/不兼容修改: $dir")
        bad "✗ [$dir] 检测到 proto 接口删除或不兼容修改"
        report_proto_findings "$dir" "$normal_out"
    else
        FAILURES+=("proto 接口新增: $dir")
        bad "✗ [$dir] 检测到 proto 接口新增"
        report_proto_findings "$dir" "$reverse_out"
        echo "  注：反向输出中的 deleted 表示 TARGET 新增了该定义。"
    fi
}

check_proto() {
    local -a proto_files=()
    local -a dirs=()
    local f

    for f in "${CHANGED[@]}"; do
        case "$f" in
            *.proto) proto_files+=("$f") ;;
        esac
    done

    if [ "${#proto_files[@]}" -eq 0 ]; then
        info "[proto] 未发现 .proto 变更"
        return 0
    fi

    info "[proto] 发现 ${#proto_files[@]} 个 .proto 变更，用 buf breaking 比较接口兼容性"
    for f in "${proto_files[@]}"; do
        dirs+=("$(dirname "$f")")
    done
    mapfile -t dirs < <(printf '%s\n' "${dirs[@]}" | sort -u)

    local index=0
    for f in "${dirs[@]}"; do
        timed "[proto] $f" check_proto_module "$f" "$index"
        index=$((index + 1))
    done
}

# ---------------------------------------------------------------------------
# OpenAPI 检查
# ---------------------------------------------------------------------------
is_openapi_ref() {
    local ref="$1"
    local path="$2"
    local content

    git cat-file -e "${ref}:${path}" 2>/dev/null || return 1
    # 不能只看文件前 N 行：许可证头、说明注释可能长达几十甚至上百行，
    # 真正的 openapi 字段会被截断掉，导致接口文件被误判为普通 YAML。
    # 这里扫描整个文件，只匹配顶格的 openapi 键（注释行以 # 开头，不会命中）。
    content="$(git show "${ref}:${path}" 2>/dev/null)" || return 1
    printf '%s\n' "$content" | grep -E "^openapi[[:space:]]*:[[:space:]]*[\"']?[0-9]" >/dev/null
}

# 把 oasdiff 的变更类型翻译成中文，例如 request-property-removed -> 删除请求体字段。
openapi_reason() {
    local id="$1"
    local action="${id##*-}"
    local subject="${id%-*}"

    case "$action" in
        added)            action="新增" ;;
        removed|deleted)  action="删除" ;;
        changed|updated)  action="修改" ;;
        deprecated)       action="废弃" ;;
        *)                action="" ;;
    esac

    case "$subject" in
        endpoint|api-path)   subject="接口" ;;
        api-operation)       subject="操作" ;;
        api-schema)          subject="schema" ;;
        request-property)    subject="请求体字段" ;;
        response-property)   subject="响应字段" ;;
        request-parameter)   subject="请求参数" ;;
        request-header)      subject="请求头" ;;
        response-header)     subject="响应头" ;;
        api-tag)             subject="tag" ;;
        *)                   subject="" ;;
    esac

    if [ -z "$action" ] || [ -z "$subject" ]; then
        printf '%s' "$id"
    elif [ "$subject" = "schema" ] || [ "$subject" = "tag" ]; then
        printf '%s %s' "$action" "$subject"
    else
        printf '%s%s' "$action" "$subject"
    fi
}

# 删除类变更 oasdiff 不给行号，按元素名到对应版本的文件里找第一处定义。
locate_line() {
    local file="$1"
    local key="$2"

    [ -f "$file" ] || return 1
    [ -n "$key" ] || return 1

    awk -v key="$key" '
        {
            line = $0
            sub(/^[[:space:]]+/, "", line)
            if (line == key ":") { print NR; exit }
        }
    ' "$file"
}

# 逐条打印 oasdiff 的变更：文件、行号、原因。
# 输入是 --format githubactions 的输出：
#   ::warning title=<id>,file=<path>,line=<n>,col=<n>::<message>
report_oasdiff_findings() {
    local f="$1"
    local output="$2"
    local line body meta message title file lineno where name search

    local base_file="$TMP_DIR/openapi/base/$f"
    local head_file="$TMP_DIR/openapi/head/$f"

    printf '%s\n' "$output" | while IFS= read -r line; do
        case "$line" in
            ::*) ;;
            *) continue ;;
        esac

        body="${line#::}"
        meta="${body%%::*}"
        message="${body#*::}"

        title="$(printf '%s' "$meta" | sed -n 's/.*title=\([^,]*\).*/\1/p')"
        file="$(printf '%s' "$meta" | sed -n 's/.*file=\([^,]*\).*/\1/p')"
        lineno="$(printf '%s' "$meta" | sed -n 's/.*line=\([0-9]*\).*/\1/p')"

        if [ -n "$lineno" ]; then
            case "$file" in
                "$TMP_DIR"/openapi/base/*) where="BASE" ;;
                *)                         where="TARGET" ;;
            esac
        else
            name="$(printf '%s' "$message" | sed -n 's/.*`\([^`]*\)`.*/\1/p')"
            case "$message" in
                *removed*|*deleted*)
                    where="BASE"
                    search="$base_file" ;;
                *)
                    where="TARGET"
                    search="$head_file" ;;
            esac
            lineno="$(locate_line "$search" "$name" || true)"
        fi

        printf '    文件: %s %s:%s\n' "$where" "$f" "${lineno:-?}"
        printf '    原因: [%s] %s\n' "$title" "$(openapi_reason "$title")"
        printf '    详情: %s\n' "$message"
    done
}

check_openapi_file() {
    local f="$1"
    local base_file="$TMP_DIR/openapi/base/$f"
    local head_file="$TMP_DIR/openapi/head/$f"

    mkdir -p "$(dirname "$base_file")" "$(dirname "$head_file")"
    git show "${BASE_SHA}:${f}" > "$base_file"
    git show "${TARGET_SHA}:${f}" > "$head_file"

    local out rc count
    out="$("$OASDIFF_BIN" changelog "$base_file" "$head_file" \
        --format githubactions \
        --fail-on INFO \
        --severity-levels "$OASDIFF_SEVERITY_FILE" 2>&1)" && rc=0 || rc=$?

    if [ "$rc" -eq 0 ]; then
        ok "✓ [$f] 无 OpenAPI 接口变更"
        return 0
    fi

    if [ "$rc" -eq 1 ]; then
        count="$(printf '%s\n' "$out" | grep -c '^::' || true)"
        FAILURES+=("OpenAPI 接口变更: $f（${count} 处）")
        bad "✗ [$f] 检测到 OpenAPI 接口新增 / 删除 / 修改（${count} 处）"
        report_oasdiff_findings "$f" "$out"
        return 0
    fi

    ERRORS+=("oasdiff 执行失败: $f（exit $rc）")
    bad "✗ [$f] oasdiff 执行失败（exit $rc）："
    printf '%s\n' "$out" | sed 's/^/    /'
}

check_openapi() {
    local -a yaml_files=()
    local f
    local base_has head_has base_openapi head_openapi

    for f in "${CHANGED[@]}"; do
        case "$f" in
            *.yml|*.yaml) yaml_files+=("$f") ;;
        esac
    done

    if [ "${#yaml_files[@]}" -eq 0 ]; then
        info "[openapi] 未发现 .yml/.yaml 变更"
        return 0
    fi

    info "[openapi] 用 oasdiff 比较变更的 OpenAPI 文件（判定规则：接口增删改都算失败）"

    for f in "${yaml_files[@]}"; do
        base_has=0
        head_has=0
        base_openapi=0
        head_openapi=0

        if git cat-file -e "${BASE_SHA}:${f}" 2>/dev/null; then
            base_has=1
        fi
        if git cat-file -e "${TARGET_SHA}:${f}" 2>/dev/null; then
            head_has=1
        fi

        if [ "$base_has" -eq 1 ] && is_openapi_ref "$BASE_SHA" "$f"; then
            base_openapi=1
        fi
        if [ "$head_has" -eq 1 ] && is_openapi_ref "$TARGET_SHA" "$f"; then
            head_openapi=1
        fi

        if [ "$base_openapi" -eq 1 ] && [ "$head_openapi" -eq 1 ]; then
            timed "[openapi] $f" check_openapi_file "$f"
        elif [ "$head_openapi" -eq 1 ]; then
            FAILURES+=("OpenAPI 新增/替换接口文件: $f")
            bad "✗ [$f] OpenAPI 接口文件新增或被替换为 OpenAPI"
        elif [ "$base_openapi" -eq 1 ]; then
            FAILURES+=("OpenAPI 删除/替换接口文件: $f")
            bad "✗ [$f] OpenAPI 接口文件删除或被替换为非 OpenAPI"
        else
            info "[openapi] 跳过（不是 OpenAPI 规范）: $f"
        fi
    done
}

# ---------------------------------------------------------------------------
# 生成文件检测（被动）
#
# Go 的生成文件有官方约定：文件头出现 "// Code generated ... DO NOT EDIT."。
# 如果生成文件变了、但它的来源（proto / OpenAPI spec / 生成配置）没变，
# 说明可能是手改了生成文件，或者生成器版本漂移，需要人工确认。
# 来源按以下顺序解析：
#   1. pb.go 头部的 "// source: xxx.proto"
#   2. 同目录 generate.go 里 //go:generate 引用的 .yml/.yaml/.proto/.sql
#   3. sqlc：任意 .sql 变更
#   4. 都找不到 -> 来源未知，只提示不失败
# ---------------------------------------------------------------------------
TREE_FILES=()

load_tree_files() {
    if [ "${#TREE_FILES[@]}" -gt 0 ]; then
        return 0
    fi
    mapfile -t TREE_FILES < <(git ls-tree -r --name-only "$TARGET_SHA")
}

file_at() {
    local ref="$1"
    local f="$2"

    git show "${ref}:${f}" 2>/dev/null
}

is_generated_file() {
    local f="$1"
    local ref="$2"
    local head

    head="$(file_at "$ref" "$f" | sed -n '1,20p' || true)"
    printf '%s\n' "$head" | grep -E '^// Code generated .* DO NOT EDIT\.$' >/dev/null
}

# 先查变更列表（能覆盖被删除的来源），再查 TARGET 的完整文件树。
find_source_by_suffix() {
    local name="$1"
    local p

    for p in "${CHANGED[@]}"; do
        case "$p" in
            "$name"|*/"$name")
                printf '%s\n' "$p"
                return 0
                ;;
        esac
    done

    for p in "${TREE_FILES[@]}"; do
        case "$p" in
            "$name"|*/"$name")
                printf '%s\n' "$p"
                return 0
                ;;
        esac
    done
    return 1
}

normalize_path() {
    local dir="$1"
    local rel="$2"

    realpath -m --relative-to="$PWD" "$dir/$rel" 2>/dev/null \
        || printf '%s/%s' "$dir" "$rel"
}

generated_sources() {
    local f="$1"
    local dir="$2"
    local ref="$3"
    local name src gen line tok

    # 1) pb.go 头部自带 source: xxx.proto
    name="$(file_at "$ref" "$f" \
        | sed -n '1,20p' \
        | sed -n 's|^// source: \(.*\)$|\1|p' \
        | head -1 || true)"
    if [ -n "$name" ]; then
        if src="$(find_source_by_suffix "$name")"; then
            printf '%s\n' "$src"
            return 0
        fi
    fi

    # 2) 同目录 generate.go 的 //go:generate
    gen="$dir/generate.go"
    if git cat-file -e "${ref}:${gen}" 2>/dev/null; then
        while IFS= read -r line; do
            for tok in $line; do
                case "$tok" in
                    *.yml|*.yaml|*.proto|*.sql)
                        normalize_path "$dir" "$tok"
                        ;;
                esac
            done
        done < <(file_at "$ref" "$gen" | grep -E '^//go:generate' || true)
    fi
    return 0
}

check_generated_files() {
    local -a gen_files=()
    local -a sources=()
    local f src c ref changed

    for f in "${CHANGED[@]}"; do
        case "$f" in
            *.go) ;;
            *) continue ;;
        esac

        # 文件被删除时看 BASE 版本，避免漏掉“删了生成文件”的情况。
        ref="$TARGET_SHA"
        if ! git cat-file -e "${TARGET_SHA}:${f}" 2>/dev/null; then
            ref="$BASE_SHA"
        fi

        if is_generated_file "$f" "$ref"; then
            gen_files+=("$f")
        fi
    done

    if [ "${#gen_files[@]}" -eq 0 ]; then
        info "[generated/files] 未发现生成文件变更"
        return 0
    fi

    info "[generated/files] 发现 ${#gen_files[@]} 个生成文件变更，检查是否随契约更新"
    load_tree_files

    for f in "${gen_files[@]}"; do
        ref="$TARGET_SHA"
        if ! git cat-file -e "${TARGET_SHA}:${f}" 2>/dev/null; then
            ref="$BASE_SHA"
        fi

        mapfile -t sources < <(generated_sources "$f" "$(dirname "$f")" "$ref")

        # sqlc 的输入是 .sql，不在 go:generate 里，用任意 .sql 变更兜底。
        case "$f" in
            */queries/*.go)
                for src in "${CHANGED[@]}"; do
                    case "$src" in
                        *.sql) sources+=("$src") ;;
                    esac
                done
                ;;
        esac

        if [ "${#sources[@]}" -eq 0 ]; then
            info "[generated/files] 来源未知，跳过: $f"
            continue
        fi

        changed=0
        for src in "${sources[@]}"; do
            for c in "${CHANGED[@]}"; do
                if [ "$c" = "$src" ]; then
                    changed=1
                    break
                fi
            done
            if [ "$changed" -eq 1 ]; then
                break
            fi
        done

        if [ "$changed" -eq 1 ]; then
            info "[generated/files] 生成文件随来源更新: $f"
            continue
        fi

        FAILURES+=("生成文件被直接修改（来源未变）: $f")
        bad "✗ [generated/files] $f 变更了，但来源没有变更"
        printf '    来源: %s\n' "${sources[*]}"
        echo "    说明: 生成文件不应手改；如确属生成器版本升级，请说明后合入。"
    done
}

# ---------------------------------------------------------------------------
# proto 生成检查（buf generate + 重新生成比对）
#
# 全部改用 buf generate，避免依赖 protoc 二进制。插件版本按目录锁定：
#   packages/envd/spec                     protoc-gen-go v1.28.1 + connect v1.21.1
#   packages/envd/internal/services/legacy protoc-gen-go v1.36.6
#   packages/orchestrator                  protoc-gen-go v1.28.1 + grpc v1.6.1
# 比对时忽略注释行（含 "// protoc vX" 版本行），只看代码差异。
# ---------------------------------------------------------------------------
install_proto_plugin() {
    local name="$1"
    local pkg="$2"
    local version="$3"
    local dest="$PROTO_PLUGIN_DIR/$name-${version#v}"
    local log="$PROTO_PLUGIN_DIR/.install-$name.log"

    if [ -x "$dest" ] && "$dest" --version 2>/dev/null | grep -q "${version#v}"; then
        printf '%s' "$dest"
        return 0
    fi

    mkdir -p "$PROTO_PLUGIN_DIR"
    info "  - 安装 $name $version ..." >&2
    if GOBIN="$PROTO_PLUGIN_DIR" GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" \
        go install "${pkg}@${version}" >"$log" 2>&1 \
        && [ -x "$PROTO_PLUGIN_DIR/$name" ] \
        && mv -f "$PROTO_PLUGIN_DIR/$name" "$dest"; then
        rm -f "$log"
        printf '%s' "$dest"
        return 0
    fi

    bad "  - $name 安装失败（${pkg}@${version}）"
    tail -10 "$log" 2>/dev/null | sed 's/^/      /' >&2 || true
    rm -f "$PROTO_PLUGIN_DIR/$name"
    return 1
}

# 建一个只包含指定插件的 bin 目录，并放到 PATH 最前面。
setup_proto_path() {
    local dir="$TMP_DIR/proto-bin"
    local pair name bin

    rm -rf "$dir"
    mkdir -p "$dir"
    for pair in "$@"; do
        name="${pair%%|*}"
        bin="${pair#*|}"
        ln -sf "$bin" "$dir/$name"
    done
    PATH="$dir:$PATH"
    export PATH
}

proto_gen_diff() {
    git -C "$GEN_WORKTREE" diff "$@" \
        | grep -E '^[+-]' \
        | grep -vE '^(---|\+\+\+)' \
        | grep -vE '^[+-][[:space:]]*//' \
        | grep -vE '^[+-][[:space:]]*$' || true
}

run_buf_generate() {
    local dir="$1"
    shift
    local out

    if ! out="$(cd "$GEN_WORKTREE/$dir" && "$BUF_BIN" generate "$@" 2>&1)"; then
        ERRORS+=("buf generate 失败: $dir $*")
        bad "✗ [generated/proto] buf generate 失败: $dir $*"
        printf '%s\n' "$out" | sed 's/^/    /'
        return 1
    fi
    return 0
}

write_orchestrator_buf_gen() {
    local file="$1"
    local out="$2"

    cat > "$file" <<EOF
version: v1
plugins:
  - plugin: go
    out: $out
    opt: paths=source_relative
  - plugin: go-grpc
    out: $out
    opt: paths=source_relative
EOF
}

# cri-multiplex 的单个 proto：建临时模块（copy 源文件 + 同目录依赖），
# 用指定版本的插件生成到 worktree 的目标目录。
generate_cri_proto() {
    local job="$1"
    local name rel outdir gover grpcver
    local dir gobin grpcbin out

    IFS='|' read -r name rel outdir gover grpcver <<< "$job"
    dir="$TMP_DIR/cri-gen/$name"

    rm -rf "$dir"
    mkdir -p "$dir"
    cp "$GEN_WORKTREE/cri-multiplex/proto/$rel" "$dir/$(basename "$rel")"

    # admin.proto 依赖同目录的 orchestrator.proto
    if [ "$(basename "$rel")" = "admin.proto" ]; then
        cp "$GEN_WORKTREE/cri-multiplex/proto/orchestrator.proto" "$dir/"
    fi

    printf 'version: v1\n' > "$dir/buf.yaml"
    cat > "$dir/buf.gen.yaml" <<EOF
version: v1
plugins:
  - plugin: go
    out: $GEN_WORKTREE/$outdir
    opt: paths=source_relative
  - plugin: go-grpc
    out: $GEN_WORKTREE/$outdir
    opt: paths=source_relative
EOF

    gobin="$(install_proto_plugin protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "$gover")" \
        || die "安装检查工具失败: protoc-gen-go ${gover}，终止检查"
    grpcbin="$(install_proto_plugin protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc "$grpcver")" \
        || die "安装检查工具失败: protoc-gen-go-grpc ${grpcver}，终止检查"

    setup_proto_path "protoc-gen-go|$gobin" "protoc-gen-go-grpc|$grpcbin"

    if ! out="$(cd "$dir" && "$BUF_BIN" generate --path "$(basename "$rel")" 2>&1)"; then
        ERRORS+=("buf generate 失败: cri-multiplex/$rel")
        bad "✗ [generated/proto] buf generate 失败: cri-multiplex/$rel"
        printf '%s\n' "$out" | sed 's/^/    /'
        return 1
    fi
    return 0
}

report_proto_gen_diff() {
    local status st f diff line

    status="$(git -C "$GEN_WORKTREE" status --porcelain -- '*.go')"
    if [ -z "$status" ]; then
        ok "✓ [generated/proto] proto 生成代码与提交一致"
        return 0
    fi

    local failed=0
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        st="${line:0:2}"
        f="${line:3}"

        if [ "$st" = "??" ]; then
            FAILURES+=("proto 生成文件未提交: $f")
            bad "✗ [generated/proto] 新增了未提交的生成文件: $f"
            failed=1
            continue
        fi

        diff="$(proto_gen_diff -- "$f")"
        if [ -z "$diff" ]; then
            continue
        fi

        FAILURES+=("proto 生成代码未同步: $f")
        bad "✗ [generated/proto] $f 与 proto 不一致，请重新生成并提交"
        printf '%s\n' "$diff" | sed -n '1,20p' | sed 's/^/    /'
        failed=1
    done <<< "$status"

    if [ "$failed" -eq 0 ]; then
        info "[generated/proto] 仅注释/版本行差异，忽略"
    fi
    return 0
}

check_generated_proto() {
    local -a touched=()
    local f envd=0 legacy=0 orch=0 cri=0
    local gobin golegacy grpcbin connectbin job

    for f in "${CHANGED[@]}"; do
        case "$f" in
            *.proto) touched+=("$f") ;;
        esac
    done

    if [ "${#touched[@]}" -eq 0 ]; then
        info "[generated/proto] 未发现 .proto 变更"
        return 0
    fi

    for f in "${touched[@]}"; do
        case "$f" in
            packages/envd/spec/*) envd=1 ;;
            packages/envd/internal/services/legacy/*) legacy=1 ;;
            packages/orchestrator/*.proto) orch=1 ;;
            cri-multiplex/proto/*) cri=1 ;;
            *) info "[generated/proto] 未配置生成器，跳过: $f" ;;
        esac
    done

    if [ "$envd" -eq 0 ] && [ "$legacy" -eq 0 ] && [ "$orch" -eq 0 ] && [ "$cri" -eq 0 ]; then
        return 0
    fi

    prepare_generation_worktree || return 0

    if [ "$envd" -eq 1 ]; then
        info "[generated/proto] 重新生成 envd spec（go ${PROTO_GO_VERSION} + connect ${PROTO_CONNECT_VERSION}）"
        gobin="$(install_proto_plugin protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "$PROTO_GO_VERSION")" \
            || die "安装检查工具失败: protoc-gen-go ${PROTO_GO_VERSION}，终止检查"
        connectbin="$(install_proto_plugin protoc-gen-connect-go connectrpc.com/connect/cmd/protoc-gen-connect-go "$PROTO_CONNECT_VERSION")" \
            || die "安装检查工具失败: protoc-gen-connect-go ${PROTO_CONNECT_VERSION}，终止检查"
        setup_proto_path "protoc-gen-go|$gobin" "protoc-gen-connect-go|$connectbin"
        run_buf_generate "packages/envd/spec" --template buf.gen.yaml || true
        run_buf_generate "packages/envd/spec" --template buf.gen.shared.yaml || true
    fi

    if [ "$legacy" -eq 1 ]; then
        info "[generated/proto] 重新生成 envd legacy（go ${PROTO_GO_LEGACY_VERSION}）"
        golegacy="$(install_proto_plugin protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "$PROTO_GO_LEGACY_VERSION")" \
            || die "安装检查工具失败: protoc-gen-go ${PROTO_GO_LEGACY_VERSION}，终止检查"
        setup_proto_path "protoc-gen-go|$golegacy"
        run_buf_generate "packages/envd/internal/services/legacy" --template buf.gen.yaml || true
    fi

    if [ "$orch" -eq 1 ]; then
        info "[generated/proto] 重新生成 orchestrator（go ${PROTO_GO_VERSION} + grpc ${PROTO_GRPC_VERSION}）"
        gobin="$(install_proto_plugin protoc-gen-go google.golang.org/protobuf/cmd/protoc-gen-go "$PROTO_GO_VERSION")" \
            || die "安装检查工具失败: protoc-gen-go ${PROTO_GO_VERSION}，终止检查"
        grpcbin="$(install_proto_plugin protoc-gen-go-grpc google.golang.org/grpc/cmd/protoc-gen-go-grpc "$PROTO_GRPC_VERSION")" \
            || die "安装检查工具失败: protoc-gen-go-grpc ${PROTO_GRPC_VERSION}，终止检查"
        setup_proto_path "protoc-gen-go|$gobin" "protoc-gen-go-grpc|$grpcbin"

        local -a protos=("orchestrator.proto:orchestrator" "info.proto:orchestrator-info" "template-manager.proto:template-manager")
        local item name out cfg
        for item in "${protos[@]}"; do
            name="${item%%:*}"
            out="$GEN_WORKTREE/packages/shared/pkg/grpc/${item#*:}"
            cfg="$TMP_DIR/buf.gen.$name.yaml"
            write_orchestrator_buf_gen "$cfg" "$out"
            run_buf_generate "packages/orchestrator" --template "$cfg" --path "$name" || true
        done
    fi

    if [ "$cri" -eq 1 ]; then
        local cri_touched=0
        for f in "${touched[@]}"; do
            case "$f" in
                cri-multiplex/proto/admin.proto|cri-multiplex/proto/admin_v2.proto|cri-multiplex/proto/orchestrator.proto|cri-multiplex/proto/envd/process.proto)
                    cri_touched=1 ;;
                cri-multiplex/proto/process.proto)
                    info "[generated/proto] 历史遗留 proto，未纳入生成检查: $f" ;;
            esac
        done

        if [ "$cri_touched" -eq 1 ]; then
            info "[generated/proto] 重新生成 cri-multiplex（按文件锁版本）"
            for job in "${CRI_PROTO_JOBS[@]}"; do
                generate_cri_proto "$job" || true
            done
        fi
    fi

    report_proto_gen_diff
}

# ---------------------------------------------------------------------------
# 生成代码检查（方案 B：按生成器组重新生成 + diff）
#
# 不维护“spec -> 生成文件”的 manifest：按变更的 spec 选择生成器命令执行
# go generate，在临时 git worktree 中生成，避免污染当前工作区。
# proto 生成物暂不检查：仓库内 protoc-gen-go 版本不统一，重新生成会产生
# 大量与接口无关的 diff。
# ---------------------------------------------------------------------------
prepare_generation_worktree() {
    if [ -n "$GEN_WORKTREE" ] && [ -d "$GEN_WORKTREE" ]; then
        return 0
    fi

    GEN_WORKTREE="$TMP_DIR/gen-worktree"
    local err="$TMP_DIR/worktree.err"
    if ! git worktree add --detach "$GEN_WORKTREE" "$TARGET_SHA" >/dev/null 2>"$err"; then
        ERRORS+=("创建生成检查 worktree 失败: $TARGET_SHA")
        bad "✗ 创建生成检查 worktree 失败："
        sed 's/^/    /' "$err" >&2 || true
        GEN_WORKTREE=""
        return 1
    fi
    return 0
}

run_go_generate() {
    local module="$1"
    local pkg="$2"
    local out
    local gowork="$GEN_WORKTREE/go.work"

    [ -f "$gowork" ] || gowork="off"

    if ! out="$(cd "$GEN_WORKTREE/$module" && GOWORK="$gowork" go generate "$pkg" 2>&1)"; then
        ERRORS+=("go generate 失败: $module $pkg")
        bad "✗ [$module] go generate $pkg 失败："
        printf '%s\n' "$out" | sed 's/^/    /'
        return 1
    fi

    if [ -n "$out" ]; then
        printf '%s\n' "$out" | sed 's/^/    /'
    fi
    return 0
}

report_generated_diff() {
    local label="$1"
    local status

    status="$(git -C "$GEN_WORKTREE" status --porcelain)"
    if [ -z "$status" ]; then
        ok "✓ [$label] 生成代码与契约一致"
        return 0
    fi

    FAILURES+=("生成代码未同步: $label")
    bad "✗ [$label] 生成代码与契约不一致，请重新生成并提交"
    printf '%s\n' "$status" | sed 's/^/    /'
    git -C "$GEN_WORKTREE" diff --stat | sed 's/^/    /' || true
    git -C "$GEN_WORKTREE" diff --unified=0 | sed -n '1,120p' | sed 's/^/    /' || true

    if [ "$(git -C "$GEN_WORKTREE" status --porcelain | wc -l)" -gt 20 ]; then
        warn "    变更文件较多，以上仅展示前 120 行 diff"
    fi
    echo "    提示：不要手改生成文件；请执行对应 go generate 后提交生成结果。"
}

check_generated_openapi() {
    local -a jobs=()
    local f job
    local failed=0

    for f in "${CHANGED[@]}"; do
        case "$f" in
            spec/openapi.yml|spec/openapi.yaml)
                jobs+=("packages/api|./internal/api")
                jobs+=("tests/integration|./internal/api") ;;
            spec/openapi-dashboard.yml|spec/openapi-dashboard.yaml)
                jobs+=("packages/dashboard-api|./internal/api") ;;
            spec/openapi-edge.yml|spec/openapi-edge.yaml)
                jobs+=("packages/shared|./pkg/http/edge") ;;
            spec/openapi-hyperloop.yml|spec/openapi-hyperloop.yaml)
                jobs+=("packages/orchestrator|./internal/hyperloopserver/contracts") ;;
            packages/envd/spec/envd.yaml)
                jobs+=("packages/envd|./internal/api")
                jobs+=("packages/orchestrator|./internal/sandbox/envd")
                jobs+=("tests/integration|./internal/envd") ;;
        esac
    done

    if [ "${#jobs[@]}" -eq 0 ]; then
        info "[generated/openapi] 未发现 OpenAPI 契约变更，跳过"
        return 0
    fi

    info "[generated/openapi] 重新生成 OpenAPI 代码并检查 diff"
    timed "[generated] 创建 worktree" prepare_generation_worktree || return 0

    mapfile -t jobs < <(printf '%s\n' "${jobs[@]}" | sort -u)
    for job in "${jobs[@]}"; do
        timed "[generated] ${job%%|*} ${job#*|}" run_go_generate "${job%%|*}" "${job#*|}" || failed=1
    done

    [ "$failed" -eq 0 ] || return 0
    timed "[generated] 比对生成结果" report_generated_diff "generated/openapi"
}

check_generated() {
    mkdir -p "$TMP_DIR/tool-bin"
    ln -sf "$BUF_BIN" "$TMP_DIR/tool-bin/buf"
    ln -sf "$OASDIFF_BIN" "$TMP_DIR/tool-bin/oasdiff"
    PATH="$TMP_DIR/tool-bin:$PATH"

    local gopath
    if gopath="$(go env GOPATH 2>/dev/null)" && [ -n "$gopath" ] && [ -d "$gopath/bin" ]; then
        PATH="$gopath/bin:$PATH"
    fi
    PATH="$TOOL_DIR:$PATH"
    export PATH

    check_generated_openapi
}

# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------
info "============================== 接口门禁检查 =============================="

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    cat >&2 <<EOF
用法: BASE=<ref> TARGET=<ref> $0
      $0 <BASE> <TARGET>

  比较当前仓库中两个 git ref 之间的接口变更。
  优先取环境变量 BASE / TARGET，其次取两个位置参数，都没有时默认 HEAD^ / HEAD。

示例:
  BASE=origin/main TARGET=HEAD $0
  $0 HEAD~1 HEAD
EOF
    exit 0
fi

stage_begin "1/8 参数与 ref 解析"
BASE_REF="${BASE:-${1:-HEAD^}}"
TARGET_REF="${TARGET:-${2:-HEAD}}"

git rev-parse --git-dir >/dev/null 2>&1 || die "当前目录不是 git 仓库"

BASE_SHA="$(git rev-parse --verify "${BASE_REF}^{commit}" 2>/dev/null)" \
    || die "无效 BASE ref: $BASE_REF"
TARGET_SHA="$(git rev-parse --verify "${TARGET_REF}^{commit}" 2>/dev/null)" \
    || die "无效 TARGET ref: $TARGET_REF"

info "比较范围: ${BASE_REF} (${BASE_SHA:0:12}) -> ${TARGET_REF} (${TARGET_SHA:0:12})"
stage_end

stage_begin "2/8 工具准备"
if ! ensure_tools; then
    bad "结果: 异常（工具未就绪，$(fmt_duration "$(( $(now_us) - STAGE_T0 ))")）"
    print_total
    exit 2
fi
info "buf:     $("$BUF_BIN" --version 2>/dev/null | head -1)"
info "oasdiff: $("$OASDIFF_BIN" --version 2>/dev/null | head -1)"
stage_end

stage_begin "3/8 收集变更文件"
mapfile -t CHANGED < <(
    git diff --name-only "$BASE_SHA" "$TARGET_SHA" -- . ':(exclude)**/vendor/**'
)
if [ "${#CHANGED[@]}" -eq 0 ]; then
    ok "两个 ref 之间无任何文件变更"
    stage_end
    print_total
    exit 0
fi
info "共 ${#CHANGED[@]} 个文件变更"
stage_end

stage "4/8 proto 接口检查" check_proto
stage "5/8 OpenAPI 接口检查" check_openapi
stage "6/8 生成文件检查" check_generated_files
stage "7/8 生成 proto 检查" check_generated_proto
stage "8/8 生成代码检查" check_generated
  
# ---------------------------------------------------------------------------
# 结果汇总
# ---------------------------------------------------------------------------
echo
if [ "${#FAILURES[@]}" -gt 0 ]; then
    bad "================ 接口门禁检查未通过 ================"
    for f in "${FAILURES[@]}"; do
        bad "  ✗ $f"
    done
    if [ "${#ERRORS[@]}" -gt 0 ]; then
        warn "另外有 ${#ERRORS[@]} 个工具执行错误："
        for f in "${ERRORS[@]}"; do
            warn "  ! $f"
        done
    fi
    bad "接口新增 / 删除 / 修改均视为门禁失败，请人工确认后再合入。"
    print_total
    exit 1
fi

if [ "${#ERRORS[@]}" -gt 0 ]; then
    bad "================ 接口门禁检查异常 ================"
    for f in "${ERRORS[@]}"; do
        bad "  ! $f"
    done
    print_total
    exit 2
fi

ok "================ 接口门禁检查通过 ================"
ok "未检测到 proto / OpenAPI 接口的新增、删除或修改。"
print_total
exit 0

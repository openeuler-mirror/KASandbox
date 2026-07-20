#!/usr/bin/env bash
# 单元测试运行脚本
#   ./run-ut.sh                # 全部 UT（跳过需 sudo 的包）
#   ./run-ut.sh -p api         # 只跑指定包
#   ./run-ut.sh --no-race      # 关闭 race
#   ./run-ut.sh --sudo         # 包含 envd/orchestrator
#   ./run-ut.sh --skip-docker  # 门禁/无 Docker 环境：跳过依赖容器的用例

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

RACE_FLAG="-race"
ENABLE_SUDO=false
SKIP_DOCKER=false
TARGET_PKG=""
VERBOSE="-v"

# 始终跳过的用例（go test -skip 正则），不依赖 --skip-docker。
ALWAYS_SKIP_TESTS=(
  # 依赖 bindfs，openEuler 等环境无此包
  '^TestIsPathOnNetworkMount_FuseMount$'
  # 测试用 nr_inodes=2 的 tmpfs；本机内核上连第 1 个文件都建不了（需 ≥3 才符合预期），暂不改测试代码
  '^TestProcessFile/write_new_file_with_no_inodes_available$'
  # 需 mount tmpfs；门禁无特权容器（root 仍无 CAP_SYS_ADMIN）会 mount exit 32
  '^TestProcessFile/out_of_disk_space$'
  '^TestProcessFile/overwrite_file_on_full_disk$'
  '^TestProcessFile/write_new_file_on_full_disk$'
  # 写 /sys/fs/cgroup；门禁 cgroup 只读（root 不 Skip 仍失败）
  '^TestProcessFile/update_sysfs_or_other_virtual_fs$'
  # orchestrator: MAP_HUGETLB，门禁无可用 hugepages → mmap cannot allocate memory
  '^TestCopyFromProcess_HugepageToRegularPage$'
  '^TestCopyFromProcess_MAX_RW_COUNT_Misalignment_Hugepage$'
  # orchestrator: 需 nbd 内核模块；门禁无 modprobe / 无 /dev/nbd*
  '^TestPathDirect_'
  '^TestPathLargeRead$'
  # orchestrator: userfaultfd（UFFDIO_API）；门禁内核/无特权常 invalid argument
  '^TestMissing$'
  '^TestMissingWrite$'
  '^TestAsyncWriteProtection$'
  '^TestParallelMissing'
  '^TestSerialMissing'
  '^TestParallelMissingWrite'
  '^TestSerialMissingWrite'
  # orchestrator: 写 /sys/fs/cgroup；门禁 cgroup 只读（root 不 Skip 仍硬失败）
  '^TestManagerInitialize$'
  '^TestCgroupHandle'
  # envd: 同上，可写 cgroup
  '^TestCgroupRoundTrip$'
)

# 不走 SetupDatabase/SetupInstance、但仍依赖 Docker 的用例（go test -skip 正则）。
# 走上述 helper 的测试靠 -short + helper 内 testing.Short() 跳过，不必逐条列入。
# 仅在 --skip-docker 时生效。
DOCKER_SKIP_TESTS=(
  '^TestResilientListener_EMFILEWithContainer$'
  '^TestIntegrationTest$'
  '^TestSmokeAllFCVersions$'
  '^TestRun$' # packages/local-dev seed（唯一 TestRun）
)

# 格式: 包路径|测试路径|需要sudo|说明
PKG_LIST=(
  "packages/api|./...|false|API gateway"
  "packages/client-proxy|./...|false|Client proxy"
  "packages/db|./...|false|Database"
  "packages/docker-reverse-proxy|./...|false|Docker reverse proxy"
  "packages/shared|./pkg/...|false|Shared library"
  "packages/envd|./...|true|In-sandbox daemon"
  "packages/orchestrator|./...|true|Orchestrator"
  "packages/auth|./...|false|Authentication"
  "packages/clickhouse|./...|false|ClickHouse"
  "packages/local-dev|./...|false|Local development"
)

usage() {
  cat <<EOF
Usage: $0 [options]

Options:
  -p, --package PKG    Only run package (api/client-proxy/db/shared/envd/orchestrator/...)
  --no-race            Disable -race
  --sudo               Also run envd/orchestrator (needs root)
  --skip-docker        Skip Docker/testcontainers tests (for gate CI without Docker)
  -q, --quiet          Omit -v
  -h, --help           Show help

--skip-docker enables go test -short (SetupDatabase / SetupInstance skip) and
-skip with patterns from DOCKER_SKIP_TESTS in this script.
EOF
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -p|--package)
        [[ $# -lt 2 || "$2" == -* ]] && { echo -e "${RED}Error: -p requires a package name${NC}"; usage; exit 1; }
        TARGET_PKG="$2"
        shift 2
        ;;
      --no-race)       RACE_FLAG=""; shift ;;
      --sudo)          ENABLE_SUDO=true; shift ;;
      --skip-docker)   SKIP_DOCKER=true; shift ;;
      -q|--quiet)      VERBOSE=""; shift ;;
      -h|--help)       usage; exit 0 ;;
      *)               echo -e "${RED}Unknown argument: $1${NC}"; usage; exit 1 ;;
    esac
  done
}

# 组装传给 go test 的 -short / -skip（写入 GO_TEST_EXTRA 数组）
build_go_test_extra_flags() {
  GO_TEST_EXTRA=()
  local -a skip_patterns=()
  local name joined=""

  skip_patterns+=("${ALWAYS_SKIP_TESTS[@]}")
  if [[ "$SKIP_DOCKER" == true ]]; then
    GO_TEST_EXTRA+=(-short)
    skip_patterns+=("${DOCKER_SKIP_TESTS[@]}")
  fi

  for name in "${skip_patterns[@]}"; do
    [[ -z "$name" ]] && continue
    if [[ -z "$joined" ]]; then
      joined="$name"
    else
      joined="$joined|$name"
    fi
  done
  [[ -n "$joined" ]] && GO_TEST_EXTRA+=(-skip "$joined")
}

validate_target_pkg() {
  [[ -z "$TARGET_PKG" ]] && return 0
  local pkg pkg_path
  for pkg in "${PKG_LIST[@]}"; do
    IFS='|' read -r pkg_path _ _ _ <<< "$pkg"
    [[ "$(basename "$pkg_path")" == "$TARGET_PKG" ]] && return 0
  done
  echo -e "${RED}Error: unknown package '$TARGET_PKG'${NC}"
  usage
  exit 1
}

# 统计 *_test.go（排除 smoketest、benchmark）
count_test_files() {
  local pkg_path="$1" test_path="$2" base="$SCRIPT_DIR/$pkg_path" search_dir
  if [[ "$test_path" == "./pkg/"* ]]; then
    search_dir="$base/pkg"
  else
    search_dir="$base"
  fi
  find "$search_dir" -name "*_test.go" \
    -not -path "*/smoketest/*" \
    -not -name "*_benchmark_test.go" \
    2>/dev/null | wc -l
}

setup_orchestrator() {
  echo -e "${YELLOW}  Preparing orchestrator test environment...${NC}"
  [[ -w /proc/sys/vm/unprivileged_userfaultfd ]] && \
    echo 1 | sudo tee /proc/sys/vm/unprivileged_userfaultfd >/dev/null

  if ! mountpoint -q /mnt/hugepages 2>/dev/null; then
    sudo mkdir -p /mnt/hugepages
    sudo mount -t hugetlbfs none /mnt/hugepages 2>/dev/null || \
      echo -e "${YELLOW}  Warning: hugepages mount failed${NC}"
  fi
  [[ -w /proc/sys/vm/nr_hugepages ]] && \
    echo 2000 | sudo tee /proc/sys/vm/nr_hugepages >/dev/null 2>&1 || true

  if ! lsmod | grep -q '^nbd '; then
    sudo modprobe nbd nbds_max=256 2>/dev/null || \
      echo -e "${YELLOW}  Warning: nbd modprobe failed${NC}"
  fi

  if [[ ! -f /etc/udev/rules.d/97-nbd-device.rules ]]; then
    echo 'ACTION=="add|change", KERNEL=="nbd*", OPTIONS:="nowatch"' | \
      sudo tee /etc/udev/rules.d/97-nbd-device.rules >/dev/null 2>&1 || true
    sudo udevadm control --reload-rules 2>/dev/null || true
    sudo udevadm trigger 2>/dev/null || true
  fi
}

indent() { sed 's/^/  /'; }

# 执行 go test，stdout/stderr 写入 log_file，返回退出码
invoke_go_test() {
  local need_sudo="$1" go_bin="$2" log_file="$3" ec
  shift 3
  set +e
  if [[ "$need_sudo" == "true" ]]; then
    sudo -E "$go_bin" test "$@" >"$log_file" 2>&1
  else
    "$go_bin" test "$@" >"$log_file" 2>&1
  fi
  ec=$?
  set -e
  return "$ec"
}

# [build failed] 时对失败子包再跑一次，打出 undefined 等编译细节
print_build_failure_details() {
  local go_bin="$1" need_sudo="$2" log_file="$3" test_path="${4:-./...}"
  local mod_path failed_import rel detail_file
  local -a failed_imports=()

  # 工作区下 `go list -m` 可能输出多行，只取当前模块
  mod_path="$("$go_bin" list -m -f '{{.Path}}' 2>/dev/null | head -n1 || true)"
  if [[ -z "$mod_path" && -f go.mod ]]; then
    mod_path="$(awk '/^module[[:space:]]/{print $2; exit}' go.mod)"
  fi
  if [[ -z "$mod_path" ]]; then
    echo -e "${YELLOW}  Warning: cannot resolve module path (go list -m / go.mod empty)${NC}"
  fi

  # 去掉可能的 ANSI 颜色后再解析: FAIL <pkg> [build|setup failed]
  mapfile -t failed_imports < <(
    grep -E '\[(build|setup) failed\]' "$log_file" \
      | sed 's/\x1b\[[0-9;]*m//g' \
      | sed -E 's/^[[:space:]]*FAIL[[:space:]]+//; s/[[:space:]]+\[(build|setup) failed\].*$//' \
      | sed '/^$/d' \
      | sort -u
  )

  if [[ ${#failed_imports[@]} -eq 0 ]]; then
    echo -e "${YELLOW}  Warning: found [build|setup failed] but failed to parse package paths${NC}"
    echo -e "${YELLOW}  matching lines:${NC}"
    grep -E '\[(build|setup) failed\]' "$log_file" | indent || true
    echo -e "${CYAN}  $ go test -mod=readonly -count=1 -timeout 10m ${GO_TEST_EXTRA[*]+${GO_TEST_EXTRA[*]} }${test_path}${NC}"
    detail_file=$(mktemp)
    invoke_go_test "$need_sudo" "$go_bin" "$detail_file" \
      -mod=readonly -count=1 -timeout 10m ${GO_TEST_EXTRA[@]+"${GO_TEST_EXTRA[@]}"} "$test_path" || true
    indent <"$detail_file"
    rm -f "$detail_file"
    return 0
  fi

  echo -e "${YELLOW}  --- compiler details ---${NC}"
  for failed_import in "${failed_imports[@]}"; do
    [[ -z "$failed_import" ]] && continue
    if [[ -n "$mod_path" && "$failed_import" == "$mod_path" ]]; then
      rel="."
    elif [[ -n "$mod_path" && "$failed_import" == "$mod_path/"* ]]; then
      rel="./${failed_import#"$mod_path"/}"
    else
      # 模块路径未知时，尽量剥成相对路径
      rel="$failed_import"
      if [[ "$failed_import" == *"/internal/"* ]]; then
        rel="./internal/${failed_import#*/internal/}"
      elif [[ "$failed_import" == *"/pkg/"* ]]; then
        rel="./pkg/${failed_import#*/pkg/}"
      fi
    fi

    echo -e "${CYAN}  $ go test -mod=readonly -count=1 -timeout 10m ${GO_TEST_EXTRA[*]+${GO_TEST_EXTRA[*]} }${rel}${NC}"
    detail_file=$(mktemp)
    invoke_go_test "$need_sudo" "$go_bin" "$detail_file" \
      -mod=readonly -count=1 -timeout 10m ${GO_TEST_EXTRA[@]+"${GO_TEST_EXTRA[@]}"} "$rel" || true
    if [[ -s "$detail_file" ]]; then
      indent <"$detail_file"
    else
      echo -e "${RED}  (no output for ${rel})${NC}"
    fi
    rm -f "$detail_file"
  done
}

run_go_test() {
  local need_sudo="$1" test_path="$2" race_flag="$3"
  local go_bin test_exit log_file
  go_bin=$(command -v go)
  log_file=$(mktemp)

  set +e
  # shellcheck disable=SC2086
  invoke_go_test "$need_sudo" "$go_bin" "$log_file" \
    $VERBOSE -mod=readonly $race_flag -count=1 -timeout 10m \
    ${GO_TEST_EXTRA[@]+"${GO_TEST_EXTRA[@]}"} "$test_path"
  test_exit=$?
  set -e

  if [[ -s "$log_file" ]]; then
    indent <"$log_file"
  elif [[ "$test_exit" -ne 0 ]]; then
    echo -e "${RED}  (go test produced no output, exit=${test_exit})${NC}"
  fi

  if [[ "$test_exit" -ne 0 ]] && grep -qE '\[(build|setup) failed\]' "$log_file"; then
    print_build_failure_details "$go_bin" "$need_sudo" "$log_file" "$test_path"
  fi

  rm -f "$log_file"
  [[ "$test_exit" -ne 0 ]] && echo -e "${RED}  go test exit code: ${test_exit}${NC}"
  return "$test_exit"
}

TOTAL_PKGS=0
TOTAL_FILES=0
PASSED=0
FAILED=0
SKIPPED=0
FAILED_PKGS=()

run_pkg_ut() {
  local pkg_path="$1" test_path="$2" need_sudo="$3" desc="$4"
  local pkg_name file_count race_flag start_time end_time
  pkg_name=$(basename "$pkg_path")

  [[ -n "$TARGET_PKG" && "$pkg_name" != "$TARGET_PKG" ]] && return 0

  if [[ "$need_sudo" == "true" && "$ENABLE_SUDO" != true ]]; then
    file_count=$(count_test_files "$pkg_path" "$test_path")
    echo -e "${YELLOW}SKIP${NC}  $pkg_name ($desc) — requires --sudo"
    ((SKIPPED++)) || true
    ((TOTAL_PKGS++)) || true
    TOTAL_FILES=$((TOTAL_FILES + file_count))
    return 0
  fi

  if [[ "$pkg_name" == "orchestrator" && "$need_sudo" == "true" ]]; then
    setup_orchestrator
  fi

  race_flag="$RACE_FLAG"
  [[ "$need_sudo" == "true" ]] && race_flag=""

  echo -e "${CYAN}▶ RUN${NC}  $pkg_name ($desc)"
  start_time=$(date +%s)
  if run_go_test "$need_sudo" "$test_path" "$race_flag"; then
    end_time=$(date +%s)
    echo -e "${GREEN}✓ PASS${NC}  $pkg_name ($((end_time - start_time))s)"
    ((PASSED++)) || true
  else
    end_time=$(date +%s)
    echo -e "${RED}✗ FAIL${NC}  $pkg_name ($((end_time - start_time))s)"
    ((FAILED++)) || true
    FAILED_PKGS+=("$pkg_name")
  fi

  ((TOTAL_PKGS++)) || true
  TOTAL_FILES=$((TOTAL_FILES + $(count_test_files "$pkg_path" "$test_path")))
}

# ---- main ----
parse_args "$@"
validate_target_pkg
build_go_test_extra_flags

echo "========================================="
echo " Unit Tests"
echo "========================================="
echo ""

command -v go &>/dev/null || { echo -e "${RED}Error: go not found${NC}"; exit 1; }
echo "Go version: $(go version)"
echo "Race: $([ -n "$RACE_FLAG" ] && echo "enabled (except sudo packages)" || echo "disabled")"
echo "Sudo packages: $([ "$ENABLE_SUDO" == true ] && echo "enabled" || echo "skipped")"
echo "Skip Docker: $([ "$SKIP_DOCKER" == true ] && echo "enabled (-short + DOCKER_SKIP_TESTS)" || echo "disabled")"
echo "Always skip: ${ALWAYS_SKIP_TESTS[*]}"
echo ""

for pkg in "${PKG_LIST[@]}"; do
  IFS='|' read -r pkg_path test_path need_sudo desc <<< "$pkg"
  if [[ ! -d "$SCRIPT_DIR/$pkg_path" ]]; then
    echo -e "${YELLOW}SKIP${NC}  $pkg_path — directory not found"
    continue
  fi
  pushd "$SCRIPT_DIR/$pkg_path" > /dev/null
  run_pkg_ut "$pkg_path" "$test_path" "$need_sudo" "$desc"
  popd > /dev/null
done

if [[ -n "$TARGET_PKG" && "$TOTAL_PKGS" -eq 0 ]]; then
  echo -e "${RED}Error: package '$TARGET_PKG' not found${NC}"
  exit 1
fi

echo ""
echo "========================================="
echo " Test Summary"
echo "========================================="
echo "Packages:    $TOTAL_PKGS"
echo "Test files:  $TOTAL_FILES"
echo -e "${GREEN}Passed:      $PASSED${NC}"
echo -e "${RED}Failed:      $FAILED${NC}"
echo -e "${YELLOW}Skipped:     $SKIPPED${NC}"

if [[ ${#FAILED_PKGS[@]} -gt 0 ]]; then
  echo ""
  echo -e "${RED}Failed packages:${NC}"
  for p in "${FAILED_PKGS[@]}"; do
    echo "  - $p"
  done
fi
echo ""

[[ "$FAILED" -gt 0 ]] && exit 1
exit 0

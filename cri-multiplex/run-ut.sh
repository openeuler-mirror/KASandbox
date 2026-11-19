#!/bin/bash
###############################################################################
# run-ut.sh — Go UT runner
#
# Usage:
#   ./run-ut.sh                         # run all UTs and print per-test result
#   ./run-ut.sh all                     # run all UTs and print per-test result
#   ./run-ut.sh pkg/engine              # run one package/module
#   ./run-ut.sh engine server           # run multiple short module names
#   ./run-ut.sh ./pkg/engine -run TestX # pass go test flags
#   ./run-ut.sh --list                  # list UT test cases without running
###############################################################################
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export GOCACHE="${GOCACHE:-/tmp/cri-multiplex-gocache}"
export GOPATH="${GOPATH:-/tmp/cri-multiplex-gopath}"
export GOWORK=off

usage() {
    sed -n '3,14p' "$0" | sed 's/^# \{0,1\}//'
}

normalize_package() {
    local item="$1"

    case "${item}" in
        all|./...)
            echo "./..."
            ;;
        ./*)
            echo "${item}"
            ;;
        github.com/cri-multiplex/*)
            echo "${item}"
            ;;
        cmd|pkg|proto)
            echo "./${item}/..."
            ;;
        engine|server|orchestrator|envd)
            echo "./pkg/${item}"
            ;;
        cmd/*|pkg/*|proto/*)
            echo "./${item}"
            ;;
        *)
            if [ -d "${ROOT_DIR}/${item}" ]; then
                echo "./${item}"
            else
                echo "${item}"
            fi
            ;;
    esac
}

packages_with_tests() {
    find "${ROOT_DIR}" -name '*_test.go' -print \
        | sed "s#^${ROOT_DIR}/##" \
        | xargs -r -n1 dirname \
        | sort -u \
        | sed 's#^#./#'
}

resolve_packages() {
    local packages=("$@")

    if [ "${#packages[@]}" -eq 0 ]; then
        packages=("./...")
    fi

    if [ "${#packages[@]}" -eq 1 ] && [ "${packages[0]}" = "./..." ]; then
        packages_with_tests
    else
        printf '%s\n' "${packages[@]}"
    fi
}

list_tests() {
    local packages=("$@")

    local pkg
    for pkg in "${packages[@]}"; do
        while IFS= read -r test_name; do
            [ -n "${test_name}" ] || continue
            printf '%s %s\n' "${pkg}" "${test_name}"
        done < <(go test -list '^Test' "${pkg}" 2>/dev/null | awk '/^Test/ { print $0 }')
    done
}

print_test_list() {
    local packages=("$@")

    echo "==> UT list"
    local test_list
    test_list="$(list_tests "${packages[@]}")"
    if [ -z "${test_list}" ]; then
        echo "No UT test cases found."
        return
    fi
    echo "${test_list}" | sed 's/^/  - /'
}

run_tests_with_summary() {
    local output_file="$1"
    shift
    local packages=("$@")

    set +e
    go test -json "${packages[@]}" "${go_args[@]}" >"${output_file}" 2>&1
    local exit_code=$?
    set -e

    python3 - "${output_file}" <<'PY'
import json
import sys

path = sys.argv[1]
results = {}
package_status = {}

with open(path, "r", encoding="utf-8", errors="replace") as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue

        pkg = event.get("Package", "")
        test = event.get("Test", "")
        action = event.get("Action", "")

        if test and action in {"pass", "fail", "skip"}:
            results[(pkg, test)] = action.upper()
        elif not test and action in {"pass", "fail"}:
            package_status[pkg] = action.upper()

print("==> UT result")

if results:
    for (pkg, test), status in sorted(results.items()):
        print(f"  [{status}] {pkg} {test}")
else:
    print("  No individual test result emitted.")

tested_packages = {pkg for pkg, _ in results}
for pkg, status in sorted(package_status.items()):
    if status == "FAIL" and pkg not in tested_packages:
        print(f"  [FAIL] {pkg} package setup")

pass_count = sum(1 for status in results.values() if status == "PASS")
fail_count = sum(1 for status in results.values() if status == "FAIL")
skip_count = sum(1 for status in results.values() if status == "SKIP")
package_fail_count = sum(1 for pkg, status in package_status.items() if status == "FAIL" and pkg not in tested_packages)

print("")
print("==> Summary")
print(f"  PASS: {pass_count}")
print(f"  FAIL: {fail_count + package_fail_count}")
print(f"  SKIP: {skip_count}")
print(f"  TOTAL: {pass_count + fail_count + skip_count + package_fail_count}")
PY

    return "${exit_code}"
}

if [ "${1:-}" = "--help" ] || [ "${1:-}" = "-h" ]; then
    usage
    exit 0
fi

original_args=("$@")
packages=()
go_args=()
list_only=0

if [ "${1:-}" = "--list" ]; then
    list_only=1
    shift
fi

while [ "$#" -gt 0 ]; do
    case "$1" in
        -*)
            go_args=("$@")
            break
            ;;
        *)
            packages+=("$(normalize_package "$1")")
            shift
            ;;
    esac
done

# 集中保存的同包测试必须与生产源码装配后执行，不能作为独立 Go 包扫描。
# 装配结果不包含 agentenv-verify，因此再次调用本脚本不会递归装配。
if [ -f "${ROOT_DIR}/agentenv-verify/prepare-tests.py" ]; then
    test_root="$(mktemp -d "${TMPDIR:-/tmp}/agentenv-ut.XXXXXX")"
    trap 'rm -rf -- "${test_root}"' EXIT
    python3 "${ROOT_DIR}/agentenv-verify/prepare-tests.py" --kind go \
        --source "${ROOT_DIR}/.." --output "${test_root}/source"
    result=0
    bash "${test_root}/source/cri-multiplex/run-ut.sh" "${original_args[@]}" || result=$?
    # 默认/all 同时覆盖迁移来的 Webhook 测试；指定包时仍只执行所选包。
    if [ "${#packages[@]}" -eq 0 ] || { [ "${#packages[@]}" -eq 1 ] && [ "${packages[0]}" = "./..." ]; }; then
        if [ "${list_only}" = "1" ]; then
            (cd "${test_root}/source/e2b-webhook" && go test -list '^Test' ./...) || result=$?
        else
            (cd "${test_root}/source/e2b-webhook" && go test ./... "${go_args[@]}") || result=$?
        fi
    fi
    exit "${result}"
fi

mapfile -t resolved_packages < <(resolve_packages "${packages[@]}")

if [ "${#resolved_packages[@]}" -eq 0 ]; then
    echo "No packages found." >&2
    exit 1
fi

cd "${ROOT_DIR}"

echo "==> GOCACHE=${GOCACHE}"
echo "==> GOPATH=${GOPATH}"
echo "==> packages: ${resolved_packages[*]}"
if [ "${#go_args[@]}" -gt 0 ]; then
    echo "==> go test args: ${go_args[*]}"
fi

print_test_list "${resolved_packages[@]}"

if [ "${list_only}" = "1" ]; then
    exit 0
fi

output_file="$(mktemp /tmp/cri-multiplex-ut.XXXXXX.json)"
trap 'rm -f "${output_file}"' EXIT

echo ""
echo "==> Running go test"
if run_tests_with_summary "${output_file}" "${resolved_packages[@]}"; then
    exit 0
fi

echo ""
echo "==> Raw go test failure output"
python3 - "${output_file}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8", errors="replace") as f:
    for line in f:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            print(line.rstrip())
            continue
        if event.get("Action") in {"fail", "output"}:
            output = event.get("Output")
            if output:
                print(output.rstrip())
PY
exit 1

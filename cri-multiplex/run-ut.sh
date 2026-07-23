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

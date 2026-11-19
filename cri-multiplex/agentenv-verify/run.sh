#!/usr/bin/env bash
set -euo pipefail

verify_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
case "${1:-}" in
    cri) script=cri/42_agentenv_native_installed_e2e.py ;;
    webhook) script=webhook/verify-admission.py ;;
    runtime) script=runtime/run-release-regression.py ;;
    contract) script=packaging/check_agentenv_contract.py ;;
    install-mock) script=packaging/test_install.py ;;
    rpm-inputs) script=packaging/verify-submission.py ;;
    licenses) script=packaging/verify-license-bundle.py ;;
    -h|--help|"")
        echo "用法：bash agentenv-verify/run.sh {cri|webhook|runtime|contract|install-mock|rpm-inputs|licenses} [参数]"
        echo "对指定套件使用 --help 查看所需参数。"
        exit 0
        ;;
    *)
        echo "未知套件：$1" >&2
        exit 2
        ;;
esac
shift
exec python3 "$verify_dir/$script" "$@"

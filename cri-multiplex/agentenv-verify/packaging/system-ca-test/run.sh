#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
: "${CARGO_BUILD_JOBS:=2}"
export CARGO_BUILD_JOBS
cargo build --offline
cargo clippy --offline -- -D warnings
export SSL_CERT_DIR="$PWD/fixtures/empty-dir"
for case in valid wrong-host untrusted expired empty; do
    if [ "$case" = empty ]; then export SSL_CERT_FILE="$PWD/fixtures/empty.pem";
    else export SSL_CERT_FILE="$PWD/fixtures/ca.pem"; fi
    timeout 30 "${CARGO_TARGET_DIR:-target}/debug/system-ca-probe" "$case"
done

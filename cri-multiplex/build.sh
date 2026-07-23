#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export GOCACHE="${GOCACHE:-/tmp/cri-multiplex-gocache}"
export GOPATH="${GOPATH:-/tmp/cri-multiplex-gopath}"
export GOWORK=off

OUTPUT="${ROOT_DIR}/cri-multiplex"

cd "${ROOT_DIR}"

echo "==> GOCACHE=${GOCACHE}"
echo "==> GOPATH=${GOPATH}"
echo "==> GOWORK=${GOWORK}"
echo "==> output: ${OUTPUT}"

go build -o "${OUTPUT}" ./cmd/cri-multiplex

echo "==> built: ${OUTPUT}"

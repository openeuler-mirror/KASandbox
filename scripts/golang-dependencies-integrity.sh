#!/bin/bash
set -euo pipefail

# go mod tidy for each standalone module (go.work was removed together
# with the e2b orchestration stack)
for dir in cri-multiplex e2b-webhook migrate-tool; do
  echo "$dir"
  pushd "$dir" > /dev/null
  go mod tidy
  go run github.com/abhijit-hota/modfmt@afa6506f86937f6c3b76e4911d57b769fa4659a0 --in-place
  popd > /dev/null
done

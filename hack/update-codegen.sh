#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

GO_CMD=${1:-go}
MODULE_ROOT="github.com/joeyloman/kubevirt-ip-helper"
SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
CODEGEN_PKG=${CODEGEN_PKG:-$(cd "${SCRIPT_ROOT}"; ls -d -1 ./vendor/k8s.io/code-generator 2>/dev/null || echo "$(go env GOMODCACHE)/k8s.io/code-generator@v0.34.1")}

cd "${SCRIPT_ROOT}"

# Source the kube_codegen.sh script
source "${CODEGEN_PKG}/kube_codegen.sh"

# Generate helper functions (deep copy, etc.)
echo "Generating helper functions..."
kube::codegen::gen_helpers \
  "${SCRIPT_ROOT}/pkg/apis" \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt"

# Generate client code (clientsets, informers, listers)
echo "Generating client code..."
kube::codegen::gen_client \
  "${SCRIPT_ROOT}/pkg/apis" \
  --output-dir "${SCRIPT_ROOT}/pkg/generated" \
  --output-pkg "${MODULE_ROOT}/pkg/generated" \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  --with-watch \
  --with-applyconfig

# Clean up temporary libraries added in go.mod by code-generator
echo "Cleaning up dependencies..."
"${GO_CMD}" mod tidy

echo "Code generation complete."

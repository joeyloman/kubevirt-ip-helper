#!/usr/bin/env bash
# Bootstrap the disposable kind cluster for the kubevirt-ip-helper E2E suite.
#
# Order matters and each step is an assertion:
#   1. kind CLI plus one control-plane and one worker on the pinned node image
#   2. inspect the kindnet delegate CNI that Multus has to chain to
#   3. bridge CNI plugin into every node's /opt/cni/bin
#   4. Multus thick as the master CNI
#   5. CNI chain preflight: contracted NAD plus plain and NAD-attached probes
#   6. KubeVirt with software emulation
# KubeVirt is only installed once step 5 passes: with a broken CNI chain every
# later DHCP assertion is meaningless.
#
# Re-running is safe. An existing ${E2E_CLUSTER_NAME} cluster is reused and each
# layer is applied again. The cluster is never deleted here; a failing run
# appends diagnostics to ${E2E_ARTIFACTS_DIR} for collect.sh and for the logs.
set -Eeuo pipefail

E2E_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${E2E_DIR}/../.." && pwd)"
# shellcheck source=test/e2e/versions.env
. "${E2E_DIR}/versions.env"

# Relative defaults are resolved against the repository root, so the script also
# works when invoked from another directory.
case "${E2E_ARTIFACTS_DIR}" in
  /*) ;;
  *) E2E_ARTIFACTS_DIR="${ROOT_DIR}/${E2E_ARTIFACTS_DIR}" ;;
esac
mkdir -p "${E2E_ARTIFACTS_DIR}" "${E2E_BIN_DIR}"
export KUBECONFIG="${KUBECONFIG:-${E2E_ARTIFACTS_DIR}/kubeconfig}"
E2E_CLUSTER_STATE_FILE="${E2E_CLUSTER_STATE_FILE:-${E2E_ARTIFACTS_DIR}/cluster-state}"
export E2E_CLUSTER_NAME E2E_IMAGE E2E_KEEP_CLUSTER E2E_CACHE_DIR E2E_BIN_DIR
export E2E_ARTIFACTS_DIR E2E_CLUSTER_STATE_FILE VIRTCTL

EVIDENCE_FILE="${E2E_ARTIFACTS_DIR}/bootstrap-evidence.txt"
BOOTSTRAP_CASES_FILE="${E2E_ARTIFACTS_DIR}/bootstrap-cases.jsonl"
BOOTSTRAP_JOURNAL_ERRORS_FILE="${E2E_ARTIFACTS_DIR}/bootstrap-journal-errors.txt"
BOOTSTRAP_CASE_ID=""
BOOTSTRAP_CASE_NAME=""
BOOTSTRAP_CASE_T0=""
: > "${BOOTSTRAP_CASES_FILE}"
: > "${BOOTSTRAP_JOURNAL_ERRORS_FILE}"

bootstrap_now_ms() {
  local stamp
  stamp="$(date +%s%3N 2> /dev/null)" || stamp=""
  case "${stamp}" in
    '' | *[!0-9]*) printf '%s' "$((SECONDS * 1000))" ;;
    *) printf '%s' "${stamp}" ;;
  esac
}

bootstrap_escape() {
  local text="$1"
  text="${text//\\/\\\\}"
  text="${text//\"/\\\"}"
  text="${text//$'\t'/ }"
  text="${text//$'\r'/}"
  text="${text//$'\n'/ }"
  printf '%s' "${text}"
}

bootstrap_case_start() {
  BOOTSTRAP_CASE_ID="$1"
  BOOTSTRAP_CASE_NAME="$2"
  BOOTSTRAP_CASE_T0="$(bootstrap_now_ms)"
}

bootstrap_case_close() {
  local status="$1" detail="$2" now elapsed rc=0
  [ -n "${BOOTSTRAP_CASE_ID}" ] || return 0
  now="$(bootstrap_now_ms)"
  elapsed=$((now - BOOTSTRAP_CASE_T0))
  [ "${elapsed}" -ge 0 ] || elapsed=0
  printf '{"kind":"bootstrap-case","id":"%s","name":"%s","group":"bootstrap","status":"%s","durationMs":%s,"detail":"%s"}\n' \
    "$(bootstrap_escape "${BOOTSTRAP_CASE_ID}")" \
    "$(bootstrap_escape "${BOOTSTRAP_CASE_NAME}")" \
    "${status}" "${elapsed}" "$(bootstrap_escape "${detail}")" \
    >> "${BOOTSTRAP_CASES_FILE}" || rc=1
  if [ "${rc}" -ne 0 ]; then
    printf '%s\t%s\t%s\t%s\n' \
      "$(bootstrap_now_ms)" "${BOOTSTRAP_CASE_ID}" "${status}" "${detail}" \
      >> "${BOOTSTRAP_JOURNAL_ERRORS_FILE}" || true
  fi
  BOOTSTRAP_CASE_ID=""
  BOOTSTRAP_CASE_NAME=""
  BOOTSTRAP_CASE_T0=""
  return "${rc}"
}

bootstrap_gate() { # <case-id> <description> <command...>
  local id="$1" description="$2"
  shift 2
  bootstrap_case_start "${id}" "${description}"
  "$@"
  bootstrap_case_close passed "${description} completed"
}

log() { printf '[bootstrap] %s\n' "$*"; }

# capture_evidence records whatever the cluster still shows; it runs on failures
# only and never changes the exit status.
capture_evidence() {
  {
    printf '# bootstrap evidence %s\n' "$(date -Is 2>/dev/null || date)"
    printf '\n## nodes\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl get nodes -o wide 2>&1
    printf '\n## pods\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl get pods -A -o wide 2>&1
    printf '\n## network-attachment-definitions\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
      get network-attachment-definitions -A 2>&1
    printf '\n## multus\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl -n "${MULTUS_NAMESPACE}" \
      get "ds/${MULTUS_DAEMONSET}" 2>&1
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl -n "${MULTUS_NAMESPACE}" \
      logs "ds/${MULTUS_DAEMONSET}" --tail=200 2>&1
    printf '\n## kubevirt\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl -n "${KUBEVIRT_NAMESPACE}" \
      get kubevirt,deploy,pods 2>&1
  } >> "${EVIDENCE_FILE}" 2>&1 || true
}
trap capture_evidence ERR
bootstrap_exit() {
  local rc=$?
  if [ -n "${BOOTSTRAP_CASE_ID}" ]; then
    bootstrap_case_close failed "bootstrap exited with status ${rc}" || true
  fi
  exit "${rc}"
}
trap bootstrap_exit EXIT

die() {
  printf '[bootstrap] ERROR: %s\n' "$*" >&2
  bootstrap_case_close failed "$*" || true
  capture_evidence
  exit 1
}

sha256_of() { # <file>
  if command -v sha256sum > /dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum > /dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  else
    die "need sha256sum or shasum to verify pinned downloads"
  fi
}

checksum_matches() { # <file> <expected>
  [ -n "$2" ] && [ "$(sha256_of "$1")" = "$2" ]
}

require_checksum() { # <file> <expected>
  checksum_matches "$1" "$2" ||
    die "checksum mismatch for $1: got $(sha256_of "$1"), expected $2"
  log "checksum ok: $(basename "$1")"
}

fetch() { # <url> <dest>
  local url="$1" dest="$2" tmp="${2}.part"
  mkdir -p "$(dirname "${dest}")"
  if command -v curl > /dev/null 2>&1; then
    curl -fsSL --retry 3 -m 300 -o "${tmp}" "${url}" || die "download failed: ${url}"
  elif command -v wget > /dev/null 2>&1; then
    wget -q -T 60 -t 3 -O "${tmp}" "${url}" || die "download failed: ${url}"
  else
    die "curl or wget is required to fetch ${url}"
  fi
  mv -f "${tmp}" "${dest}"
  log "downloaded ${url}"
}

arch_suffix() {
  case "$(uname -m)" in
    x86_64 | amd64) printf 'linux-amd64' ;;
    *) die "unsupported machine architecture '$(uname -m)': this suite pins linux-amd64 assets" ;;
  esac
}

# upper_arch <arch-suffix> -> variable suffix for the matching checksum.
upper_arch() { printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_'; }

resolve_runtime() {
  if command -v docker > /dev/null 2>&1 && docker info > /dev/null 2>&1; then
    RUNTIME="docker"
  elif command -v podman > /dev/null 2>&1 && podman info > /dev/null 2>&1; then
    RUNTIME="podman"
    export KIND_EXPERIMENTAL_PROVIDER="podman"
  else
    die "no usable Docker or Podman runtime is available"
  fi
  log "container runtime: ${RUNTIME}"
}

# ensure_kind leaves a ${KIND_VERSION} kind binary in ${KIND}. A cached or PATH
# binary is reused only when both its checksum and its version are right.
ensure_kind() {
  local sha_var expected candidate have
  sha_var="KIND_SHA256_$(upper_arch "${KIND_ARCH}")"
  expected="${!sha_var}"
  KIND=""
  for candidate in "${E2E_BIN_DIR}/kind" "$(command -v kind || true)"; do
    [ -n "${candidate}" ] && [ -x "${candidate}" ] || continue
    if ! checksum_matches "${candidate}" "${expected}"; then
      log "kind at ${candidate} does not match the pinned checksum"
      continue
    fi
    have="$("${candidate}" version 2> /dev/null | head -n 1 || true)"
    case "${have}" in
      *"${KIND_VERSION}"*)
        if [ "${candidate}" != "${E2E_BIN_DIR}/kind" ]; then
          cp "${candidate}" "${E2E_BIN_DIR}/kind"
          chmod 755 "${E2E_BIN_DIR}/kind"
        fi
        KIND="${E2E_BIN_DIR}/kind"
        log "using ${KIND} (${have})"
        return
        ;;
    esac
    log "kind at ${candidate} is not ${KIND_VERSION} (${have:-unresponsive})"
  done
  fetch "${KIND_BASE_URL}/kind-${KIND_ARCH}" "${E2E_BIN_DIR}/kind"
  chmod 755 "${E2E_BIN_DIR}/kind"
  require_checksum "${E2E_BIN_DIR}/kind" "${expected}"
  KIND="${E2E_BIN_DIR}/kind"
  log "installed ${KIND} ($("${KIND}" version | head -n 1))"
}

verify_cluster_pin() {
  local node image kubelet_version control_planes nodes_output
  local -a nodes=()
  nodes_output="$("${KIND}" get nodes --name "${E2E_CLUSTER_NAME}")" ||
    die "cannot enumerate kind nodes for ${E2E_CLUSTER_NAME}"
  while IFS= read -r node; do
    [ -n "${node}" ] && nodes+=("${node}")
  done <<< "${nodes_output}"
  # Topology contract: exactly one control-plane and one worker. Every
  # per-node layer iterates over both nodes via cluster_nodes.
  control_planes="$(kubectl get nodes \
    -l node-role.kubernetes.io/control-plane= --no-headers 2> /dev/null | wc -l)"
  [ "${control_planes}" -eq 1 ] ||
    die "cluster ${E2E_CLUSTER_NAME} has ${control_planes} control-plane nodes; expected exactly one"
  [ "${#nodes[@]}" -eq 2 ] ||
    die "cluster ${E2E_CLUSTER_NAME} has ${#nodes[@]} nodes; expected exactly one control-plane and one worker"
  for node in "${nodes[@]}"; do
    image="$("${RUNTIME}" inspect "${node}" --format '{{.Config.Image}}')"
    [ "${image}" = "${KINDEST_NODE_IMAGE}" ] ||
      die "cluster ${E2E_CLUSTER_NAME} uses node ${node} image ${image}, expected ${KINDEST_NODE_IMAGE}"
    kubelet_version="$(kubectl get node "${node}" -o jsonpath='{.status.nodeInfo.kubeletVersion}')"
    [ "${kubelet_version}" = "${KUBERNETES_VERSION}" ] ||
      die "cluster ${E2E_CLUSTER_NAME} runs ${kubelet_version} on ${node}, expected ${KUBERNETES_VERSION}"
  done
}


# ensure_cluster creates the named cluster once and reuses it afterwards.
ensure_cluster() {
  if "${KIND}" get clusters 2> /dev/null | grep -qx "${E2E_CLUSTER_NAME}"; then
    printf 'reused\n' > "${E2E_CLUSTER_STATE_FILE}"
    log "reusing existing cluster '${E2E_CLUSTER_NAME}' (E2E_KEEP_CLUSTER='${E2E_KEEP_CLUSTER}')"
  else
    printf 'owned\n' > "${E2E_CLUSTER_STATE_FILE}"
    log "creating cluster '${E2E_CLUSTER_NAME}' with ${KINDEST_NODE_IMAGE}"
    "${KIND}" create cluster \
      --name "${E2E_CLUSTER_NAME}" \
      --image "${KINDEST_NODE_IMAGE}" \
      --config "${E2E_DIR}/kind.yaml" \
      --wait "${E2E_WAIT_TIMEOUT}s"
  fi
  "${KIND}" export kubeconfig --name "${E2E_CLUSTER_NAME}"
  verify_cluster_pin
  kubectl wait --for=condition=Ready node --all --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl wait --for=condition=Ready pod -n kube-system \
    -l k8s-app=kube-dns --timeout="${E2E_WAIT_TIMEOUT}s"
  log "cluster '${E2E_CLUSTER_NAME}' ready on $(kubectl get nodes --no-headers | wc -l) node(s)"
  # Topology-aware evidence for the logs: the control-plane role and every
  # node that the per-node CNI layers and topology assertions cover.
  log "nodes: $(kubectl get nodes --no-headers | awk '{printf "%s(%s) ", $1, $3}')"
}

# ensure_helper_image loads a locally built helper image when one exists, so a
# rebuilt image is always the one the suite runs against.
ensure_helper_image() {
  if "${RUNTIME}" image inspect "${E2E_IMAGE}" > /dev/null 2>&1; then
    log "loading ${E2E_IMAGE} into '${E2E_CLUSTER_NAME}'"
    "${KIND}" load docker-image "${E2E_IMAGE}" --name "${E2E_CLUSTER_NAME}"
  else
    log "image '${E2E_IMAGE}' not present in ${RUNTIME}; run.sh loads it after building"
  fi
}

cluster_nodes() {
  local nodes count
  nodes="$("${KIND}" get nodes --name "${E2E_CLUSTER_NAME}")" || return 1
  count="$(printf '%s\n' "${nodes}" | sed '/^$/d' | wc -l)"
  [ "${count}" -eq 2 ] || return 1
  printf '%s\n' "${nodes}"
}

# inspect_kindnet checks the delegate CNI each node starts with, before Multus
# takes over as master plugin. Without it Multus has nothing to chain to.
inspect_kindnet() {
  local node listing delegate content nodes
  nodes="$(cluster_nodes)" ||
    die "kind cannot enumerate exactly two nodes for ${E2E_CLUSTER_NAME}"
  while IFS= read -r node; do
    listing="$("${RUNTIME}" exec "${node}" ls -1 /etc/cni/net.d)"
    delegate="$(printf '%s\n' "${listing}" | awk '!/multus/ && !seen++')"
    [ -n "${delegate}" ] || die "node ${node}: no CNI config in /etc/cni/net.d"
    content="$("${RUNTIME}" exec "${node}" cat "/etc/cni/net.d/${delegate}")"
    case "${content}" in
      *kindnet*) log "node ${node}: delegate ${delegate} uses kindnet" ;;
      *) die "node ${node}: ${delegate} does not configure the kindnet CNI" ;;
    esac
  done <<< "${nodes}"
}

# ensure_bridge_plugin installs the pinned CNI plugin bundle into every node and
# checks the bridge binary actually reports the pinned version.
ensure_bridge_plugin() {
  local sha_var tgz stage node out nodes
  sha_var="CNI_PLUGINS_SHA256_$(upper_arch "${KIND_ARCH}")"
  tgz="${E2E_CACHE_DIR}/cni-plugins-${KIND_ARCH}-${CNI_PLUGINS_VERSION}.tgz"
  stage="${E2E_CACHE_DIR}/cni-plugins-${KIND_ARCH}-${CNI_PLUGINS_VERSION}"
  [ -s "${tgz}" ] ||
    fetch "${CNI_PLUGINS_BASE_URL}/cni-plugins-${KIND_ARCH}-${CNI_PLUGINS_VERSION}.tgz" "${tgz}"
  require_checksum "${tgz}" "${!sha_var}"
  if [ ! -x "${stage}/bridge" ]; then
    rm -rf "${stage}"
    mkdir -p "${stage}"
    tar -xzf "${tgz}" -C "${stage}"
  fi
  nodes="$(cluster_nodes)" ||
    die "kind cannot enumerate exactly two nodes for ${E2E_CLUSTER_NAME}"
  while IFS= read -r node; do
    "${RUNTIME}" exec "${node}" mkdir -p /opt/cni/bin
    tar -C "${stage}" -cf - . |
      "${RUNTIME}" exec -i "${node}" tar -xf - -C /opt/cni/bin
    out="$("${RUNTIME}" exec "${node}" /opt/cni/bin/bridge --version 2>&1 | head -n 1)"
    case "${out}" in
      *"${CNI_PLUGINS_VERSION}"*)
        log "node ${node}: ${out}"
        ;;
      *) die "node ${node}: /opt/cni/bin/bridge reports '${out}', expected '${CNI_PLUGINS_VERSION}'" ;;
    esac
  done <<< "${nodes}"
}

# ensure_multus applies the upstream thick manifest with the moving snapshot tag
# replaced by the digest-pinned image, then waits for the DaemonSet.
ensure_multus() {
  local raw="${E2E_CACHE_DIR}/multus-daemonset-${MULTUS_VERSION}.yml"
  local rendered="${E2E_CACHE_DIR}/multus-daemonset-${MULTUS_VERSION}-pinned.yml"
  fetch "${MULTUS_MANIFEST_URL}" "${raw}"
  require_checksum "${raw}" "${MULTUS_MANIFEST_SHA256}"
  sed "s|${MULTUS_IMAGE_REPO}:${MULTUS_MANIFEST_TAG}|${MULTUS_IMAGE}|g" \
    "${raw}" > "${rendered}"
  grep -qF "${MULTUS_IMAGE}" "${rendered}" ||
    die "Multus manifest does not carry ${MULTUS_IMAGE}"
  if grep -q "${MULTUS_MANIFEST_TAG}" "${rendered}"; then
    die "Multus manifest still references ${MULTUS_MANIFEST_TAG}"
  fi
  kubectl apply -f "${rendered}"
  kubectl -n "${MULTUS_NAMESPACE}" rollout status \
    "ds/${MULTUS_DAEMONSET}" --timeout="${E2E_WAIT_TIMEOUT}s"
}

# assert_multus_chain checks that Multus took over as master config while the
# kindnet delegate config stayed in place, and that both CNI binaries exist.
assert_multus_chain() {
  local node listing master delegate content nodes
  nodes="$(cluster_nodes)" ||
    die "kind cannot enumerate exactly two nodes for ${E2E_CLUSTER_NAME}"
  while IFS= read -r node; do
    master=""
    for _ in $(seq 1 30); do
      listing="$("${RUNTIME}" exec "${node}" ls -1 /etc/cni/net.d)"
      master="$(printf '%s\n' "${listing}" | awk '/multus/ { print; exit }')"
      [ -n "${master}" ] && break
      sleep 2
    done
    [ -n "${master}" ] ||
      die "node ${node}: Multus did not generate a master CNI config"
    delegate="$(printf '%s\n' "${listing}" | awk '!/multus/ && !seen++')"
    [ -n "${delegate}" ] ||
      die "node ${node}: Multus replaced the kindnet delegate config instead of chaining to it"
    content="$("${RUNTIME}" exec "${node}" cat "/etc/cni/net.d/${master}")"
    case "${content}" in
      *multus-shim*) ;;
      *) die "node ${node}: ${master} does not configure multus-shim" ;;
    esac
    case "${content}" in
      *\"clusterNetwork\":\"/host/etc/cni/net.d/${delegate}\"*) ;;
      *) die "node ${node}: ${master} does not delegate to ${delegate}" ;;
    esac
    "${RUNTIME}" exec "${node}" test -x /opt/cni/bin/multus-shim ||
      die "node ${node}: /opt/cni/bin/multus-shim is missing"
    "${RUNTIME}" exec "${node}" test -x /opt/cni/bin/bridge ||
      die "node ${node}: /opt/cni/bin/bridge is missing"
    log "node ${node}: ${master} chains to ${delegate}"
  done <<< "${nodes}"
}

ensure_namespaces() {
  local ns
  for ns in "${KIH_HELPER_NAMESPACE}" "${KIH_WORKLOAD_NAMESPACE}"; do
    kubectl create namespace "${ns}" --dry-run=client -o yaml | kubectl apply -f -
  done
}

apply_nad() {
  # The contracted NetworkAttachmentDefinition: bridge ${KIH_BRIDGE_NAME} with no
  # CNI IPAM, since kubevirt-ip-helper itself serves the addresses.
  kubectl apply -f - << EOF
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ${KIH_NAD_NAME}
  namespace: ${KIH_HELPER_NAMESPACE}
spec:
  config: '{"cniVersion":"0.3.1","name":"${KIH_NAD_NAME}","bridge":"${KIH_BRIDGE_NAME}","type":"bridge"}'
EOF
}

# wait_for_probe blocks until the named probe pod is Ready.
wait_for_probe() { # <pod>
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" wait \
    --for=condition=Ready "pod/$1" --timeout="${E2E_PROBE_TIMEOUT}s"
}

# run_cni_preflight proves both halves of the chain before KubeVirt is touched:
# a plain pod for the Multus -> kindnet delegation and an NAD-attached pod for
# the secondary interface that the helper later serves addresses on.
run_cni_preflight() {
  ensure_namespaces
  apply_nad
  local node pod pod_ip status nodes
  local index=0

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete \
    "pod/${KIH_PLAIN_PROBE_POD}" --ignore-not-found
  kubectl apply -f - << EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${KIH_PLAIN_PROBE_POD}
  namespace: ${KIH_WORKLOAD_NAMESPACE}
spec:
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
EOF
  wait_for_probe "${KIH_PLAIN_PROBE_POD}"
  pod_ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get \
    "pod/${KIH_PLAIN_PROBE_POD}" -o jsonpath='{.status.podIP}')"
  [ -n "${pod_ip}" ] ||
    die "plain probe pod has no IP: the Multus to kindnet chain does not work"
  log "plain probe pod on ${pod_ip}: delegate CNI chain works"

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete pods \
    -l app=kubevirt-ip-helper-e2e-nad-probe --ignore-not-found
  nodes="$(cluster_nodes)" ||
    die "kind cannot enumerate exactly two nodes for ${E2E_CLUSTER_NAME}"
  while IFS= read -r node; do
    index=$((index + 1))
    pod="${KIH_NAD_PROBE_POD}-${index}"
    kubectl apply -f - << EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: ${KIH_WORKLOAD_NAMESPACE}
  labels:
    app: kubevirt-ip-helper-e2e-nad-probe
  annotations:
    k8s.v1.cni.cncf.io/networks: '[{"name":"${KIH_NAD_NAME}","namespace":"${KIH_HELPER_NAMESPACE}","interface":"${KIH_HELPER_INTERFACE}"}]'
spec:
  nodeName: ${node}
  tolerations:
    - operator: Exists
      effect: NoSchedule
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
EOF
    wait_for_probe "${pod}"
    status="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get "pod/${pod}" \
      -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}')"
    case "${status}" in
      *"${KIH_HELPER_INTERFACE}"*) ;;
      *) die "NAD probe ${pod} network-status '${status:-empty}' lacks interface ${KIH_HELPER_INTERFACE}" ;;
    esac
    "${RUNTIME}" exec "${node}" ip -o link show "${KIH_BRIDGE_NAME}" > /dev/null 2>&1 ||
      die "node ${node}: bridge ${KIH_BRIDGE_NAME} was not created by its NAD probe"
    log "node ${node}: NAD probe attached ${KIH_HELPER_INTERFACE}; bridge ${KIH_BRIDGE_NAME} exists"
  done <<< "${nodes}"

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete \
    "pod/${KIH_PLAIN_PROBE_POD}" --ignore-not-found
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete pods \
    -l app=kubevirt-ip-helper-e2e-nad-probe --ignore-not-found
}

ensure_virtctl() {
  local sha_var expected have
  sha_var="VIRTCTL_SHA256_$(upper_arch "${KIND_ARCH}")"
  expected="${!sha_var}"
  if [ -x "${VIRTCTL}" ] && checksum_matches "${VIRTCTL}" "${expected}"; then
    log "using cached ${VIRTCTL}"
    return
  fi
  fetch "${VIRTCTL_BASE_URL}/virtctl-${KUBEVIRT_VERSION}-${KIND_ARCH}" "${VIRTCTL}"
  chmod 755 "${VIRTCTL}"
  require_checksum "${VIRTCTL}" "${expected}"
  have="$("${VIRTCTL}" version --client 2>&1 || true)"
  case "${have}" in
    *"${KUBEVIRT_VERSION}"*) log "virtctl ready: ${have}" ;;
    *) die "${VIRTCTL} does not report KubeVirt ${KUBEVIRT_VERSION}: '${have:-nothing}'" ;;
  esac
}

ensure_kubevirt() {
  local operator="${E2E_CACHE_DIR}/kubevirt-operator-${KUBEVIRT_VERSION}.yaml"
  local emulation
  fetch "${KUBEVIRT_OPERATOR_URL}" "${operator}"
  require_checksum "${operator}" "${KUBEVIRT_OPERATOR_SHA256}"
  kubectl apply -f "${operator}"
  kubectl -n "${KUBEVIRT_NAMESPACE}" rollout status \
    deploy/virt-operator --timeout="${E2E_WAIT_TIMEOUT}s"
  # The CR is generated rather than patched, so the very first reconcile already
  # runs with software emulation: kind nodes may expose no /dev/kvm.
  kubectl apply -f - << EOF
apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata:
  name: ${KUBEVIRT_CR_NAME}
  namespace: ${KUBEVIRT_NAMESPACE}
spec:
  configuration:
    developerConfiguration:
      useEmulation: true
EOF
  kubectl -n "${KUBEVIRT_NAMESPACE}" wait \
    --for="jsonpath={.status.targetKubeVirtVersion}=${KUBEVIRT_VERSION}" \
    "kubevirt/${KUBEVIRT_CR_NAME}" --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl -n "${KUBEVIRT_NAMESPACE}" wait \
    --for="jsonpath={.status.observedKubeVirtVersion}=${KUBEVIRT_VERSION}" \
    "kubevirt/${KUBEVIRT_CR_NAME}" --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl -n "${KUBEVIRT_NAMESPACE}" wait \
    --for=condition=Available "kubevirt/${KUBEVIRT_CR_NAME}" \
    --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl -n "${KUBEVIRT_NAMESPACE}" rollout status \
    deploy/virt-api --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl -n "${KUBEVIRT_NAMESPACE}" rollout status \
    deploy/virt-controller --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl -n "${KUBEVIRT_NAMESPACE}" rollout status \
    ds/virt-handler --timeout="${E2E_WAIT_TIMEOUT}s"
  emulation="$(kubectl -n "${KUBEVIRT_NAMESPACE}" get \
    "kubevirt/${KUBEVIRT_CR_NAME}" \
    -o jsonpath='{.spec.configuration.developerConfiguration.useEmulation}')"
  [ "${emulation}" = "true" ] ||
    die "KubeVirt CR has useEmulation='${emulation:-unset}', expected true"
  log "KubeVirt ${KUBEVIRT_VERSION} available with software emulation"
}

bootstrap_select_arch() {
  KIND_ARCH="$(arch_suffix)"
}

main() {
  # Every pin below comes from the selected profile in versions.env, so the
  # bootstrap evidence stays attributable to one lane.
  log "stack profile '${E2E_STACK}': Kubernetes ${KUBERNETES_VERSION}, \
KubeVirt ${KUBEVIRT_VERSION}, guest ${KIH_GUEST_IMAGE}"
  bootstrap_gate BOOTSTRAP-ARCH "linux amd64 architecture is supported" \
    bootstrap_select_arch
  bootstrap_gate BOOTSTRAP-RUNTIME "container runtime is available" resolve_runtime
  bootstrap_gate BOOTSTRAP-KIND "pinned kind binary is available" ensure_kind
  bootstrap_gate BOOTSTRAP-CLUSTER "pinned two-node kind cluster is ready" ensure_cluster
  bootstrap_gate BOOTSTRAP-IMAGE "helper image is loaded into the cluster" ensure_helper_image
  bootstrap_gate BOOTSTRAP-KINDNET "kindnet delegate is present on every node" inspect_kindnet
  bootstrap_gate BOOTSTRAP-BRIDGE "pinned bridge CNI is installed on every node" ensure_bridge_plugin
  bootstrap_gate BOOTSTRAP-MULTUS "pinned Multus is installed" ensure_multus
  bootstrap_gate BOOTSTRAP-MULTUS-CHAIN "Multus chains to kindnet on every node" assert_multus_chain
  bootstrap_gate BOOTSTRAP-CNI-PREFLIGHT "plain and NAD-attached CNI probes are Ready" \
    run_cni_preflight
  bootstrap_gate BOOTSTRAP-VIRTCTL "pinned virtctl is available" ensure_virtctl
  bootstrap_gate BOOTSTRAP-KUBEVIRT "KubeVirt is Available with software emulation" ensure_kubevirt
  log "cluster '${E2E_CLUSTER_NAME}' bootstrapped"
}

main "$@"

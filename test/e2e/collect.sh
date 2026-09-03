#!/usr/bin/env bash
# Best-effort failure/success evidence for test/e2e/run.sh. Every command is
# read-only and every capture failure is recorded instead of replacing the E2E
# result that caused collection.
#
# The numbered diagnostics below are this script's own files and are rewritten on
# each collection. Checkpoint evidence under evidence/ belongs to evidence.sh: it
# is never truncated or rewritten here, and the run's only shared append target is
# evidence/capture-errors.txt, which stays append-only so every failure of the run
# remains visible in one ordered list.
set -uo pipefail

E2E_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${E2E_DIR}/../.." && pwd)"
# shellcheck source=test/e2e/versions.env
. "${E2E_DIR}/versions.env"
# shellcheck source=test/e2e/evidence.sh
. "${E2E_DIR}/evidence.sh"

case "${E2E_ARTIFACTS_DIR}" in
  /*) ;;
  *) E2E_ARTIFACTS_DIR="${ROOT_DIR}/${E2E_ARTIFACTS_DIR}" ;;
esac
collect_prepare_run_dir() {
  local dir="$1"
  local -a existing=()
  if [ -d "${dir}" ]; then
    shopt -s nullglob dotglob
    existing=("${dir}"/*)
    shopt -u nullglob dotglob
    if [ "${#existing[@]}" -gt 0 ]; then
      printf '[e2e] ERROR: run directory %s already holds %s entry(ies); rerun with a fresh E2E_RUN_ID\n' \
        "${dir}" "${#existing[@]}" >&2
      return 1
    fi
  elif ! mkdir -p "${dir}"; then
    printf '[e2e] ERROR: cannot create run directory %s\n' "${dir}" >&2
    return 1
  fi
  return 0
}
if [ "${E2E_COLLECTION_IN_PROGRESS:-}" = 1 ]; then
  mkdir -p "${E2E_ARTIFACTS_DIR}"
else
  collect_prepare_run_dir "${E2E_ARTIFACTS_DIR}" || exit 1
fi
# The evidence helpers are also usable when collect.sh runs on its own, so the run
# directory, the previous-run pointer, and the checksum manifest do not depend on
# run.sh having reached its own evidence_init call.
evidence_init || true

# evidence_index summarises the artifact layout, every checkpoint, the comparison
# status against the previous run, and the recorded capture errors.
evidence_index() {
  local manifest comparison order names checkpoint dir file size
  manifest="${E2E_ARTIFACTS_DIR}/artifact-manifest.sha256"
  comparison="${E2E_ARTIFACTS_DIR}/evidence/comparison-to-previous-run.json"
  order="${E2E_ARTIFACTS_DIR}/evidence/checkpoints/.order"
  {
    printf '# evidence index for run %s\n' "${E2E_RUN_ID}"
    printf 'run directory: %s\n' "$(_evidence_portable_path "${E2E_ARTIFACTS_DIR}")"
    printf 'artifact root: %s\n' "$(_evidence_portable_path "${E2E_ARTIFACTS_ROOT}")"
    if [ -n "${E2E_PREVIOUS_RUN_DIR:-}" ]; then
      printf 'previous run: %s\n' "$(_evidence_portable_path "${E2E_PREVIOUS_RUN_DIR}")"
    else
      printf 'previous run: none\n'
    fi
    printf 'latest pointer: %s\n' \
      "$(_evidence_portable_path "${E2E_ARTIFACTS_ROOT}/${E2E_LATEST_NAME}")"
    printf '\n===== run artifacts =====\n'
    for checkpoint in "${E2E_ARTIFACTS_DIR}"/*; do
      [ -e "${checkpoint}" ] || continue
      if [ -d "${checkpoint}" ]; then
        printf -- '-- %s/\n' "$(basename "${checkpoint}")"
      else
        printf -- '-- %s (%s bytes)\n' "$(basename "${checkpoint}")" \
          "$(wc -c < "${checkpoint}")"
      fi
    done
    printf '\n===== checkpoints =====\n'
    if [ -s "${order}" ]; then
      names="$(cat "${order}")"
    else
      names=""
      for checkpoint in "${E2E_ARTIFACTS_DIR}"/evidence/checkpoints/*/; do
        [ -d "${checkpoint}" ] || continue
        names="${names}$(basename "${checkpoint}")
"
      done
    fi
    for checkpoint in ${names}; do
      dir="${E2E_ARTIFACTS_DIR}/evidence/checkpoints/${checkpoint}"
      printf -- '-- %s\n' "${checkpoint}"
      for file in raw.json normalized.json changes-from-previous.json \
        comparison-to-previous-run.json observations.txt; do
        [ -f "${dir}/${file}" ] || continue
        size="$(wc -c < "${dir}/${file}")"
        printf '   %s %s bytes\n' "${file}" "${size}"
      done
      jq -e 'has("objects")' "${dir}/normalized.json" > /dev/null 2>&1 &&
        printf '   objects %s\n' "$(jq '.objects | length' "${dir}/normalized.json")"
      jq -e 'has("added")' "${dir}/changes-from-previous.json" > /dev/null 2>&1 &&
        jq -c '
          "   versus \(.previousCheckpoint // "nothing"): "
          + "added \(.added | length), removed \(.removed | length), changed \(.changed | length)"
        ' "${dir}/changes-from-previous.json"
      jq -e 'has("status")' "${dir}/comparison-to-previous-run.json" > /dev/null 2>&1 &&
        jq -c '"   versus previous run: \(.status) (\(.previousCheckpoint // "none"))"' \
          "${dir}/comparison-to-previous-run.json"
    done
    printf '\n===== comparison =====\n'
    if [ -s "${comparison}" ]; then
      jq -S '{status, previousCheckpoint, counts,
              added: (.added | length), removed: (.removed | length),
              changed: (.changed | length)}' "${comparison}"
    else
      printf 'no comparison written\n'
    fi
    printf '\n===== recorded capture errors =====\n'
    if [ -s "${E2E_ARTIFACTS_DIR}/evidence/capture-errors.txt" ]; then
      cat "${E2E_ARTIFACTS_DIR}/evidence/capture-errors.txt"
    else
      printf 'none\n'
    fi
    printf '\n===== checksums =====\n'
    if [ -s "${manifest}" ]; then
      printf 'manifest present; final checksum verification runs after this index is written\n'
    else
      printf 'no manifest written yet; finalization will create it\n'
    fi
  } > "${E2E_ARTIFACTS_DIR}/11-evidence.txt" 2>&1
  printf 'diagnostics collected in %s\n' "${E2E_ARTIFACTS_DIR}"
  return 0
}

# finish closes the run: the comparison against the previous run and the checksum
# manifest are written before any exit path, so even a collection that stopped at a
# missing kind binary leaves a complete, verifiable artifact set. Only these
# finalization steps are fatal; a skipped cluster diagnostic is recorded instead.
COLLECT_FINISHED=0
finish() { # <exit-code>
  local rc="$1" manifest entries standalone=0
  manifest="${E2E_ARTIFACTS_DIR}/artifact-manifest.sha256"
  if [ "${COLLECT_FINISHED}" -eq 1 ]; then
    return "${rc}"
  fi
  COLLECT_FINISHED=1
  trap - EXIT
  trap '' INT TERM HUP
  if [ "${rc}" -eq 0 ]; then
    :
  else
    _evidence_record_error "collection" "diagnostics incomplete for exit ${rc}" || true
  fi
  if [ "${E2E_DEFER_LATEST:-}" != 1 ] &&
    ! declare -F report_finalize > /dev/null 2>&1; then
    standalone=1
    if [ -s "${E2E_ARTIFACTS_DIR}/evidence/capture-errors.txt" ]; then
      _evidence_record_error "collection" \
        "existing evidence/capture-errors.txt prevents latest publication" || true
      [ "${rc}" -ne 0 ] || rc=1
    fi
    if [ ! -s "${E2E_ARTIFACTS_DIR}/report.json" ] ||
      ! jq -e '.schemaVersion == 1 and .status == "passed" and .exitCode == 0' \
        "${E2E_ARTIFACTS_DIR}/report.json" > /dev/null 2>&1; then
      _evidence_record_error "collection" \
        "standalone collection requires a passing report before latest publication" || true
      [ "${rc}" -ne 0 ] || rc=1
    fi
  fi
  if ! evidence_finalize; then
    [ "${rc}" -ne 0 ] || rc=1
  fi
  if ! evidence_index; then
    [ "${rc}" -ne 0 ] || rc=1
  fi
  # evidence_index writes 11-evidence.txt, so the manifest is rewritten after it and
  # then checked to actually name that file.
  if ! evidence_finalize; then
    [ "${rc}" -ne 0 ] || rc=1
  fi
  entries="$(wc -l < "${manifest}" 2> /dev/null || printf '0')"
  if [ ! -s "${manifest}" ] || ! grep -qF './11-evidence.txt' "${manifest}"; then
    _evidence_fail "manifest" "${manifest} does not cover 11-evidence.txt" || true
    rc=1
    # The failure record itself must be included in the final checksum walk.
    if ! evidence_finalize; then
      [ "${rc}" -ne 0 ] || rc=1
    fi
  fi
  if [ "${rc}" -eq 0 ] && [ "${standalone}" -eq 1 ]; then
    _evidence_link_latest "${E2E_ARTIFACTS_ROOT}" \
      "${E2E_RUNS_SUBDIR}/${E2E_RUN_ID}" || rc=1
  fi
  printf 'diagnostics finalized in %s (%s manifest entries)\n' \
    "${E2E_ARTIFACTS_DIR}" "${entries}"
  exit "${rc}"
}
collector_signal() {
  finish "$1"
}
trap 'collector_signal 130' INT
trap 'collector_signal 143' TERM
trap 'collector_signal 129' HUP
trap 'finish "$?"' EXIT


KIND="${E2E_BIN_DIR}/kind"
if [ ! -x "${KIND}" ]; then
  KIND="$(command -v kind 2> /dev/null || true)"
fi
RUNTIME=""
for candidate in docker podman; do
  if command -v "${candidate}" > /dev/null 2>&1 &&
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${candidate}" info > /dev/null 2>&1; then
    RUNTIME="${candidate}"
    [ "${candidate}" != podman ] || export KIND_EXPERIMENTAL_PROVIDER=podman
    break
  fi
done

capture() { # <file> <heading> <command...>
  local file="$1" heading="$2" rc=0 write_rc=0 output
  shift 2
  output="${E2E_ARTIFACTS_DIR}/.capture-output-${BASHPID}.tmp"
  rm -f "${output}"
  {
    printf '\n===== %s =====\n' "${heading}"
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "$@" > "${output}" 2>&1
    rc=$?
    if ! cat "${output}"; then
      write_rc=1
    fi
    [ "${rc}" -eq 0 ] || printf '[capture failed: exit %s]\n' "${rc}"
  } >> "${E2E_ARTIFACTS_DIR}/${file}" 2>&1 || write_rc=1
  # The group above ran in this same shell, so its rc survived the redirect.
  if [ "${write_rc}" -ne 0 ]; then
    _evidence_record_error "diagnostics" "${heading} output file write failed" || true
  fi
  if [ "${rc}" -ne 0 ]; then
    if [[ "${heading}" == *"previous logs" ]] &&
      grep -Eqi 'previous.*(not found|does not exist)' \
        "${E2E_ARTIFACTS_DIR}/.capture-output-${BASHPID}.tmp" 2> /dev/null; then
      # A first run normally has no terminated container to inspect. Keep the
      # command output, but do not classify that optional absence as a gap.
      :
    else
      # A failed diagnostic is an explicit record, not a silent gap in the
      # artifact set: the reason has to be readable without opening every
      # numbered file.
      _evidence_record_error "diagnostics" "${heading} exited ${rc}" || true
    fi
  fi
  if ! rm -f "${output}"; then
    _evidence_record_error "diagnostics" "${heading} temporary output cleanup failed" || true
  fi
  return 0
}
collector_list() { # <subject> <command> <arguments...>
  local subject="$1" command output err rc=0
  shift
  command="$1"
  shift
  err="${E2E_ARTIFACTS_DIR}/.collector-list-${BASHPID}.err"
  output="$(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${command}" "$@" \
    2> "${err}")" || rc=$?
  if [ "${rc}" -ne 0 ]; then
    _evidence_record_error "diagnostics" \
      "${subject} listing exited ${rc}: $(tr '\n' ' ' < "${err}")" || true
    rm -f "${err}"
    return 1
  fi
  rm -f "${err}"
  printf '%s\n' "${output}"
  return 0
}

{
  printf 'collected: %s\n' "$(date -Is 2> /dev/null || date)"
  printf 'cluster: %s\n' "${E2E_CLUSTER_NAME}"
  printf 'image: %s\n' "${E2E_IMAGE}"
  printf 'stack: %s\n' "${E2E_STACK}"
  printf 'kubernetes: %s\n' "${KUBERNETES_VERSION}"
  printf 'kubevirt: %s\n' "${KUBEVIRT_VERSION}"
  printf 'guest image: %s\n' "${KIH_GUEST_IMAGE}"
  printf 'kernel: %s\n' "$(uname -srmo)"
  printf 'runtime: %s\n' "${RUNTIME:-not found}"
  printf 'kind: %s\n' "${KIND:-not found}"
} > "${E2E_ARTIFACTS_DIR}/00-environment.txt"

if [ -z "${KIND}" ]; then
  printf 'kind binary unavailable; cluster diagnostics skipped\n' >> "${E2E_ARTIFACTS_DIR}/00-environment.txt"
  _evidence_record_error "diagnostics" "kind binary unavailable; cluster diagnostics skipped" || true
  finish 0
fi
if ! timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${KIND}" get clusters 2> /dev/null |
  grep -qx "${E2E_CLUSTER_NAME}"; then
  printf 'kind cluster %s does not exist; cluster diagnostics skipped\n' \
    "${E2E_CLUSTER_NAME}" >> "${E2E_ARTIFACTS_DIR}/00-environment.txt"
  _evidence_record_error "diagnostics" \
    "kind cluster ${E2E_CLUSTER_NAME} does not exist; cluster diagnostics skipped" || true
  finish 0
fi

kubeconfig_rc=0
if timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${KIND}" get kubeconfig \
  --name "${E2E_CLUSTER_NAME}" > "${E2E_ARTIFACTS_DIR}/kubeconfig" \
  2>> "${E2E_ARTIFACTS_DIR}/00-environment.txt"; then
  :
else
  kubeconfig_rc=$?
  _evidence_record_error "diagnostics" \
    "kind kubeconfig export exited ${kubeconfig_rc}; cluster diagnostics skipped" || true
  finish 1
fi
export KUBECONFIG="${E2E_ARTIFACTS_DIR}/kubeconfig"

: > "${E2E_ARTIFACTS_DIR}/01-cluster.txt"
: > "${E2E_ARTIFACTS_DIR}/02-network.txt"
: > "${E2E_ARTIFACTS_DIR}/03-kubevirt.txt"
: > "${E2E_ARTIFACTS_DIR}/04-helper.txt"
: > "${E2E_ARTIFACTS_DIR}/05-helper-logs.txt"
: > "${E2E_ARTIFACTS_DIR}/06-ipam-vm.txt"
: > "${E2E_ARTIFACTS_DIR}/07-vm-logs.txt"
: > "${E2E_ARTIFACTS_DIR}/08-netns.txt"
: > "${E2E_ARTIFACTS_DIR}/09-runtime.txt"

capture 01-cluster.txt "cluster info" kubectl cluster-info
capture 01-cluster.txt "nodes" kubectl get nodes -o wide
capture 01-cluster.txt "node descriptions" kubectl describe nodes
capture 01-cluster.txt "all pods" kubectl get pods -A -o wide
capture 01-cluster.txt "all events" kubectl get events -A --sort-by=.lastTimestamp
capture 01-cluster.txt "api resources" kubectl api-resources

capture 02-network.txt "kube-system workloads" kubectl -n kube-system get pods,daemonsets,deployments -o wide
capture 02-network.txt "Multus DaemonSet" kubectl -n kube-system get daemonset/kube-multus-ds -o yaml
capture 02-network.txt "all NetworkAttachmentDefinitions" kubectl get network-attachment-definitions -A -o yaml
for pod in $(collector_list "Multus pod listing" kubectl -n kube-system \
  get pods -l app=multus -o jsonpath='{.items[*].metadata.name}' || true); do
  capture 02-network.txt "${pod} Multus logs" kubectl -n kube-system logs "${pod}" -c kube-multus --tail=-1 --prefix
  capture 02-network.txt "${pod} Multus previous logs" kubectl -n kube-system logs "${pod}" -c kube-multus --previous --tail=-1 --prefix
done

capture 03-kubevirt.txt "KubeVirt resources" kubectl -n kubevirt get kubevirt,deployments,daemonsets,pods -o wide
capture 03-kubevirt.txt "KubeVirt CR" kubectl -n kubevirt get kubevirt/kubevirt -o yaml
for component in virt-operator virt-controller virt-api; do
  capture 03-kubevirt.txt "${component} logs" kubectl -n kubevirt logs "deployment/${component}" --all-containers --tail=-1 --prefix
  capture 03-kubevirt.txt "${component} previous logs" kubectl -n kubevirt logs "deployment/${component}" --all-containers --previous --tail=-1 --prefix
done
for pod in $(collector_list "virt-handler pod listing" kubectl -n kubevirt \
  get pods -l kubevirt.io=virt-handler -o jsonpath='{.items[*].metadata.name}' || true); do
  capture 03-kubevirt.txt "${pod} logs" kubectl -n kubevirt logs "${pod}" --all-containers --tail=-1 --prefix
done

capture 04-helper.txt "helper deployment" kubectl -n "${KIH_HELPER_NAMESPACE}" get deployment/kubevirt-ip-helper -o yaml
capture 04-helper.txt "helper pods" kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l app=kubevirt-ip-helper -o yaml
capture 04-helper.txt "leader Lease" kubectl -n "${KIH_HELPER_NAMESPACE}" get lease/kubevirt-ip-helper-lock -o yaml
capture 04-helper.txt "metrics Service" kubectl -n "${KIH_HELPER_NAMESPACE}" get service/kubevirt-ip-helper-metrics -o yaml
capture 04-helper.txt "metrics Endpoints" kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints/kubevirt-ip-helper-metrics -o yaml
capture 04-helper.txt "metrics EndpointSlices" kubectl -n "${KIH_HELPER_NAMESPACE}" get endpointslices -l kubernetes.io/service-name=kubevirt-ip-helper-metrics -o yaml
capture 04-helper.txt "helper events" kubectl -n "${KIH_HELPER_NAMESPACE}" get events --sort-by=.lastTimestamp
for pod in $(collector_list "helper pod listing" kubectl -n "${KIH_HELPER_NAMESPACE}" \
  get pods -l app=kubevirt-ip-helper \
  -o jsonpath='{.items[*].metadata.name}' || true); do
  capture 05-helper-logs.txt "${pod} logs" kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}" --tail=-1 --prefix
  capture 05-helper-logs.txt "${pod} previous logs" kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}" --previous --tail=-1 --prefix
done

capture 06-ipam-vm.txt "IPPools" kubectl get ippools -o yaml
capture 06-ipam-vm.txt "VirtualMachineNetworkConfigs" kubectl get vmnetcfg -A -o yaml
capture 06-ipam-vm.txt "VirtualMachines" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vm -o yaml
capture 06-ipam-vm.txt "VirtualMachineInstances" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmi -o yaml
capture 06-ipam-vm.txt "workload pods" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get pods -o yaml
capture 06-ipam-vm.txt "workload events" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get events --sort-by=.lastTimestamp
for pod in $(collector_list "workload pod listing" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" \
  get pods -o jsonpath='{.items[*].metadata.name}' || true); do
  case "${pod}" in
    virt-launcher-*) capture 07-vm-logs.txt "${pod} logs" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" logs "${pod}" --all-containers --tail=-1 --prefix ;;
  esac
done

for pod in $(collector_list "leader pod listing" kubectl -n "${KIH_HELPER_NAMESPACE}" \
  get pods -l kubevirtiphelper/leader=active \
  -o jsonpath='{.items[*].metadata.name}' || true); do
  capture 08-netns.txt "${pod} addresses" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- ip addr
  capture 08-netns.txt "${pod} routes" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- ip route
  capture 08-netns.txt "${pod} UDP sockets" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- cat /proc/net/udp
  capture 08-netns.txt "${pod} metrics" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- wget -qO- http://127.0.0.1:8080/
done

if [ -n "${RUNTIME}" ]; then
  capture 09-runtime.txt "runtime version" "${RUNTIME}" --version
  capture 09-runtime.txt "runtime info" "${RUNTIME}" info
  capture 09-runtime.txt "cluster containers" "${RUNTIME}" ps --filter "name=${E2E_CLUSTER_NAME}"
  for node in $(collector_list "kind node listing" "${KIND}" \
    get nodes --name "${E2E_CLUSTER_NAME}" || true); do
    capture 09-runtime.txt "${node} CNI files" "${RUNTIME}" exec "${node}" sh -c 'ls -la /etc/cni/net.d /opt/cni/bin'
    capture 09-runtime.txt "${node} links and bridges" "${RUNTIME}" exec "${node}" sh -c 'ip addr; bridge link; bridge vlan show'
  done
fi

{
  for file in "${E2E_ARTIFACTS_DIR}"/console-*.log; do
    [ -f "${file}" ] || continue
    printf '===== %s =====\n' "$(basename "${file}")"
    grep -o 'E2E_DHCP_[A-Z]*[^[:space:]]*' "${file}" || true
  done
} > "${E2E_ARTIFACTS_DIR}/10-guest-markers.txt"

# The remaining work happens in finish: comparison against the previous run plus the
# sha256 manifest, then this human-readable index of what the run actually kept.
finish 0

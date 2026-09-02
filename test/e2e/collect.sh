#!/usr/bin/env bash
# Best-effort failure/success evidence for test/e2e/run.sh. Every command is
# read-only and every capture failure is recorded instead of replacing the E2E
# result that caused collection.
set -uo pipefail

E2E_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${E2E_DIR}/../.." && pwd)"
# shellcheck source=test/e2e/versions.env
. "${E2E_DIR}/versions.env"

case "${E2E_ARTIFACTS_DIR}" in
  /*) ;;
  *) E2E_ARTIFACTS_DIR="${ROOT_DIR}/${E2E_ARTIFACTS_DIR}" ;;
esac
mkdir -p "${E2E_ARTIFACTS_DIR}"

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
  local file="$1" heading="$2" rc
  shift 2
  {
    printf '\n===== %s =====\n' "${heading}"
    printf '$'
    printf ' %q' "$@"
    printf '\n'
    timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "$@"
    rc=$?
    [ "${rc}" -eq 0 ] || printf '[capture failed: exit %s]\n' "${rc}"
  } >> "${E2E_ARTIFACTS_DIR}/${file}" 2>&1
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
  exit 0
fi
if ! timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${KIND}" get clusters 2> /dev/null |
  grep -qx "${E2E_CLUSTER_NAME}"; then
  printf 'kind cluster %s does not exist; cluster diagnostics skipped\n' \
    "${E2E_CLUSTER_NAME}" >> "${E2E_ARTIFACTS_DIR}/00-environment.txt"
  exit 0
fi

timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${KIND}" get kubeconfig \
  --name "${E2E_CLUSTER_NAME}" > "${E2E_ARTIFACTS_DIR}/kubeconfig" \
  2>> "${E2E_ARTIFACTS_DIR}/00-environment.txt" || exit 0
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
for pod in $(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl -n kube-system \
  get pods -l app=multus -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
  capture 02-network.txt "${pod} Multus logs" kubectl -n kube-system logs "${pod}" -c kube-multus --tail=-1 --prefix
  capture 02-network.txt "${pod} Multus previous logs" kubectl -n kube-system logs "${pod}" -c kube-multus --previous --tail=-1 --prefix
done

capture 03-kubevirt.txt "KubeVirt resources" kubectl -n kubevirt get kubevirt,deployments,daemonsets,pods -o wide
capture 03-kubevirt.txt "KubeVirt CR" kubectl -n kubevirt get kubevirt/kubevirt -o yaml
for component in virt-operator virt-controller virt-api; do
  capture 03-kubevirt.txt "${component} logs" kubectl -n kubevirt logs "deployment/${component}" --all-containers --tail=-1 --prefix
  capture 03-kubevirt.txt "${component} previous logs" kubectl -n kubevirt logs "deployment/${component}" --all-containers --previous --tail=-1 --prefix
done
for pod in $(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl -n kubevirt \
  get pods -l kubevirt.io=virt-handler -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
  capture 03-kubevirt.txt "${pod} logs" kubectl -n kubevirt logs "${pod}" --all-containers --tail=-1 --prefix
done

capture 04-helper.txt "helper deployment" kubectl -n "${KIH_HELPER_NAMESPACE}" get deployment/kubevirt-ip-helper -o yaml
capture 04-helper.txt "helper pods" kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l app=kubevirt-ip-helper -o yaml
capture 04-helper.txt "leader Lease" kubectl -n "${KIH_HELPER_NAMESPACE}" get lease/kubevirt-ip-helper-lock -o yaml
capture 04-helper.txt "metrics Service" kubectl -n "${KIH_HELPER_NAMESPACE}" get service/kubevirt-ip-helper-metrics -o yaml
capture 04-helper.txt "metrics Endpoints" kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints/kubevirt-ip-helper-metrics -o yaml
capture 04-helper.txt "metrics EndpointSlices" kubectl -n "${KIH_HELPER_NAMESPACE}" get endpointslices -l kubernetes.io/service-name=kubevirt-ip-helper-metrics -o yaml
capture 04-helper.txt "helper events" kubectl -n "${KIH_HELPER_NAMESPACE}" get events --sort-by=.lastTimestamp
for pod in $(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
  -n "${KIH_HELPER_NAMESPACE}" get pods -l app=kubevirt-ip-helper \
  -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
  capture 05-helper-logs.txt "${pod} logs" kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}" --tail=-1 --prefix
  capture 05-helper-logs.txt "${pod} previous logs" kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}" --previous --tail=-1 --prefix
done

capture 06-ipam-vm.txt "IPPools" kubectl get ippools -o yaml
capture 06-ipam-vm.txt "VirtualMachineNetworkConfigs" kubectl get vmnetcfg -A -o yaml
capture 06-ipam-vm.txt "VirtualMachines" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vm -o yaml
capture 06-ipam-vm.txt "VirtualMachineInstances" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmi -o yaml
capture 06-ipam-vm.txt "workload pods" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get pods -o yaml
capture 06-ipam-vm.txt "workload events" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get events --sort-by=.lastTimestamp
for pod in $(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
  -n "${KIH_WORKLOAD_NAMESPACE}" get pods \
  -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
  case "${pod}" in
    virt-launcher-*) capture 07-vm-logs.txt "${pod} logs" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" logs "${pod}" --all-containers --tail=-1 --prefix ;;
  esac
done

for pod in $(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
  -n "${KIH_HELPER_NAMESPACE}" get pods -l kubevirtiphelper/leader=active \
  -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
  capture 08-netns.txt "${pod} addresses" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- ip addr
  capture 08-netns.txt "${pod} routes" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- ip route
  capture 08-netns.txt "${pod} UDP sockets" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- cat /proc/net/udp
  capture 08-netns.txt "${pod} metrics" kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- wget -qO- http://127.0.0.1:8080/
done

if [ -n "${RUNTIME}" ]; then
  capture 09-runtime.txt "runtime version" "${RUNTIME}" --version
  capture 09-runtime.txt "runtime info" "${RUNTIME}" info
  capture 09-runtime.txt "cluster containers" "${RUNTIME}" ps --filter "name=${E2E_CLUSTER_NAME}"
  for node in $(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" "${KIND}" \
    get nodes --name "${E2E_CLUSTER_NAME}" 2> /dev/null); do
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

printf 'diagnostics collected in %s\n' "${E2E_ARTIFACTS_DIR}"
exit 0

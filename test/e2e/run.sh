#!/usr/bin/env bash
set -Eeuo pipefail

E2E_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${E2E_DIR}/../.." && pwd)"
# shellcheck source=test/e2e/versions.env
. "${E2E_DIR}/versions.env"

case "${E2E_ARTIFACTS_DIR}" in
  /*) ;;
  *) E2E_ARTIFACTS_DIR="${ROOT_DIR}/${E2E_ARTIFACTS_DIR}" ;;
esac
mkdir -p "${E2E_ARTIFACTS_DIR}"
export E2E_STACK E2E_CLUSTER_NAME E2E_IMAGE E2E_KEEP_CLUSTER E2E_CACHE_DIR E2E_BIN_DIR
export E2E_ARTIFACTS_DIR VIRTCTL
export KUBECONFIG="${KUBECONFIG:-${E2E_ARTIFACTS_DIR}/kubeconfig}"
E2E_CLUSTER_STATE_FILE="${E2E_ARTIFACTS_DIR}/cluster-state"
export E2E_CLUSTER_STATE_FILE

KIND="${E2E_BIN_DIR}/kind"
HELPER_DEPLOYMENT="kubevirt-ip-helper"
HELPER_SELECTOR="app=kubevirt-ip-helper"
LEADER_SELECTOR="kubevirtiphelper/leader=active"
LEADER_LEASE="kubevirt-ip-helper-lock"
METRICS_SERVICE="kubevirt-ip-helper-metrics"
E2E_VM_BOOT_TIMEOUT="${E2E_VM_BOOT_TIMEOUT:-300}"
LEADER_POD=""
LEADER_ID=""
RESERVED_IP=""
RUNTIME=""
CONSOLE_PID=""
CONSOLE_FEEDER_PID=""
CONSOLE_FIFO=""
CLUSTER_STATE=""

log() { printf '[e2e] %s\n' "$*"; }
die() { printf '[e2e] ERROR: %s\n' "$*" >&2; exit 1; }

keep_cluster() {
  case "${E2E_KEEP_CLUSTER}" in
    1 | true | yes) return 0 ;;
    *) return 1 ;;
  esac
}

finish() {
  local rc=$?
  trap - EXIT
  set +e
  if [ -n "${CONSOLE_PID}" ]; then
    kill "${CONSOLE_PID}" > /dev/null 2>&1 || true
  fi
  if [ -n "${CONSOLE_FEEDER_PID}" ]; then
    kill "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
  fi
  [ -z "${CONSOLE_FIFO}" ] || rm -f "${CONSOLE_FIFO}"
  if [ -s "${E2E_CLUSTER_STATE_FILE}" ]; then
    CLUSTER_STATE="$(cat "${E2E_CLUSTER_STATE_FILE}")"
  fi
  if [ -n "${CLUSTER_STATE}" ] && [ -x "${KIND}" ] && [ -s "${KUBECONFIG}" ]; then
    timeout --foreground "${E2E_COLLECT_TOTAL_TIMEOUT}s" "${E2E_DIR}/collect.sh" || true
  fi
  if keep_cluster && [ -n "${CLUSTER_STATE}" ]; then
    log "kept cluster ${E2E_CLUSTER_NAME}; kubeconfig: ${KUBECONFIG}"
  elif [ "${CLUSTER_STATE}" = "owned" ] && [ -x "${KIND}" ]; then
    "${KIND}" delete cluster --name "${E2E_CLUSTER_NAME}" || true
  elif [ "${CLUSTER_STATE}" = "reused" ]; then
    log "left pre-existing cluster ${E2E_CLUSTER_NAME} in place"
  fi
  exit "${rc}"
}
trap finish EXIT

wait_for() { # <seconds> <description> <predicate> [args...]
  local timeout="$1" description="$2"
  shift 2
  local deadline=$((SECONDS + timeout))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    if "$@"; then
      if [ "${SECONDS}" -lt "${deadline}" ]; then
        log "ok: ${description}"
        return 0
      fi
      break
    fi
    sleep 2
  done
  die "timed out after ${timeout}s: ${description}"
}

wait_before_deadline() { # <absolute SECONDS> <maximum seconds> <description> <predicate> [args...]
  local deadline="$1" maximum="$2" description="$3" remaining
  shift 3
  remaining=$((deadline - SECONDS))
  [ "${remaining}" -gt 0 ] ||
    die "deadline expired before ${description}"
  [ "${remaining}" -le "${maximum}" ] || remaining="${maximum}"
  wait_for "${remaining}" "${description}" "$@"
}

resolve_runtime() {
  if command -v docker > /dev/null 2>&1 && docker info > /dev/null 2>&1; then
    RUNTIME=docker
  elif command -v podman > /dev/null 2>&1 && podman info > /dev/null 2>&1; then
    RUNTIME=podman
    export KIND_EXPERIMENTAL_PROVIDER=podman
  else
    die "no usable Docker or Podman runtime is available"
  fi
  command -v kubectl > /dev/null 2>&1 || die "kubectl is required"
  command -v timeout > /dev/null 2>&1 || die "GNU timeout is required"
}

helper_pods_ready() {
  local pods pod ready status
  local -a pod_names
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)"
  read -r -a pod_names <<< "${pods}"
  [ "${#pod_names[@]}" -eq 2 ] || return 1
  for pod in "${pod_names[@]}"; do
    ready="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2> /dev/null)"
    [ "${ready}" = "True" ] || return 1
    status="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}' 2> /dev/null)"
    case "${status}" in
      *"${KIH_HELPER_INTERFACE}"*) ;;
      *) return 1 ;;
    esac
    kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
      ip link show "${KIH_HELPER_INTERFACE}" > /dev/null 2>&1 || return 1
  done
}

leader_consistent() {
  local pods holder generated endpoint_ips pod_ip
  local -a pod_names endpoint_addresses
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${LEADER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)"
  read -r -a pod_names <<< "${pods}"
  [ "${#pod_names[@]}" -eq 1 ] || return 1
  LEADER_POD="${pod_names[0]}"
  holder="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get lease "${LEADER_LEASE}" \
    -o jsonpath='{.spec.holderIdentity}' 2> /dev/null)"
  [ -n "${holder}" ] || return 1
  generated="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${LEADER_POD}" 2> /dev/null |
    grep -oE 'generated leader id: [0-9a-f-]+' | awk '{print $4}' | tail -n 1)"
  [ "${generated}" = "${holder}" ] || return 1
  endpoint_ips="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints "${METRICS_SERVICE}" \
    -o jsonpath='{.subsets[*].addresses[*].ip}' 2> /dev/null)"
  read -r -a endpoint_addresses <<< "${endpoint_ips}"
  [ "${#endpoint_addresses[@]}" -eq 1 ] || return 1
  pod_ip="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${LEADER_POD}" \
    -o jsonpath='{.status.podIP}' 2> /dev/null)"
  [ "${endpoint_addresses[0]}" = "${pod_ip}" ] || return 1
  LEADER_ID="${holder}"
}

pool_initialized() {
  local last used available
  last="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.lastupdate}' 2> /dev/null)"
  used="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)"
  available="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)"
  case "${used}" in "" | 0) ;; *) return 1 ;; esac
  [ -n "${last}" ] && [ "${available}" = "11" ]
}

metrics_text() {
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
    wget -qO- http://127.0.0.1:8080/ 2> /dev/null
}

metric_pool_equals() { # <used> <available>
  local text used_line available_line
  text="$(metrics_text)" || return 1
  used_line="$(printf '%s\n' "${text}" | grep '^kubevirtiphelper_ippool_used{' |
    grep 'ippool="e2e-pool"' | grep 'network="kubevirt-ip-helper/kubevirt-ip-helper-e2e"' || true)"
  available_line="$(printf '%s\n' "${text}" | grep '^kubevirtiphelper_ippool_available{' |
    grep 'ippool="e2e-pool"' | grep 'network="kubevirt-ip-helper/kubevirt-ip-helper-e2e"' || true)"
  case "${used_line}" in *" $1") ;; *) return 1 ;; esac
  case "${available_line}" in *" $2") ;; *) return 1 ;; esac
}

metric_vm_ok() {
  local text line
  text="$(metrics_text)" || return 1
  line="$(printf '%s\n' "${text}" | grep '^kubevirtiphelper_vmnetcfg_status{' |
    grep 'vm="e2e/e2e-vm"' | grep 'mac="02:00:00:00:00:11"' |
    grep "ip=\"${RESERVED_IP}\"" | grep 'status="OK"' || true)"
  case "${line}" in *" 1") return 0 ;; *) return 1 ;; esac
}

metric_vm_absent() {
  local text
  text="$(metrics_text)" || return 1
  ! printf '%s\n' "${text}" | grep '^kubevirtiphelper_vmnetcfg_status{' | grep -q 'vm="e2e/e2e-vm"'
}

leader_services_healthy() {
  leader_consistent || return 1
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- sh -c \
    "ip -4 addr show dev '${KIH_HELPER_INTERFACE}' | grep -q '${KIH_IPPOOL_SERVER}/24'" \
    > /dev/null 2>&1 || return 1
  # shellcheck disable=SC2016
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- awk \
    'NR > 1 && $2 ~ /:0043$/ { found=1 } END { exit !found }' /proc/net/udp \
    > /dev/null 2>&1 || return 1
  metrics_text > /dev/null
}

vm_reservation_ready() {
  local ip mac network status
  ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}' 2> /dev/null)"
  mac="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].macaddress}' 2> /dev/null)"
  network="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].networkname}' 2> /dev/null)"
  status="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.status.networkconfig[0].status}' 2> /dev/null)"
  [ -n "${ip}" ] && [ "${mac}" = "${KIH_VM_MAC}" ] &&
    [ "${network}" = "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}" ] && [ "${status}" = "OK" ]
}

pool_allocation_matches() {
  local allocations used available
  # shellcheck disable=SC2016
  allocations="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o go-template='{{range $ip, $owner := .status.ipv4.allocated}}{{$ip}}={{$owner}}{{"\n"}}{{end}}' 2> /dev/null)"
  printf '%s\n' "${allocations}" | grep -F "${RESERVED_IP}=e2e/e2e-vm [${KIH_VM_MAC}]" > /dev/null || return 1
  used="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)"
  available="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)"
  [ "${used}" = "1" ] && [ "${available}" = "10" ]
}

reservation_stable() {
  local ip
  ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}' 2> /dev/null)"
  [ "${ip}" = "${RESERVED_IP}" ] && pool_allocation_matches
}

vmi_exists() {
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmi "${KIH_VM_NAME}" > /dev/null 2>&1
}

vmi_absent() {
  ! vmi_exists
}

console_has_reserved_ip() { # <file>
  grep -q "${E2E_DHCP_MARKER}=${RESERVED_IP}" "$1"
}

start_guest_and_assert() { # <label> [absolute SECONDS deadline]
  local label="$1" deadline="${2:-}" console_log markers console_timeout_minutes console_budget start_budget ready_budget
  console_log="${E2E_ARTIFACTS_DIR}/console-${label}.log"
  : > "${console_log}"
  if [ -n "${deadline}" ]; then
    start_budget=$((deadline - SECONDS))
    [ "${start_budget}" -gt 0 ] ||
      die "deadline expired before starting guest (${label})"
    timeout --foreground "${start_budget}s" \
      "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" start "${KIH_VM_NAME}" ||
      die "guest start did not finish before the failover deadline (${label})"
    wait_before_deadline "${deadline}" 60 "VMI object created (${label})" vmi_exists
    console_budget=$((deadline - SECONDS))
    [ "${console_budget}" -gt 0 ] ||
      die "deadline expired before guest DHCP marker (${label})"
    [ "${console_budget}" -le "${E2E_VM_BOOT_TIMEOUT}" ] ||
      console_budget="${E2E_VM_BOOT_TIMEOUT}"
  else
    "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" start "${KIH_VM_NAME}"
    wait_for 60 "VMI object created (${label})" vmi_exists
    console_budget="${E2E_VM_BOOT_TIMEOUT}"
  fi
  console_timeout_minutes=$(((console_budget + 59) / 60))
  CONSOLE_FIFO="${console_log}.stdin"
  rm -f "${CONSOLE_FIFO}"
  mkfifo "${CONSOLE_FIFO}"
  tail -f /dev/null > "${CONSOLE_FIFO}" &
  CONSOLE_FEEDER_PID=$!
  timeout --foreground "${console_budget}s" \
    "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" console "${KIH_VM_NAME}" \
    --timeout="${console_timeout_minutes}" < "${CONSOLE_FIFO}" \
    > "${console_log}" 2>&1 &
  CONSOLE_PID=$!
  if [ -n "${deadline}" ]; then
    wait_before_deadline "${deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest DHCP marker (${label})" console_has_reserved_ip "${console_log}"
  else
    wait_for "${E2E_VM_BOOT_TIMEOUT}" "guest DHCP marker (${label})" \
      console_has_reserved_ip "${console_log}"
  fi
  kill "${CONSOLE_PID}" > /dev/null 2>&1 || true
  kill "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
  wait "${CONSOLE_PID}" > /dev/null 2>&1 || true
  wait "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
  rm -f "${CONSOLE_FIFO}"
  CONSOLE_PID=""
  CONSOLE_FEEDER_PID=""
  CONSOLE_FIFO=""
  if [ -n "${deadline}" ]; then
    ready_budget=$((deadline - SECONDS))
    [ "${ready_budget}" -gt 0 ] ||
      die "deadline expired before VMI Ready (${label})"
    [ "${ready_budget}" -le "${E2E_VM_BOOT_TIMEOUT}" ] ||
      ready_budget="${E2E_VM_BOOT_TIMEOUT}"
    timeout --foreground "${ready_budget}s" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" \
      wait --for=condition=Ready "vmi/${KIH_VM_NAME}" --timeout="${ready_budget}s" ||
      die "VMI did not become Ready before the deadline (${label})"
  else
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" wait --for=condition=Ready \
      "vmi/${KIH_VM_NAME}" --timeout="${E2E_VM_BOOT_TIMEOUT}s"
  fi
  markers="$(grep -o "${E2E_DHCP_MARKER}=[0-9.]*" "${console_log}" | sort -u)"
  [ "${markers}" = "${E2E_DHCP_MARKER}=${RESERVED_IP}" ] ||
    die "guest markers for ${label} disagree with reservation ${RESERVED_IP}: ${markers:-none}"
}

stop_guest() {
  "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" stop "${KIH_VM_NAME}"
  wait_for 120 "VMI stopped" vmi_absent
  wait_for 60 "reservation stable while halted" reservation_stable
}

reload_processed() {
  kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${LEADER_POD}" 2> /dev/null |
    grep -q 'IPPool configuration changes detected, updating the dhcppool'
}

reload_count_exceeds() { # <pod> <baseline>
  local count
  count="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "$1" 2> /dev/null |
    grep -c 'IPPool configuration changes detected, updating the dhcppool' || true)"
  [ "${count}" -gt "$2" ]
}

new_leader_elected() { # <old pod> <old id>
  local old_pod="$1" old_id="$2"
  leader_consistent || return 1
  [ "${LEADER_POD}" != "${old_pod}" ] && [ "${LEADER_ID}" != "${old_id}" ]
}

cleanup_complete() {
  local used available allocations
  ! kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" > /dev/null 2>&1 || return 1
  used="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)"
  available="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)"
  # shellcheck disable=SC2016
  allocations="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o go-template='{{range $ip, $owner := .status.ipv4.allocated}}{{$ip}}={{$owner}}{{"\n"}}{{end}}' 2> /dev/null)"
  case "${used}" in "" | 0) ;; *) return 1 ;; esac
  [ "${available}" = "11" ] && [ -z "${allocations}" ]
}

pool_counts_equal() { # <pool> <used> <available>
  local pool="$1" expected_used="$2" expected_available="$3" used available
  used="$(kubectl get ippool "${pool}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)" || return 1
  available="$(kubectl get ippool "${pool}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)" || return 1
  used="${used:-0}"
  available="${available:-0}"
  [ "${used}" = "${expected_used}" ] && [ "${available}" = "${expected_available}" ]
}

vmnetcfg_status_is() { # <name> <status>
  [ "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" \
    -o jsonpath='{.status.networkconfig[0].status}' 2> /dev/null)" = "$2" ]
}

duplicate_mac_refused() {
  vmnetcfg_absent_named pool-vm-duplicate || return 1
  kubectl -n "${KIH_HELPER_NAMESPACE}" logs -l "${HELPER_SELECTOR}" \
    --tail=200 2> /dev/null |
    grep -qF "belongs to e2e/pool-vm-01 instead of e2e/pool-vm-duplicate"
}

vmnetcfg_absent_named() { # <name>
  ! kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" > /dev/null 2>&1
}

render_halted_vm() { # <name> <mac> <output>
  local name="$1" mac="$2" output="$3"
  sed \
    -e "s|name: ${KIH_VM_NAME}|name: ${name}|" \
    -e "s|${KIH_VM_MAC}|${mac}|g" \
    -e "s|${KIH_GUEST_IMAGE_TEMPLATE}|${KIH_GUEST_IMAGE}|" \
    "${E2E_DIR}/manifests/vm.yaml" > "${output}"
}

pool_group_allocations_ready() {
  local i name ip all_ips=""
  for i in $(seq 1 11); do
    name="$(printf 'pool-vm-%02d' "${i}")"
    vmnetcfg_status_is "${name}" OK || return 1
    ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${name}" \
      -o jsonpath='{.spec.networkconfig[0].ipaddress}' 2> /dev/null)"
    [ -n "${ip}" ] || return 1
    all_ips="${all_ips}${ip}
"
  done
  [ "$(printf '%s' "${all_ips}" | sed '/^$/d' | wc -l)" -eq 11 ] &&
    [ "$(printf '%s' "${all_ips}" | sed '/^$/d' | sort -u | wc -l)" -eq 11 ]
}

cleanup_pool_group() {
  local i name
  for i in $(seq 1 12); do
    name="$(printf 'pool-vm-%02d' "${i}")"
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${name}" \
      --ignore-not-found --wait=true --timeout=120s > /dev/null
  done
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-duplicate \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg \
    pool-vm-reclaim pool-vm-outside --ignore-not-found --wait=true --timeout=120s > /dev/null
}

run_pool_group() {
  local deadline i name mac manifest refused_ip reclaim_ip old_vm old_mac
  deadline=$((SECONDS + 720))
  log "group pool: filling all eleven addresses"
  cleanup_pool_group
  for i in $(seq 1 11); do
    name="$(printf 'pool-vm-%02d' "${i}")"
    mac="$(printf '02:00:00:00:01:%02x' "${i}")"
    manifest="${E2E_ARTIFACTS_DIR}/11-${name}.yaml"
    render_halted_vm "${name}" "${mac}" "${manifest}"
    kubectl apply -f "${manifest}" > /dev/null
  done
  wait_before_deadline "${deadline}" 180 "eleven unique reservations fill the pool" \
    pool_group_allocations_ready
  wait_before_deadline "${deadline}" 60 "pool reports exhaustion" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  log "group pool: refusing a twelfth reservation without disturbing existing leases"
  render_halted_vm pool-vm-12 02:00:00:00:01:0c \
    "${E2E_ARTIFACTS_DIR}/12-pool-vm-refused.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/12-pool-vm-refused.yaml" > /dev/null
  wait_before_deadline "${deadline}" 90 "twelfth reservation is refused" \
    vmnetcfg_status_is pool-vm-12 ERROR
  wait_before_deadline "${deadline}" 60 "refusal leaves pool accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="pool-vm-01"
  KIH_VM_MAC="02:00:00:00:01:01"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert exhausted-pool
  "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" stop "${KIH_VM_NAME}"
  wait_before_deadline "${deadline}" 120 "exhausted-pool guest stops" vmi_absent
  wait_before_deadline "${deadline}" 60 "served reservation remains allocated" \
    vmnetcfg_status_is "${KIH_VM_NAME}" OK
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"

  log "group pool: refusing a duplicate MAC without consuming another address"
  render_halted_vm pool-vm-duplicate 02:00:00:00:01:01 \
    "${E2E_ARTIFACTS_DIR}/13-duplicate-mac.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/13-duplicate-mac.yaml" > /dev/null
  wait_before_deadline "${deadline}" 90 "duplicate MAC claim is refused before VMNetCfg creation" \
    duplicate_mac_refused
  wait_before_deadline "${deadline}" 60 "duplicate MAC leaves pool accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  reclaim_ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-11 \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-11 --wait=true --timeout=120s
  wait_before_deadline "${deadline}" 90 "deleted VM releases its reservation" \
    vmnetcfg_absent_named pool-vm-11
  wait_before_deadline "${deadline}" 60 "released address returns to capacity" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 10 1

  cat > "${E2E_ARTIFACTS_DIR}/14-reclaim-vmnetcfg.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: VirtualMachineNetworkConfig
metadata:
  name: pool-vm-reclaim
  namespace: ${KIH_WORKLOAD_NAMESPACE}
  finalizers:
    - kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup
spec:
  vmname: pool-vm-reclaim
  networkconfig:
    - macaddress: "02:00:00:00:01:ee"
      networkname: "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}"
      ipaddress: "${reclaim_ip}"
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/14-reclaim-vmnetcfg.yaml" > /dev/null
  wait_before_deadline "${deadline}" 90 "different MAC reclaims the released CR-requested address" \
    vmnetcfg_status_is pool-vm-reclaim OK
  [ "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-reclaim \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')" = "${reclaim_ip}" ] ||
    die "CR-driven reclaim did not retain ${reclaim_ip}"
  wait_before_deadline "${deadline}" 60 "reclaim fills the pool again" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  refused_ip="${KIH_IPPOOL_START%.*}.99"
  cat > "${E2E_ARTIFACTS_DIR}/15-out-of-range-vmnetcfg.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: VirtualMachineNetworkConfig
metadata:
  name: pool-vm-outside
  namespace: ${KIH_WORKLOAD_NAMESPACE}
  finalizers:
    - kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup
spec:
  vmname: pool-vm-outside
  networkconfig:
    - macaddress: "02:00:00:00:01:fe"
      networkname: "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}"
      ipaddress: "${refused_ip}"
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/15-out-of-range-vmnetcfg.yaml" > /dev/null
  wait_before_deadline "${deadline}" 90 "out-of-range explicit address is refused" \
    vmnetcfg_status_is pool-vm-outside ERROR
  wait_before_deadline "${deadline}" 60 "out-of-range refusal leaves accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0
  wait_before_deadline "${deadline}" 60 "helper remains healthy after refusals" \
    leader_services_healthy

  cleanup_pool_group
  wait_before_deadline "${deadline}" 180 "pool group cleanup returns exact capacity" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  printf 'PASS pool group: exhaustion, refusal, duplicate MAC, reclaim, and out-of-range request\n' \
    > "${E2E_ARTIFACTS_DIR}/11-pool-group.txt"
}

helper_pod_count_is() { # <count>
  local total ready
  total="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null | wc -w)"
  ready="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{range .items[*]}{range .status.conditions[?(@.type=="Ready")]}{.status}{"\n"}{end}{end}' \
    2> /dev/null | grep -c '^True$' || true)"
  [ "${total}" -eq "$1" ] && [ "${ready}" -eq "$1" ]
}

leader_link_state_is() { # <UP|DOWN>
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
    ip -o link show dev "${KIH_HELPER_INTERFACE}" 2> /dev/null |
    grep -q " state $1 "
}

run_ha_group() {
  local deadline old_leader follower pods
  deadline=$((SECONDS + 480))
  log "group ha: follower churn"
  leader_consistent || die "leader state inconsistent before HA group"
  old_leader="${LEADER_POD}"
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}')"
  follower=""
  for pod in ${pods}; do
    [ "${pod}" = "${old_leader}" ] || follower="${pod}"
  done
  [ -n "${follower}" ] || die "could not identify follower pod"
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete pod "${follower}" --wait=false
  wait_before_deadline "${deadline}" 120 "follower replacement becomes Ready" helper_pods_ready
  wait_before_deadline "${deadline}" 90 "leader remains healthy after follower churn" \
    leader_services_healthy

  log "group ha: scale to one and back to two"
  kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=1
  wait_before_deadline "${deadline}" 120 "one helper replica remains Ready" helper_pod_count_is 1
  wait_before_deadline "${deadline}" 90 "single replica serves the pool" leader_services_healthy
  kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=2
  wait_before_deadline "${deadline}" 120 "second helper replica returns Ready" helper_pods_ready
  wait_before_deadline "${deadline}" 90 "two-replica leadership converges" leader_services_healthy

  log "group ha: leader secondary interface down/up"
  leader_consistent || die "leader state inconsistent before link bounce"
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
    ip link set "${KIH_HELPER_INTERFACE}" down
  wait_before_deadline "${deadline}" 30 "leader interface reports DOWN" leader_link_state_is DOWN
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
    ip link set "${KIH_HELPER_INTERFACE}" up
  wait_before_deadline "${deadline}" 30 "leader interface reports UP" leader_link_state_is UP
  wait_before_deadline "${deadline}" 90 "service remains healthy after interface bounce" \
    leader_services_healthy

  log "group ha: simultaneous deletion of both replicas"
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete pods -l "${HELPER_SELECTOR}" --wait=false
  wait_before_deadline "${deadline}" 180 "both helper replicas are replaced" helper_pods_ready
  wait_before_deadline "${deadline}" 120 "leadership and services reconstruct after total pod loss" \
    leader_services_healthy
  printf 'PASS HA group: follower churn, scaling, interface bounce, and simultaneous pod loss\n' \
    > "${E2E_ARTIFACTS_DIR}/12-ha-group.txt"
}

console_has_live_dhcp_activity() { # <file>
  grep -Eq "E2E_DHCP_EVENT=(bound|renew):${RESERVED_IP}" "$1"
}

capture_live_dhcp_activity() { # <label> <absolute deadline>
  local label="$1" deadline="$2" console_log console_budget console_timeout_minutes
  console_log="${E2E_ARTIFACTS_DIR}/console-${label}.log"
  : > "${console_log}"
  console_budget=$((deadline - SECONDS))
  [ "${console_budget}" -gt 0 ] || die "lease deadline expired before live DHCP capture"
  [ "${console_budget}" -le 75 ] || console_budget=75
  console_timeout_minutes=$(((console_budget + 59) / 60))
  CONSOLE_FIFO="${console_log}.stdin"
  rm -f "${CONSOLE_FIFO}"
  mkfifo "${CONSOLE_FIFO}"
  tail -f /dev/null > "${CONSOLE_FIFO}" &
  CONSOLE_FEEDER_PID=$!
  timeout --foreground "${console_budget}s" \
    "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" console "${KIH_VM_NAME}" \
    --timeout="${console_timeout_minutes}" < "${CONSOLE_FIFO}" \
    > "${console_log}" 2>&1 &
  CONSOLE_PID=$!
  wait_before_deadline "${deadline}" 60 "live guest emits a subsequent DHCP client event" \
    console_has_live_dhcp_activity "${console_log}"
  kill "${CONSOLE_PID}" > /dev/null 2>&1 || true
  kill "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
  wait "${CONSOLE_PID}" > /dev/null 2>&1 || true
  wait "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
  rm -f "${CONSOLE_FIFO}"
  CONSOLE_PID=""
  CONSOLE_FEEDER_PID=""
  CONSOLE_FIFO=""
}

run_lease_group() {
  local deadline manifest lease_leader reloads_before
  deadline=$((SECONDS + 420))
  log "group lease: observing live guest DHCP activity with a short lease"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" \
    --ignore-not-found --wait=true --timeout=120s
  wait_before_deadline "${deadline}" 90 "lease group starts from an empty pool" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  leader_consistent || die "leader state inconsistent before short-lease update"
  lease_leader="${LEADER_POD}"
  reloads_before="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${lease_leader}" 2> /dev/null |
    grep -c 'IPPool configuration changes detected, updating the dhcppool' || true)"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p '{"spec":{"ipv4config":{"leasetime":30}}}'
  wait_before_deadline "${deadline}" 60 "short-lease update reaches the live DHCP pool" \
    reload_count_exceeds "${lease_leader}" "${reloads_before}"
  wait_before_deadline "${deadline}" 90 "short-lease pool remains healthy" leader_services_healthy

  manifest="${E2E_ARTIFACTS_DIR}/16-lease-vm.yaml"
  render_halted_vm "${KIH_VM_NAME}" "${KIH_VM_MAC}" "${manifest}"
  kubectl apply -f "${manifest}" > /dev/null
  wait_before_deadline "${deadline}" 120 "short-lease VM reservation" vm_reservation_ready
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert short-lease "${deadline}"
  capture_live_dhcp_activity live-dhcp "${deadline}"
  wait_before_deadline "${deadline}" 60 "live DHCP activity preserves the reservation" reservation_stable
  stop_guest
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true --timeout=120s
  wait_before_deadline "${deadline}" 120 "lease group releases its reservation" cleanup_complete
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"leasetime\":${E2E_RETAINED_LEASE_SECONDS}}}}"
  wait_before_deadline "${deadline}" 90 "normal lease configuration is restored" \
    leader_services_healthy
  printf 'PASS lease group: live guest repeated DHCP activity retained the reservation\n' \
    > "${E2E_ARTIFACTS_DIR}/13-lease-group.txt"
}

helper_pods_have_interface() { # <interface>
  local pod
  for pod in $(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null); do
    kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- ip link show "$1" \
      > /dev/null 2>&1 || return 1
  done
}

pool_initialized_named() { # <pool> <available>
  pool_counts_equal "$1" 0 "$2"
}

second_pool_services_healthy() {
  leader_consistent || return 1
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- sh -c \
    "ip -4 addr show dev 'kihnet1' | grep -q '10.78.0.2/24'" \
    > /dev/null 2>&1
}

run_multipool_group() {
  local deadline
  deadline=$((SECONDS + 600))
  log "group multipool: attaching an independent second bridge and pool"
  if kubectl get ippool e2e-pool-second > /dev/null 2>&1 ||
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg multipool-vm > /dev/null 2>&1 ||
    kubectl -n "${KIH_HELPER_NAMESPACE}" get network-attachment-definition \
      kubevirt-ip-helper-e2e-second > /dev/null 2>&1; then
    die "stale multipool resources exist; remove e2e-pool-second, multipool-vm, and kubevirt-ip-helper-e2e-second before rerunning"
  fi
  cat > "${E2E_ARTIFACTS_DIR}/17-second-nad.yaml" <<EOF
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: kubevirt-ip-helper-e2e-second
  namespace: ${KIH_HELPER_NAMESPACE}
spec:
  config: '{"cniVersion":"0.3.1","type":"bridge","bridge":"br-kih-e2e2"}'
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/17-second-nad.yaml" > /dev/null
  kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=0
  kubectl -n "${KIH_HELPER_NAMESPACE}" wait --for=delete pod \
    -l app=kubevirt-ip-helper --timeout="${E2E_WAIT_TIMEOUT}s"
  cat > "${E2E_ARTIFACTS_DIR}/18-second-pool.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: IPPool
metadata:
  name: e2e-pool-second
spec:
  ipv4config:
    serverip: 10.78.0.2
    subnet: 10.78.0.0/24
    pool:
      start: 10.78.0.100
      end: 10.78.0.102
    router: 10.78.0.1
    leasetime: 300
  networkname: ${KIH_HELPER_NAMESPACE}/kubevirt-ip-helper-e2e-second
  bindinterface: kihnet1
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/18-second-pool.yaml" > /dev/null
  kubectl -n "${KIH_HELPER_NAMESPACE}" patch deployment "${HELPER_DEPLOYMENT}" \
    --type=merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"k8s.v1.cni.cncf.io/networks":"[{\"name\":\"kubevirt-ip-helper-e2e\",\"namespace\":\"kubevirt-ip-helper\",\"interface\":\"kihnet0\"},{\"name\":\"kubevirt-ip-helper-e2e-second\",\"namespace\":\"kubevirt-ip-helper\",\"interface\":\"kihnet1\"}]"}}}}}'
  kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=2
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  wait_before_deadline "${deadline}" 180 "two helper pods return after second attachment" \
    helper_pods_ready
  wait_before_deadline "${deadline}" 120 "both helper pods contain kihnet1" \
    helper_pods_have_interface kihnet1
  wait_before_deadline "${deadline}" 120 "primary pool reconstructs after attachment rollout" \
    leader_services_healthy

  wait_before_deadline "${deadline}" 120 "second pool initializes independently" \
    pool_initialized_named e2e-pool-second 3
  wait_before_deadline "${deadline}" 120 "leader serves the second pool address" \
    second_pool_services_healthy

  cat > "${E2E_ARTIFACTS_DIR}/19-second-pool-vmnetcfg.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: VirtualMachineNetworkConfig
metadata:
  name: multipool-vm
  namespace: ${KIH_WORKLOAD_NAMESPACE}
  finalizers:
    - kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup
spec:
  vmname: multipool-vm
  networkconfig:
    - macaddress: "02:00:00:00:02:01"
      networkname: "${KIH_HELPER_NAMESPACE}/kubevirt-ip-helper-e2e-second"
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/19-second-pool-vmnetcfg.yaml" > /dev/null
  wait_before_deadline "${deadline}" 120 "second pool allocates its own reservation" \
    vmnetcfg_status_is multipool-vm OK
  wait_before_deadline "${deadline}" 60 "second pool accounting is isolated" \
    pool_counts_equal e2e-pool-second 1 2
  wait_before_deadline "${deadline}" 60 "primary pool remains empty" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg multipool-vm \
    --wait=true --timeout=120s
  wait_before_deadline "${deadline}" 120 "second pool reservation is released" \
    pool_counts_equal e2e-pool-second 0 3
  kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=0
  kubectl -n "${KIH_HELPER_NAMESPACE}" wait --for=delete pod \
    -l app=kubevirt-ip-helper --timeout="${E2E_WAIT_TIMEOUT}s"
  kubectl delete ippool e2e-pool-second --wait=true --timeout=120s
  kubectl -n "${KIH_HELPER_NAMESPACE}" patch deployment "${HELPER_DEPLOYMENT}" \
    --type=merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"k8s.v1.cni.cncf.io/networks":"[{\"name\":\"kubevirt-ip-helper-e2e\",\"namespace\":\"kubevirt-ip-helper\",\"interface\":\"kihnet0\"}]"}}}}}'
  kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=2
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  wait_before_deadline "${deadline}" 180 "primary-only helper topology returns" helper_pods_ready
  wait_before_deadline "${deadline}" 120 "primary pool remains healthy after second-pool removal" \
    leader_services_healthy
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete network-attachment-definition \
    kubevirt-ip-helper-e2e-second --ignore-not-found > /dev/null
  printf 'PASS multipool group: independent attachment, pool, allocation, and cleanup\n' \
    > "${E2E_ARTIFACTS_DIR}/14-multipool-group.txt"
}

main() {
  local rendered vm_rendered default_image old_leader old_id octet failover_deadline failover_budget retained_lease_deadline
  rm -f "${E2E_CLUSTER_STATE_FILE}"
  resolve_runtime
  log "building ${E2E_IMAGE} from ${ROOT_DIR}"
  "${RUNTIME}" build -t "${E2E_IMAGE}" "${ROOT_DIR}"
  "${E2E_DIR}/bootstrap.sh"
  [ -x "${VIRTCTL}" ] || die "bootstrap did not install ${VIRTCTL}"
  # A kept cluster can be rerun, but no reservation from the previous run may
  # leak into this one.
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" \
    --ignore-not-found --wait=true --timeout=120s
  if kubectl get crd virtualmachinenetworkconfigs.kubevirtiphelper.k8s.binbash.org > /dev/null 2>&1; then
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg "${KIH_VM_NAME}" \
      --ignore-not-found --wait=true --timeout=120s
  fi
  if kubectl get crd ippools.kubevirtiphelper.k8s.binbash.org > /dev/null 2>&1; then
    kubectl delete ippool "${KIH_IPPOOL_NAME}" \
      --ignore-not-found --wait=true --timeout=120s
  fi


  rendered="${E2E_ARTIFACTS_DIR}/helper-rendered.yaml"
  kubectl kustomize --load-restrictor=LoadRestrictionsNone "${E2E_DIR}/manifests" > "${rendered}"
  default_image='kubevirt-ip-helper:e2e'
  if [ "${E2E_IMAGE}" != "${default_image}" ]; then
    case "${E2E_IMAGE}" in *'|'* | *'&'*) die "E2E_IMAGE may not contain | or &: ${E2E_IMAGE}" ;; esac
    sed -i "s|image: ${default_image}|image: ${E2E_IMAGE}|" "${rendered}"
  fi
  grep -q "image: ${E2E_IMAGE}" "${rendered}" || die "rendered helper image is not ${E2E_IMAGE}"
  kubectl apply -f "${rendered}"
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout restart \
    "deployment/${HELPER_DEPLOYMENT}"
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  wait_for 120 "two helper pods Ready with ${KIH_HELPER_INTERFACE}" helper_pods_ready
  wait_for 120 "one labelled leader, matching Lease, and one metrics endpoint" leader_consistent

  kubectl apply -f "${E2E_DIR}/manifests/pool.yaml"
  wait_for 120 "IPPool initialized with 11 available addresses" pool_initialized
  wait_for 120 "leader owns ${KIH_IPPOOL_SERVER}/24 and UDP/67" leader_services_healthy
  wait_for 60 "initial IPPool metrics" metric_pool_equals 0 11

  # manifests/vm.yaml is the current-profile template. Render it into this
  # profile's artifact directory and substitute the profile's guest image, so the
  # dependency-era lane never starts the guest on the current Cirros image.
  vm_rendered="${E2E_ARTIFACTS_DIR}/vm-rendered.yaml"
  sed "s|${KIH_GUEST_IMAGE_TEMPLATE}|${KIH_GUEST_IMAGE}|" \
    "${E2E_DIR}/manifests/vm.yaml" > "${vm_rendered}"
  grep -qF "image: ${KIH_GUEST_IMAGE}" "${vm_rendered}" ||
    die "rendered guest image is not ${KIH_GUEST_IMAGE}"
  if [ "${KIH_GUEST_IMAGE}" != "${KIH_GUEST_IMAGE_TEMPLATE}" ] &&
    grep -qF "image: ${KIH_GUEST_IMAGE_TEMPLATE}" "${vm_rendered}"; then
    die "rendered guest manifest kept ${KIH_GUEST_IMAGE_TEMPLATE} for the ${E2E_STACK} profile"
  fi
  kubectl apply -f "${vm_rendered}"
  [ "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vm "${KIH_VM_NAME}" -o jsonpath='{.spec.runStrategy}')" = "Halted" ] ||
    die "VM was not created Halted"
  vmi_absent || die "VMI exists before the helper reserved an address"
  wait_for 120 "VMNetCfg reservation while VM is halted" vm_reservation_ready
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  octet="${RESERVED_IP##*.}"
  [ "${RESERVED_IP%.*}" = "10.77.0" ] && [ "${octet}" -ge 100 ] && [ "${octet}" -le 110 ] ||
    die "reservation ${RESERVED_IP} is outside 10.77.0.100-10.77.0.110"
  wait_for 60 "IPPool allocation matches VMNetCfg" pool_allocation_matches
  wait_for 60 "IPPool used/available metrics after reservation" metric_pool_equals 1 10
  wait_for 60 "VMNetCfg OK metric" metric_vm_ok

  start_guest_and_assert initial
  stop_guest
  start_guest_and_assert restart
  stop_guest

  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"leasetime\":${E2E_RETAINED_LEASE_SECONDS}}}}"
  wait_for 60 "reloadable IPPool update processed" reload_processed
  wait_for 90 "DHCP and metrics healthy after IPPool reload" leader_services_healthy
  wait_for 90 "reservation and metrics stable after reload" reservation_stable
  wait_for 60 "metrics stable after reload" metric_pool_equals 1 10
  # Start the lease clock before the DHCP boot. The failover proof may use less
  # than its nominal budget after stop latency, but can never pass after expiry.
  retained_lease_deadline=$((SECONDS + E2E_RETAINED_LEASE_SECONDS))
  start_guest_and_assert reload
  stop_guest

  leader_consistent || die "leader state became inconsistent before failover"
  old_leader="${LEADER_POD}"
  old_id="${LEADER_ID}"
  failover_deadline=$((SECONDS + E2E_FAILOVER_DHCP_TIMEOUT))
  [ "${failover_deadline}" -le "${retained_lease_deadline}" ] ||
    failover_deadline="${retained_lease_deadline}"
  failover_budget=$((failover_deadline - SECONDS))
  [ "${failover_budget}" -gt 0 ] ||
    die "retained lease expired before active leader deletion"
  timeout --foreground "${failover_budget}s" kubectl \
    -n "${KIH_HELPER_NAMESPACE}" delete pod "${old_leader}" --wait=false ||
    die "active leader deletion did not finish before the failover deadline"
  wait_before_deadline "${failover_deadline}" 75 "leader label and Lease transfer" \
    new_leader_elected "${old_leader}" "${old_id}"
  wait_before_deadline "${failover_deadline}" 90 \
    "new leader reconstructs server IP, UDP/67, and metrics" leader_services_healthy
  wait_before_deadline "${failover_deadline}" 90 \
    "new leader reconstructs reservation" reservation_stable
  wait_before_deadline "${failover_deadline}" 60 \
    "new leader reconstructs IPPool metrics" metric_pool_equals 1 10
  wait_before_deadline "${failover_deadline}" 60 \
    "new leader reconstructs VM metric" metric_vm_ok
  start_guest_and_assert failover "${failover_deadline}"

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true
  wait_for 120 "VM deletion releases VMNetCfg and IP allocation" cleanup_complete
  wait_for 60 "cleanup updates IPPool metrics" metric_pool_equals 0 11
  wait_for 60 "cleanup removes VM metric" metric_vm_absent

  case "${E2E_GROUP}" in
    all)
      run_pool_group
      run_lease_group
      run_ha_group
      run_multipool_group
      ;;
    core) ;;
    pool) run_pool_group ;;
    lease) run_lease_group ;;
    ha) run_ha_group ;;
    multipool) run_multipool_group ;;
  esac

  log "PASS (${E2E_GROUP}): core reservation lifecycle and selected expansion groups verified"
}

main "$@"

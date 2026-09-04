#!/usr/bin/env bash
set -Eeuo pipefail

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
# shellcheck source=test/e2e/report.sh
. "${E2E_DIR}/report.sh"
REPORT_INIT_CAN_FINALIZE=1
if [ -e "${E2E_ARTIFACTS_DIR}" ]; then
  shopt -s nullglob dotglob
  REPORT_INIT_EXISTING=("${E2E_ARTIFACTS_DIR}"/*)
  shopt -u nullglob dotglob
  [ "${#REPORT_INIT_EXISTING[@]}" -eq 0 ] || REPORT_INIT_CAN_FINALIZE=0
fi
report_init_abort() {
  local rc=$?
  trap - EXIT
  set +e
  if [ "${REPORT_INIT_CAN_FINALIZE}" -eq 1 ] &&
    [ -n "${E2E_RUN_DIR:-}" ] && [ -d "${E2E_RUN_DIR}" ]; then
    report_case_start SUITE-REPORT-INITIALIZATION \
      "report initialization produced a finalizable run directory"
    report_case_fail "report initialization failed with exit ${rc}"
    E2E_EVIDENCE_COMPLETE=0
    report_finalize "${rc}" || true
  fi
  exit "${rc}"
}
trap report_init_abort EXIT
mkdir -p "${E2E_ARTIFACTS_DIR}"
# Every execution gets its own directory under the shared artifact root, keeps
# the previous run for comparison, and publishes itself as `latest`. Both systems
# start before any cluster mutation so a failure during bootstrap still leaves a
# partial journal plus the checkpoints captured so far.
report_init
REPORT_BOOTSTRAP_STARTED=0
EVIDENCE_FAILURE=""
E2E_EVIDENCE_COMPLETE=0
if declare -F evidence_init > /dev/null 2>&1 && evidence_init; then
  EVIDENCE_ENABLED=1
else
  EVIDENCE_ENABLED=''
  printf '[e2e] ERROR: object evidence initialization failed\n' >&2
fi
report_case_start SUITE-EVIDENCE-LAYER "object evidence layer loaded"
if [ -n "${EVIDENCE_ENABLED}" ]; then
  report_case_pass "checkpoints under evidence/checkpoints"
else
  report_case_fail "test/e2e/evidence.sh is missing; object checkpoints unavailable"
  EVIDENCE_FAILURE=1
fi
report_note PROFILE "stack ${E2E_STACK}, group ${E2E_GROUP}, cluster ${E2E_CLUSTER_NAME}"
report_group core
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
E2E_PRED_SECONDS="${E2E_PRED_SECONDS:-20}"
# Lease-loss fast-fail: a storm of "NO LEASE FOUND" entries for the owner MAC
# inside the storm window flags the duplicate-cfg lease-release bug signature.
E2E_LEASE_STORM_COUNT="${E2E_LEASE_STORM_COUNT:-3}"
E2E_LEASE_STORM_WINDOW="${E2E_LEASE_STORM_WINDOW:-10}"
LEADER_POD=""
LEADER_ID=""
RESERVED_IP=""
RUNTIME=""
CONSOLE_PID=""
CONSOLE_FEEDER_PID=""
CONSOLE_FIFO=""
CLUSTER_STATE=""

log() { printf '[e2e] %s\n' "$*"; }
die() {
  printf '[e2e] ERROR: %s\n' "$*" >&2
  if declare -F report_case_is_open > /dev/null 2>&1 &&
    report_case_is_open; then
    report_case_fail "$*"
  elif [ -z "${REPORT_ABORTED:-}" ]; then
    report_case_start SUITE-ABORTED "suite aborted outside an open case"
    report_case_fail "$*"
    REPORT_ABORTED=1
  fi
  exit 1
}

# One executed assertion with a stable id. The predicate runs directly so
# predicates that record leader or reservation state keep their side effects.
assert_case() { # <case-id> <name> <predicate> [args...]
  local name="$2"
  report_case_start "$1" "${name}"
  shift 2
  if "$@"; then
    log "ok: ${name}"
    report_case_pass "${name}"
  else
    die "${name}"
  fi
}

# A deadline guard or a one-shot operation gets its own stable case. The case is
# opened before the check and closed here on success; on failure die() closes the
# still-open case, so the record carries the operation's id instead of landing on
# the generic suite record.
guard_case() { # <case-id> <name> <command> [args...]
  local name="$2"
  report_case_start "$1" "${name}"
  shift 2
  if "$@"; then
    log "ok: ${name}"
    report_case_pass "${name}"
    return 0
  fi
  die "${name}"
}

# Records the first failing command before the EXIT trap finalizes the report.
on_error() {
  local rc="$?" line="${BASH_LINENO[0]:-${LINENO}}"
  # ERR is inherited into command substitutions by `set -E`; only the parent
  # shell owns the report journal. Let the parent assignment/pipeline record the
  # failure once, rather than appending a duplicate case from a child process.
  if [ "${BASHPID:-$$}" != "$$" ]; then
    exit "${rc}"
  fi
  printf '[e2e] ERROR: %s exited %s at line %s\n' "${BASH_COMMAND}" "${rc}" "${line}" >&2
  # An already-open case keeps its identity: it closes as failed instead of
  # being overwritten by the generic record.
  if [ -z "${REPORT_CASE_ID}" ]; then
    report_case_start UNEXPECTED-COMMAND "unexpected command failure at line ${line}"
  fi
  report_case_fail "${BASH_COMMAND} exited ${rc}"
}

# One object checkpoint. A short capture degrades into a failed case instead of
# an invisible gap in the evidence.
capture_checkpoint() { # <id> <description>
  if [ -z "${EVIDENCE_ENABLED}" ]; then
    return 0
  fi
  report_case_start "EVIDENCE-${1}" "object checkpoint ${2}"
  if evidence_capture "$1" "$2"; then
    report_case_pass "captured ${2}"
  else
    report_case_fail "checkpoint ${1} incomplete for ${2}"
    EVIDENCE_FAILURE=1
  fi
}

keep_cluster() {
  case "${E2E_KEEP_CLUSTER}" in
    1 | true | yes) return 0 ;;
    *) return 1 ;;
  esac
}

finish() {
  local rc=$? collection_rc=0 report_rc=0 diagnostic_errors cleanup_rc=0
  trap - EXIT
  trap '' INT TERM HUP
  set +e
  # Any assertion-failure path that bypasses the success-path teardown in
  # start_guest_and_assert can still have the console capture running; the
  # kill -0 guards touch only still-live pids, and clearing the globals
  # keeps this a single effective teardown even if the trap sequence runs
  # once more after a signal.
  if [ -n "${CONSOLE_PID}" ] && kill -0 "${CONSOLE_PID}" > /dev/null 2>&1; then
    kill "${CONSOLE_PID}" > /dev/null 2>&1 || true
  fi
  if [ -n "${CONSOLE_FEEDER_PID}" ] && kill -0 "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1; then
    kill "${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
  fi
  if [ -n "${CONSOLE_FIFO}" ] && [ -e "${CONSOLE_FIFO}" ]; then
    rm -f "${CONSOLE_FIFO}"
  fi
  CONSOLE_PID=""
  CONSOLE_FEEDER_PID=""
  CONSOLE_FIFO=""
  if [ -s "${E2E_CLUSTER_STATE_FILE}" ]; then
    CLUSTER_STATE="$(cat "${E2E_CLUSTER_STATE_FILE}")"
  fi
  # collect.sh can regenerate kubeconfig from kind, so preserve diagnostics even
  # when bootstrap failed after creating the cluster but before exporting it.
  if [ -n "${CLUSTER_STATE}" ] && [ -x "${KIND}" ]; then
    E2E_COLLECTION_IN_PROGRESS=1 E2E_DEFER_LATEST=1 timeout --foreground "${E2E_COLLECT_TOTAL_TIMEOUT}s" \
      "${E2E_DIR}/collect.sh" ||
      collection_rc=$?
    if [ "${collection_rc}" -ne 0 ]; then
      report_case_start SUITE-DIAGNOSTICS-COLLECTION \
        "required diagnostics and evidence collection finalized"
      report_case_fail "collect.sh exited ${collection_rc}"
      EVIDENCE_FAILURE=1
    fi
  fi
  if [ -s "${E2E_ARTIFACTS_DIR}/evidence/capture-errors.txt" ]; then
    diagnostic_errors="$(wc -l < "${E2E_ARTIFACTS_DIR}/evidence/capture-errors.txt")"
    report_note CAPTURE-ERRORS \
      "${diagnostic_errors} diagnostic/evidence capture error(s); see evidence/capture-errors.txt"
  fi
  if [ "${REPORT_BOOTSTRAP_IMPORTED}" -ne 1 ]; then
    if [ "${REPORT_BOOTSTRAP_IMPORT_ATTEMPTED}" -ne 1 ] &&
      report_import_bootstrap_cases "${REPORT_BOOTSTRAP_STARTED}"; then
      :
    else
      report_case_start SUITE-BOOTSTRAP-REPORT \
        "bootstrap gate journal was imported into the suite report"
      report_case_fail "cannot import bootstrap-cases.jsonl"
      EVIDENCE_FAILURE=1
    fi
  fi
  if keep_cluster && [ -n "${CLUSTER_STATE}" ]; then
    log "kept cluster ${E2E_CLUSTER_NAME}; kubeconfig: ${KUBECONFIG}"
  elif [ "${CLUSTER_STATE}" = "owned" ]; then
    if [ -x "${KIND}" ]; then
      timeout --foreground "${E2E_COLLECT_TOTAL_TIMEOUT}s" \
        "${KIND}" delete cluster --name "${E2E_CLUSTER_NAME}" || cleanup_rc=$?
    else
      cleanup_rc=127
    fi
    if [ "${cleanup_rc}" -ne 0 ]; then
      report_case_start SUITE-CLUSTER-CLEANUP \
        "owned disposable cluster deletion completed before report finalization"
      if [ -x "${KIND}" ]; then
        report_case_fail "kind delete cluster exited ${cleanup_rc}; cluster retained for diagnosis"
      else
        report_case_fail "kind binary unavailable; owned cluster retained for diagnosis"
      fi
      rc=1
    fi
  elif [ "${CLUSTER_STATE}" = "reused" ]; then
    log "left pre-existing cluster ${E2E_CLUSTER_NAME} in place"
  fi
  # Close evidence first, then the report, so the manifest covers the collected
  # diagnostics. Missing required evidence makes an otherwise successful test
  # run fail rather than publishing an incomplete PASS.
  if [ -n "${EVIDENCE_ENABLED}" ]; then
    if ! evidence_finalize; then
      report_case_start SUITE-EVIDENCE-FINALIZATION \
        "required object evidence comparison and checksums finalized"
      report_case_fail "evidence finalization failed"
      EVIDENCE_FAILURE=1
    fi
  fi
  if [ -n "${EVIDENCE_ENABLED}" ] && [ -z "${EVIDENCE_FAILURE}" ]; then
    E2E_EVIDENCE_COMPLETE=1
  fi
  export E2E_EVIDENCE_COMPLETE
  if [ -n "${EVIDENCE_FAILURE}" ] && [ "${rc}" -eq 0 ]; then
    rc=1
  fi
  report_finalize "${rc}" || report_rc=$?
  if [ "${report_rc}" -ne 0 ]; then
    rc=1
    report_case_start SUITE-REPORT-FINALIZATION \
      "required machine-readable and human-readable reports finalized"
    report_case_fail "report finalization failed with status ${report_rc}"
    report_finalize "${rc}" || true
  fi
  exit "${rc}"
}
trap finish EXIT
trap on_error ERR

# CI cancellation should still close the current assertion and run the EXIT
# finalizers. SIGKILL remains inherently uncatchable, but TERM/INT/HUP are
# converted into ordinary failed exits with a durable report case.
on_signal() {
  local signal="$1" rc="$2"
  trap - INT TERM HUP
  if [ -z "${REPORT_SIGNAL_RECORDED:-}" ]; then
    if report_case_is_open; then
      report_case_fail "received SIG${signal}; run interrupted" || true
    fi
    report_case_start "SUITE-SIGNAL-${signal}" \
      "external signal was recorded before report finalization" || true
    report_case_fail "received SIG${signal}; run interrupted" || true
    REPORT_SIGNAL_RECORDED=1
  fi
  exit "${rc}"
}
trap 'on_signal INT 130' INT
trap 'on_signal TERM 143' TERM
trap 'on_signal HUP 129' HUP

# Runs one predicate under a hard per-attempt cap so a single wedged
# kubectl/exec cannot consume the whole loop budget: the predicate runs in a
# background subshell, a sleep-based watchdog SIGTERMs it once <seconds> are
# gone (default E2E_PRED_SECONDS), and the wrapper returns the subshell's
# status (143 when the watchdog fired, which polling treats as a plain
# failure). Subshells inherit shell functions, so function predicates work
# unchanged. A kubectl child the killed subshell strands is bounded by the
# run's existing EXIT-trap cleanup of stray processes.
run_pred_once() { # <seconds> <predicate> [args...]
  local guard="$1" rc=0
  shift
  ( "$@" ) &
  local pred_pid=$!
  ( sleep "${guard}" && kill "${pred_pid}" ) &
  local guard_pid=$!
  wait "${pred_pid}" || rc=$?
  kill "${guard_pid}" > /dev/null 2>&1 || true
  wait "${guard_pid}" > /dev/null 2>&1 || true
  return "${rc}"
}

wait_for() { # <case-id> <seconds> <description> <predicate> [args...]
  local case_id="$1" timeout="$2" description="$3" start rc=0
  shift 3
  start="${SECONDS}"
  local deadline=$((SECONDS + timeout))
  report_case_start "${case_id}" "${description}"
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    # rc is captured from run_pred_once directly: an `if <cmd>; then ... fi`
    # with no else resets $? to 0 on the false branch, which would silently
    # downgrade a predicate exit code 2 (bug signature) to a plain failure.
    run_pred_once "${E2E_PRED_SECONDS}" "$@" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
      if [ "${SECONDS}" -lt "${deadline}" ]; then
        log "ok: ${description}"
        report_case_pass "satisfied $((SECONDS - start))s after the first attempt"
        return 0
      fi
      break
    fi
    if [ "${rc}" -eq 2 ]; then
      die "bug signature detected: ${description} (owner DHCP lease destroyed by duplicate-cfg cleanup; lease release is keyed by MAC without ownership check)"
    fi
    sleep 2
  done
  # One final probe outside the watchdog at the deadline: a predicate that
  # only needs one more event can still pass, and the pass then carries the
  # grace detail instead of a timeout. The rc is captured the same way as in
  # the polling loop so an exit code 2 bug signature still reports its
  # attribution message rather than a generic timeout.
  "$@" && rc=0 || rc=$?
  if [ "${rc}" -eq 0 ]; then
    log "ok: ${description}"
    report_case_pass "satisfied via grace probe at the ${timeout}s deadline"
    return 0
  fi
  if [ "${rc}" -eq 2 ]; then
    die "bug signature detected: ${description} (owner DHCP lease destroyed by duplicate-cfg cleanup; lease release is keyed by MAC without ownership check)"
  fi
  die "timed out after ${timeout}s: ${description}"
}

wait_before_deadline() { # <case-id> <absolute SECONDS> <maximum seconds> <description> <predicate> [args...]
  local case_id="$1" deadline="$2" maximum="$3" description="$4" remaining rc=0 start window
  shift 4
  report_case_start "${case_id}" "${description}"
  remaining=$((deadline - SECONDS))
  [ "${remaining}" -gt 0 ] ||
    die "deadline expired before ${description}"
  [ "${remaining}" -le "${maximum}" ] || remaining="${maximum}"
  start="${SECONDS}"
  window=$((start + remaining))
  # The per-attempt watchdog applies here too, but never a grace probe:
  # absolute deadlines are window contracts and must not pass after expiry.
  while [ "${SECONDS}" -lt "${window}" ]; do
    run_pred_once "${E2E_PRED_SECONDS}" "$@" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
      log "ok: ${description}"
      report_case_pass "satisfied $((SECONDS - start))s after the first attempt"
      return 0
    fi
    if [ "${rc}" -eq 2 ]; then
      die "bug signature detected: ${description} (owner DHCP lease destroyed by duplicate-cfg cleanup; lease release is keyed by MAC without ownership check)"
    fi
    sleep 2
  done
  die "timed out after ${remaining}s: ${description}"
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
  command -v jq > /dev/null 2>&1 || die "jq is required for reports and evidence"
}

helper_pods_ready() {
  local pods pod ready status deletion
  local -a pod_names
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || return 1
  read -r -a pod_names <<< "${pods}"
  [ "${#pod_names[@]}" -eq 2 ] || return 1
  for pod in "${pod_names[@]}"; do
    deletion="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2> /dev/null)" || return 1
    [ -z "${deletion}" ] || return 1
    ready="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2> /dev/null)" || return 1
    [ "${ready}" = "True" ] || return 1
    status="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}' 2> /dev/null)" || return 1
    case "${status}" in
      *"${KIH_HELPER_INTERFACE}"*) ;;
      *) return 1 ;;
    esac
    kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
      ip link show "${KIH_HELPER_INTERFACE}" > /dev/null 2>&1 || return 1
  done
}
helper_pod_uids() {
  kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2> /dev/null
}

# A replacement predicate rejects terminating old pods and requires every UID
# from the pre-transition snapshot to be absent before a new Ready set counts.
helper_pods_replaced_since() { # <old-uid-list>
  local old_uids="$1" current uid
  [ -n "${old_uids}" ] || return 1
  helper_pods_ready || return 1
  current="$(helper_pod_uids)" || return 1
  for uid in ${old_uids}; do
    printf '%s\n' "${current}" | grep -qxF "${uid}" && return 1
  done
  return 0
}

helper_pod_runtime_snapshot() {
  local pods pod uid restarts
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || return 1
  [ -n "${pods}" ] || return 1
  for pod in ${pods}; do
    uid="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.metadata.uid}' 2> /dev/null)" || return 1
    restarts="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{range .status.containerStatuses[*]}{.restartCount}{" "}{end}' \
      2> /dev/null)" || return 1
    printf '%s\t%s\t%s\n' "${pod}" "${uid}" "${restarts}"
  done
}

# Pool deletion is in-place configuration cleanup, not a helper restart. Keep
# pod UIDs and aggregate container restart counts unchanged across that action.
helper_pods_unchanged_since() { # <pod<TAB>uid<TAB>restart snapshot>
  local snapshot="$1" pod uid restarts current_uid current_restarts
  [ -n "${snapshot}" ] || return 1
  helper_pods_ready || return 1
  while IFS=$'\t' read -r pod uid restarts; do
    [ -n "${pod}" ] || continue
    current_uid="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.metadata.uid}' 2> /dev/null)" || return 1
    [ "${current_uid}" = "${uid}" ] || return 1
    current_restarts="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{range .status.containerStatuses[*]}{.restartCount}{" "}{end}' \
      2> /dev/null)" || return 1
    [ "${current_restarts}" = "${restarts}" ] || return 1
  done <<< "${snapshot}"
  return 0
}

# The pod that currently carries the leader label. Metric predicates
# resolve this fresh on every call so they follow a transition, while the
# global LEADER_POD keeps serving failover bookkeeping (new_leader_elected,
# start_guest_and_assert argument capture) unchanged.
current_leader_pod() {
  local pods pod
  local -a pod_names
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${LEADER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || return 1
  read -r -a pod_names <<< "${pods}"
  [ "${#pod_names[@]}" -eq 1 ] || return 1
  printf '%s\n' "${pod_names[0]}"
}

leader_consistent() {
  local pods holder generated endpoint_ips pod_ip
  local -a endpoint_addresses
  pods="$(current_leader_pod)" || return 1
  LEADER_POD="${pods}"
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
  local last used available capacity
  last="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.lastupdate}' 2> /dev/null)"
  used="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)"
  available="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)"
  # used is strictly required and must be exactly zero; missing or empty
  # fails. available is normalized: absent means an exhausted pool (0) and
  # must then equal the capacity derived from the spec pool range.
  case "${used}" in '' | *[!0-9]*) return 1 ;; esac
  [ "${used}" -eq 0 ] || return 1
  [ -n "${available}" ] || available=0
  case "${available}" in '' | *[!0-9]*) return 1 ;; esac
  capacity="$(capacity_from_spec "${KIH_IPPOOL_NAME}")" || return 1
  [ -n "${last}" ] && [ "${available}" -eq "${capacity}" ]
}

# Metrics are scraped from whichever pod currently holds the leader label,
# resolved per call (never through the cached LEADER_POD). The -T 5
# watchdog bounds the in-pod wget so one exec cannot hang the caller.
metrics_text() {
  local pod
  pod="$(current_leader_pod)" || return 1
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
    wget -T 5 -qO- http://127.0.0.1:8080/ 2> /dev/null
}

# Extracts the value of exactly one exposition series: matching lines must
# start with the family name and carry every required label substring,
# exactly one line may match (duplicate series lines are a helper-side
# leak and fail the extraction), and the value must be the sole trailing
# token after the closing brace and strictly numeric.
metric_value_for() { # <family> <label-substring> [label-substring...]
  local text matches count value sub
  text="$(metrics_text)" || return 1
  matches="$(printf '%s\n' "${text}" | grep "^${1}{" || true)"
  shift
  for sub in "$@"; do
    matches="$(printf '%s\n' "${matches}" | grep -F "${sub}" || true)"
  done
  count="$(printf '%s\n' "${matches}" | sed '/^$/d' | wc -l)"
  [ "${count}" -eq 1 ] || return 1
  value="${matches##*\}}"
  case "${value}" in
    ' '*)
      value="${value# }"
      case "${value}" in
        '' | *[!0-9]*) return 1 ;;
      esac
      printf '%s\n' "${value}"
      return 0
      ;;
  esac
  return 1
}

metric_pool_equals() { # <used> <available>
  local used available
  used="$(metric_value_for kubevirtiphelper_ippool_used 'ippool="e2e-pool"' \
    'network="kubevirt-ip-helper/kubevirt-ip-helper-e2e"')" || return 1
  available="$(metric_value_for kubevirtiphelper_ippool_available 'ippool="e2e-pool"' \
    'network="kubevirt-ip-helper/kubevirt-ip-helper-e2e"')" || return 1
  [ "${used}" = "$1" ] && [ "${available}" = "$2" ]
}

metric_vm_ok() {
  local value
  value="$(metric_value_for kubevirtiphelper_vmnetcfg_status \
    'vm="e2e/e2e-vm"' 'mac="02:00:00:00:00:11"' \
    "ip=\"${RESERVED_IP}\"" 'status="OK"')" || return 1
  # The helper always exposes the vmnetcfg-status series with value 1.
  [ "${value}" = "1" ]
}

metric_vm_absent() {
  local text
  text="$(metrics_text)" || return 1
  ! printf '%s\n' "${text}" | grep '^kubevirtiphelper_vmnetcfg_status{' | grep -q 'vm="e2e/e2e-vm"'
}

metric_ippool_absent() { # <pool>
  local text
  text="$(metrics_text)" || return 1
  # No used or available series line may carry the pool label.
  ! printf '%s\n' "${text}" | grep '^kubevirtiphelper_ippool_' | grep -qF "ippool=\"$1\""
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

vmnetcfg_pool_name() {
  local network
  network="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].networkname}' 2> /dev/null)" || return 1
  [ -n "${network}" ] || return 1
  # shellcheck disable=SC2016
  kubectl get ippool -o go-template='{{range .items}}{{if eq .spec.networkname "'"${network}"'"}}{{.metadata.name}}{{end}}{{end}}' \
    2> /dev/null
}

# Inclusive capacity of the configured pool range, derived from the spec
# pool start/end in ip_number arithmetic (never hard-coded): the same
# accounting the helper itself uses.
capacity_from_spec() { # <pool>
  local pool="$1" capacity
  capacity="$(
    kubectl get ippool "${pool}" -o json |
      jq -e -r '
        def ip_number:
          split(".") | map(tonumber) |
          .[0] * 16777216 + .[1] * 65536 + .[2] * 256 + .[3];
        (.spec.ipv4config.pool.start | ip_number) as $start |
        (.spec.ipv4config.pool.end | ip_number) as $end |
        if $end >= $start then ($end - $start + 1)
        else error("invalid IPPool range")
        end
      ' 2> /dev/null
  )" || return 1
  case "${capacity}" in '' | *[!0-9]*) return 1 ;; esac
  printf '%s\n' "${capacity}"
}

pool_allocation_matches() {
  local allocations pool used available allocated_count capacity
  pool="$(vmnetcfg_pool_name)" || return 1
  [ -n "${pool}" ] || return 1
  # shellcheck disable=SC2016
  allocations="$(kubectl get ippool "${pool}" -o go-template='{{range $ip, $owner := .status.ipv4.allocated}}{{$ip}}={{$owner}}{{"\n"}}{{end}}' 2> /dev/null)" || return 1
  printf '%s\n' "${allocations}" |
    grep -F "${RESERVED_IP}=${KIH_WORKLOAD_NAMESPACE}/${KIH_VM_NAME} [${KIH_VM_MAC}]" \
    > /dev/null || return 1
  used="$(kubectl get ippool "${pool}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)" || return 1
  available="$(kubectl get ippool "${pool}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)" || return 1
  # The helper omits status.ipv4.available when the range is exhausted; that
  # representation is equivalent to zero free addresses for this invariant.
  [ -n "${available}" ] || available=0
  case "${used}" in '' | *[!0-9]*) return 1 ;; esac
  case "${available}" in '' | *[!0-9]*) return 1 ;; esac
  allocated_count="$(printf '%s\n' "${allocations}" | sed '/^$/d' | wc -l)"
  capacity="$(capacity_from_spec "${pool}")" || return 1
  [ "${used}" -eq "${allocated_count}" ] &&
    [ "${available}" -eq "$((capacity - used))" ]
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

object_absent_not_found() { # <kubectl arguments...>
  local message
  if message="$(kubectl "$@" 2>&1 > /dev/null)"; then
    return 1
  fi
  case "${message}" in
    *"(NotFound)"* | *" not found"*) return 0 ;;
    *) return 1 ;;
  esac
}

vmi_absent() {
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" get vmi "${KIH_VM_NAME}"
}

console_has_reserved_ip() { # <file> [ip]
  local ip="${2:-${RESERVED_IP}}"
  grep -q "${E2E_DHCP_MARKER}=${ip}" "$1"
}
console_has_dhcp_event() { # <file> <address>
  grep -qF "E2E_DHCP_EVENT=bound:${2}" "$1" ||
    grep -qF "E2E_DHCP_EVENT=renew:${2}" "$1"
}

# The udhcpc deconfig callback reports an empty or `unset` router before a
# successful bound/renew event. Ignore only those pre-bind markers; any
# actual option printed by the guest must agree with the expected pool
# router, and the last non-unset marker must carry the same router: after
# a pool switch the guest configures itself from that final option.
console_has_router_marker() { # <file> <router>
  local routers last
  routers="$(grep -oE 'E2E_DHCP_ROUTER=[^[:space:]]+' "$1" |
    grep -vE '=unset$|=$' | sort -u || true)"
  last="$(grep -oE 'E2E_DHCP_ROUTER=[^[:space:]]+' "$1" |
    grep -vE '=unset$|=$' | tail -n 1 || true)"
  [ "${routers}" = "E2E_DHCP_ROUTER=$2" ] &&
    [ "${last}" = "E2E_DHCP_ROUTER=$2" ]
}

# The duplicate-cfg cleanup released the DHCP lease keyed by MAC without an
# ownership check (pkg/controller/vmnetcfg/vmnetcfg.go:262,
# pkg/dhcp/dhcp.go:169-183), so the live owner's lease can be destroyed
# with the duplicate. Once that happened, every request from the owner
# MAC hits "NO LEASE FOUND" in the leader log: a storm of those entries is
# the lease-loss signature.
leader_log_storm_for() { # <mac> <since-minutes>
  local mac="$1" since="$2" pod count
  pod="$(current_leader_pod)" || return 1
  count="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}" --since="${since}m" 2> /dev/null |
    grep -cF "NO LEASE FOUND: hwaddr=${mac}" || true)"
  [ "${count}" -ge "${E2E_LEASE_STORM_COUNT}" ]
}

# Boot predicate for the boot that follows the duplicate config cleanup:
# the awaited marker passes normally; otherwise a lease-loss storm for the
# owner MAC is the production-bug signature and must fail fast (exit code
# 2). The marker is checked before the log scan for clarity, though once
# the lease is gone it can no longer appear.
boot_marker_or_lease_loss() { # <log-file> <owner-mac> <expected-ip>
  local log_file="$1" owner_mac="$2" expected_ip="$3"
  if console_has_reserved_ip "${log_file}" "${expected_ip}"; then
    return 0
  fi
  if leader_log_storm_for "${owner_mac}" "${E2E_LEASE_STORM_WINDOW}"; then
    return 2
  fi
  return 1
}

# The router option belongs to the pool that owns the guest's own network: the
# primary bridge answers on 10.77.0.x and the second bridge answers on
# 10.78.0.x, so the expectation follows the VMNetCfg network rather than the
# primary pool name.
expected_router_for_guest() {
  local pool
  pool="$(vmnetcfg_pool_name)" || return 1
  [ -n "${pool}" ] || return 1
  kubectl get ippool "${pool}" \
    -o jsonpath='{.spec.ipv4config.router}' 2> /dev/null
}

start_guest_and_assert() { # <label> [absolute SECONDS deadline]
  local label="$1" deadline="${2:-}" console_log markers expected_router
  local console_timeout_minutes console_budget start_budget ready_budget
  console_log="${E2E_ARTIFACTS_DIR}/console-${label}.log"
  : > "${console_log}"
  if [ -n "${deadline}" ]; then
    start_budget=$((deadline - SECONDS))
    guard_case "BOOT-${label}-START-WINDOW" \
      "guest start window remains before the failover deadline (${label})" \
      test "${start_budget}" -gt 0
    guard_case "BOOT-${label}-START" \
      "virtctl start finished before the failover deadline (${label})" \
      timeout --foreground "${start_budget}s" \
      "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" start "${KIH_VM_NAME}"
    wait_before_deadline "BOOT-${label}-VMI" "${deadline}" 60 "VMI object created (${label})" vmi_exists
    console_budget=$((deadline - SECONDS))
    guard_case "BOOT-${label}-DHCP-WINDOW" \
      "console window remains for the DHCP marker (${label})" \
      test "${console_budget}" -gt 0
    [ "${console_budget}" -le "${E2E_VM_BOOT_TIMEOUT}" ] ||
      console_budget="${E2E_VM_BOOT_TIMEOUT}"
  else
    guard_case "BOOT-${label}-START" "virtctl start finished (${label})" \
      "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" start "${KIH_VM_NAME}"
    wait_for "BOOT-${label}-VMI" 60 "VMI object created (${label})" vmi_exists
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
    wait_before_deadline "BOOT-${label}-DHCP" "${deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest DHCP marker (${label})" console_has_reserved_ip "${console_log}"
  elif [ "${label}" = "duplicate-owner-after-cfg" ]; then
    wait_for "BOOT-${label}-DHCP" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest DHCP marker (${label}) (fails fast on the lease-loss signature)" \
      boot_marker_or_lease_loss "${console_log}" "${KIH_VM_MAC}" "${RESERVED_IP}"
  else
    wait_for "BOOT-${label}-DHCP" "${E2E_VM_BOOT_TIMEOUT}" "guest DHCP marker (${label})" \
      console_has_reserved_ip "${console_log}"
  fi
  # Both DHCP options are read while the console still streams, so a later
  # renewal stays visible instead of being cut off with the console.
  expected_router="$(expected_router_for_guest || true)"
  guard_case "BOOT-${label}-ROUTER-POOL" \
    "IPPool serving the ${label} guest network declares a router option" \
    test -n "${expected_router}"
  if [ -n "${deadline}" ]; then
    wait_before_deadline "BOOT-${label}-ROUTER" "${deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest DHCP router option for ${label} matches ${expected_router}" \
      console_has_router_marker "${console_log}" "${expected_router}"
  else
    wait_for "BOOT-${label}-ROUTER" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest DHCP router option for ${label} matches ${expected_router}" \
      console_has_router_marker "${console_log}" "${expected_router}"
  fi
  if [ -n "${deadline}" ]; then
    wait_before_deadline "BOOT-${label}-DHCP-EVENT" "${deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest bound/renew event names ${RESERVED_IP} (${label})" \
      console_has_dhcp_event "${console_log}" "${RESERVED_IP}"
  else
    wait_for "BOOT-${label}-DHCP-EVENT" "${E2E_VM_BOOT_TIMEOUT}" \
      "guest bound/renew event names ${RESERVED_IP} (${label})" \
      console_has_dhcp_event "${console_log}" "${RESERVED_IP}"
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
    guard_case "BOOT-${label}-READY-WINDOW" \
      "Ready window remains before the failover deadline (${label})" \
      test "${ready_budget}" -gt 0
    [ "${ready_budget}" -le "${E2E_VM_BOOT_TIMEOUT}" ] ||
      ready_budget="${E2E_VM_BOOT_TIMEOUT}"
    assert_case "BOOT-${label}-READY" "VMI Ready before the failover deadline (${label})" \
      timeout --foreground "${ready_budget}s" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" \
        wait --for=condition=Ready "vmi/${KIH_VM_NAME}" --timeout="${ready_budget}s"
  else
    assert_case "BOOT-${label}-READY" "VMI Ready after boot (${label})" \
      kubectl -n "${KIH_WORKLOAD_NAMESPACE}" wait --for=condition=Ready \
        "vmi/${KIH_VM_NAME}" --timeout="${E2E_VM_BOOT_TIMEOUT}s"
  fi
  markers="$(grep -o "${E2E_DHCP_MARKER}=[0-9.]*" "${console_log}" | sort -u)"
  report_case_start "BOOT-${label}-MARKERS" \
    "guest console markers for ${label} match reservation ${RESERVED_IP}"
  if [ "${markers}" = "${E2E_DHCP_MARKER}=${RESERVED_IP}" ]; then
    report_case_pass "${markers}"
  else
    die "guest markers for ${label} disagree with reservation ${RESERVED_IP}: ${markers:-none}"
  fi
}

stop_guest() { # <label> [reservation-predicate [predicate-args...]]
  local label="${1:-guest}" predicate=reservation_stable
  if [ "$#" -ge 2 ]; then
    predicate="$2"
    shift 2
  else
    shift
  fi
  guard_case "STOP-${label}-SUBMITTED" "virtctl stop accepted (${label})" \
    "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" stop "${KIH_VM_NAME}"
  wait_for "STOP-${label}-VM-GONE" 120 "VMI stopped" vmi_absent
  wait_for "STOP-${label}-RESERVATION-STABLE" 60 "reservation stable while halted" \
    "${predicate}" "$@"
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

# Restart-class changes log a distinct message, so a pool update that only
# reloads the DHCP pool cannot satisfy this predicate.
reinit_count_exceeds() { # <baseline>
  local count
  count="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs -l "${HELPER_SELECTOR}" --tail=200 2> /dev/null |
    grep -c 'starting application reinitialization' || true)"
  [ "${count}" -gt "$1" ]
}

new_leader_elected() { # <old pod> <old id>
  local old_pod="$1" old_id="$2"
  leader_consistent || return 1
  [ "${LEADER_POD}" != "${old_pod}" ] && [ "${LEADER_ID}" != "${old_id}" ]
}

cleanup_complete() {
  local used available allocations capacity
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" \
    get vmnetcfg "${KIH_VM_NAME}" || return 1
  used="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)"
  available="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)"
  # shellcheck disable=SC2016
  allocations="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o go-template='{{range $ip, $owner := .status.ipv4.allocated}}{{$ip}}={{$owner}}{{"\n"}}{{end}}' 2> /dev/null)"
  # used is strictly required and must be exactly zero; missing or empty
  # fails. available is normalized: absent means an exhausted pool (0),
  # and the expected free count is the spec-derived capacity.
  case "${used}" in '' | *[!0-9]*) return 1 ;; esac
  [ "${used}" -eq 0 ] || return 1
  [ -n "${available}" ] || available=0
  case "${available}" in '' | *[!0-9]*) return 1 ;; esac
  capacity="$(capacity_from_spec "${KIH_IPPOOL_NAME}")" || return 1
  [ "${available}" -eq "${capacity}" ] && [ -z "${allocations}" ]
}

pool_counts_equal() { # <pool> <used> <available>
  local pool="$1" expected_used="$2" expected_available="$3" used available
  used="$(kubectl get ippool "${pool}" -o jsonpath='{.status.ipv4.used}' 2> /dev/null)" || return 1
  available="$(kubectl get ippool "${pool}" -o jsonpath='{.status.ipv4.available}' 2> /dev/null)" || return 1
  # used is strictly numeric; available is normalized: absent means an
  # exhausted pool (0).
  case "${used}" in '' | *[!0-9]*) return 1 ;; esac
  [ -n "${available}" ] || available=0
  case "${available}" in '' | *[!0-9]*) return 1 ;; esac
  [ "${used}" = "${expected_used}" ] && [ "${available}" = "${expected_available}" ]
}

vmnetcfg_status_is() { # <name> <status>
  [ "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" \
    -o jsonpath='{.status.networkconfig[0].status}' 2> /dev/null)" = "$2" ]
}

duplicate_mac_refused() {
  local message
  vmnetcfg_absent_named pool-vm-duplicate || return 1
  kubectl -n "${KIH_HELPER_NAMESPACE}" logs -l "${HELPER_SELECTOR}" \
    --tail=200 2> /dev/null |
    grep -qF "belongs to e2e/pool-vm-01 instead of e2e/pool-vm-duplicate" || return 1
  vmnetcfg_status_is pool-vm-duplicate-cfg ERROR || return 1
  message="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-duplicate-cfg \
    -o jsonpath='{.status.networkconfig[0].message}' 2> /dev/null)"
  [ "${message}" = "macaddress belongs to another vm" ]
}

# Named counterpart of pool_allocation_matches: a refused duplicate claim must
# leave the original owner's address and accounting entry untouched.
named_reservation_kept() { # <name> <mac>
  local ip allocations
  ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}' 2> /dev/null)" || return 1
  [ -n "${ip}" ] || return 1
  vmnetcfg_status_is "$1" OK || return 1
  # shellcheck disable=SC2016
  allocations="$(kubectl get ippool "${KIH_IPPOOL_NAME}" -o go-template='{{range $ip, $owner := .status.ipv4.allocated}}{{$ip}}={{$owner}}{{"\n"}}{{end}}' 2> /dev/null)"
  printf '%s\n' "${allocations}" |
    grep -qF "${ip}=${KIH_WORKLOAD_NAMESPACE}/${1} [${2}]"
}

vmnetcfg_absent_named() { # <name>
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1"
}
vm_absent_named() { # <name>
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" get vm "$1"
}
render_halted_vm() { # <name> <mac> <output>
  local name="$1" mac="$2" output="$3"
  sed \
    -e "s|name: ${KIH_VM_NAME}|name: ${name}|" \
    -e "s|${KIH_VM_MAC}|${mac}|g" \
    -e "s|${KIH_GUEST_IMAGE_TEMPLATE}|${KIH_GUEST_IMAGE}|" \
    "${E2E_DIR}/manifests/vm.yaml" > "${output}"
}

# The second bridge is attached under its own interface and NAD, and its pool
# answers a different subnet, so the guest template needs those three swaps on
# top of the name, MAC, and image substitutions.
render_second_nad_vm() { # <name> <mac> <output>
  local name="$1" mac="$2" output="$3"
  sed \
    -e "s|name: ${KIH_VM_NAME}|name: ${name}|" \
    -e "s|${KIH_VM_MAC}|${mac}|g" \
    -e "s|networkName: ${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}|networkName: ${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}-second|" \
    -e "s|${KIH_HELPER_INTERFACE}|kihnet1|g" \
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
    pool-vm-reclaim pool-vm-outside pool-vm-duplicate-cfg \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
}
cleanup_stale_expanded_resources() {
  cleanup_pool_group
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm \
    multipool-vm multipool-guest --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg \
    multipool-vm multipool-guest --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl delete ippool e2e-pool-second \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete network-attachment-definition \
    "${KIH_NAD_NAME}-second" --ignore-not-found --wait=true --timeout=120s > /dev/null
}


run_pool_group() {
  report_group pool
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
  wait_before_deadline POOL-FILL-UNIQUE "${deadline}" 180 "eleven unique reservations fill the pool" \
    pool_group_allocations_ready
  wait_before_deadline POOL-EXHAUSTION "${deadline}" 60 "pool reports exhaustion" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0
  capture_checkpoint 11-pool-filled "eleven reservations fill ${KIH_IPPOOL_NAME}"

  log "group pool: refusing a twelfth reservation without disturbing existing leases"
  render_halted_vm pool-vm-12 02:00:00:00:01:0c \
    "${E2E_ARTIFACTS_DIR}/12-pool-vm-refused.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/12-pool-vm-refused.yaml" > /dev/null
  wait_before_deadline POOL-TWELFTH-REFUSED "${deadline}" 90 "twelfth reservation is refused" \
    vmnetcfg_status_is pool-vm-12 ERROR
  wait_before_deadline POOL-REFUSAL-ACCOUNTING "${deadline}" 60 "refusal leaves pool accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="pool-vm-01"
  KIH_VM_MAC="02:00:00:00:01:01"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert exhausted-pool
  stop_guest exhausted-pool named_reservation_kept "${KIH_VM_NAME}" "${KIH_VM_MAC}"
  wait_before_deadline POOL-RESERVATION-RETAINED "${deadline}" 60 "served reservation remains allocated" \
    vmnetcfg_status_is "${KIH_VM_NAME}" OK
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"

  log "group pool: refusing a duplicate MAC without consuming another address"
  capture_checkpoint 21-duplicate-owner-before "pool filled with pool-vm-01 holding its original address"
  render_halted_vm pool-vm-duplicate 02:00:00:00:01:01 \
    "${E2E_ARTIFACTS_DIR}/13-duplicate-mac.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/13-duplicate-mac.yaml" > /dev/null
  cat > "${E2E_ARTIFACTS_DIR}/16-duplicate-owner-cfg.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: VirtualMachineNetworkConfig
metadata:
  name: pool-vm-duplicate-cfg
  namespace: ${KIH_WORKLOAD_NAMESPACE}
  finalizers:
    - kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup
spec:
  vmname: pool-vm-duplicate-cfg
  networkconfig:
    - macaddress: "02:00:00:00:01:01"
      networkname: "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}"
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/16-duplicate-owner-cfg.yaml" > /dev/null
  wait_before_deadline POOL-DUPLICATE-REFUSED "${deadline}" 90 \
    "VM precreation guard and VMNetCfg controller both refuse the duplicate MAC" \
    duplicate_mac_refused
  wait_before_deadline POOL-DUPLICATE-ACCOUNTING "${deadline}" 60 "duplicate MAC leaves pool accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0
  assert_case POOL-DUPLICATE-ORIGINAL-RESERVATION \
    "pool-vm-01 keeps its address and accounting entry" \
    named_reservation_kept pool-vm-01 02:00:00:00:01:01
  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="pool-vm-01"
  KIH_VM_MAC="02:00:00:00:01:01"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert duplicate-owner
  stop_guest duplicate-owner
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg pool-vm-duplicate-cfg \
    --wait=true --timeout=120s
  wait_before_deadline POOL-DUPLICATE-CFG-CLEANED "${deadline}" 90 \
    "refused duplicate config is removed" vmnetcfg_absent_named pool-vm-duplicate-cfg
  wait_before_deadline POOL-DUPLICATE-CFG-ACCOUNTING "${deadline}" 60 \
    "accounting still shows the eleven original reservations" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0
  # Deleting a refused duplicate must not remove the live owner's DHCP lease.
  # Reboot the original VM after the duplicate config is gone so cleanup
  # cannot silently make the reservation unreachable.
  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="pool-vm-01"
  KIH_VM_MAC="02:00:00:00:01:01"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert duplicate-owner-after-cfg
  stop_guest duplicate-owner-after-cfg
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-duplicate \
    --wait=true --timeout=120s
  wait_before_deadline POOL-DUPLICATE-VM-CLEANED "${deadline}" 90 \
    "refused duplicate VM is removed" vm_absent_named pool-vm-duplicate
  capture_checkpoint 22-duplicate-owner-after \
    "refused duplicate config gone with pool-vm-01 still holding its address"

  reclaim_ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-11 \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-11 --wait=true --timeout=120s
  wait_before_deadline POOL-DELETE-RELEASES "${deadline}" 90 "deleted VM releases its reservation" \
    vmnetcfg_absent_named pool-vm-11
  wait_before_deadline POOL-CAPACITY-RESTORED "${deadline}" 60 "released address returns to capacity" \
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
  wait_before_deadline POOL-CR-RECLAIM "${deadline}" 90 "different MAC reclaims the released CR-requested address" \
    vmnetcfg_status_is pool-vm-reclaim OK
  assert_case POOL-CR-RECLAIM-ADDRESS "CR-driven reclaim retained ${reclaim_ip}" \
    test "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-reclaim \
      -o jsonpath='{.spec.networkconfig[0].ipaddress}')" = "${reclaim_ip}"
  wait_before_deadline POOL-RECLAIM-COUNTS "${deadline}" 60 "reclaim fills the pool again" \
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
  wait_before_deadline POOL-OUT-OF-RANGE-REFUSED "${deadline}" 90 \
    "out-of-range explicit address is refused" vmnetcfg_status_is pool-vm-outside ERROR
  wait_before_deadline POOL-OUT-OF-RANGE-ACCOUNTING "${deadline}" 60 \
    "out-of-range refusal leaves accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0
  wait_before_deadline POOL-HEALTH-AFTER-REFUSALS "${deadline}" 60 \
    "helper remains healthy after refusals" leader_services_healthy
  capture_checkpoint 28-pool-refusals-held \
    "reclaimed address held and out-of-range request refused with accounting at 11 used 0 available"
  cleanup_pool_group
  wait_before_deadline POOL-CLEANUP-CAPACITY "${deadline}" 180 \
    "pool group cleanup returns exact capacity" pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  capture_checkpoint 12-pool-cleaned "pool group cleanup restored exact capacity"
  printf 'PASS pool group: exhaustion, refusal, duplicate MAC, reclaim, and out-of-range request\n' \
    > "${E2E_ARTIFACTS_DIR}/11-pool-group.txt"
}



helper_pod_count_is() { # <count>
  local pods pod deletion ready total=0 ready_count=0
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || return 1
  for pod in ${pods}; do
    deletion="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2> /dev/null)" || return 1
    [ -z "${deletion}" ] || continue
    total=$((total + 1))
    ready="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${pod}" \
      -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2> /dev/null)" || return 1
    [ "${ready}" = "True" ] && ready_count=$((ready_count + 1))
  done
  [ "${total}" -eq "$1" ] && [ "${ready_count}" -eq "$1" ]
}

leader_link_state_is() { # <UP|DOWN>
  kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
    ip -o link show dev "${KIH_HELPER_INTERFACE}" 2> /dev/null |
    grep -q " state $1 "
}

run_ha_group() {
  report_group ha
  local deadline old_leader follower follower_uid pods old_uids
  deadline=$((SECONDS + 780))
  log "group ha: follower churn"
  assert_case HA-LEADER-BEFORE-CHURN "leader state consistent before HA group" leader_consistent
  # Every later boundary has to keep a live reservation, its accounting entry,
  # and both metrics intact, so the guest is created before the first transition.
  render_halted_vm "${KIH_VM_NAME}" "${KIH_VM_MAC}" \
    "${E2E_ARTIFACTS_DIR}/20-ha-vm.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/20-ha-vm.yaml" > /dev/null
  wait_before_deadline HA-RESERVATION-CREATED "${deadline}" 120 \
    "halted VM reserves an address before helper topology churn" vm_reservation_ready
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  assert_case HA-RESERVATION-ALLOCATED "reservation matches the IPPool accounting" pool_allocation_matches
  assert_case HA-RESERVATION-METRICS "used and available metrics cover the reservation" \
    metric_pool_equals 1 10
  assert_case HA-VM-METRIC-OK "VMNetCfg metric reports OK" metric_vm_ok
  capture_checkpoint 23-ha-reservation-held \
    "${KIH_VM_NAME} holds ${RESERVED_IP} before helper topology churn"
  wait_before_deadline HA-TWO-REPLICAS-BEFORE-CHURN "${deadline}" 120 \
    "two non-terminating helper replicas are Ready before follower churn" helper_pods_ready
  old_leader="${LEADER_POD}"
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}')"
  follower=""
  for pod in ${pods}; do
    [ "${pod}" = "${old_leader}" ] || follower="${pod}"
  done
  assert_case HA-FOLLOWER-IDENTIFIED "follower pod identified besides ${old_leader}" \
    test -n "${follower}"
  follower_uid="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${follower}" \
    -o jsonpath='{.metadata.uid}')"
  guard_case HA-FOLLOWER-SNAPSHOT "follower UID captured before deletion" \
    test -n "${follower_uid}"
  guard_case HA-FOLLOWER-DELETE "follower accepts asynchronous deletion" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" delete pod "${follower}" --wait=false
  wait_before_deadline HA-FOLLOWER-REPLACED "${deadline}" 120 \
    "follower UID disappears and a different non-terminating Ready pod replaces it" \
    helper_pods_replaced_since "${follower_uid}"

  log "group ha: scale to one and back to two"
  guard_case HA-SCALE-DOWN-SUBMITTED "helper deployment scales down to one replica" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=1
  wait_before_deadline HA-SCALE-DOWN-READY "${deadline}" 120 "one helper replica remains Ready" helper_pod_count_is 1
  wait_before_deadline HA-SINGLE-REPLICA-SERVES "${deadline}" 90 "single replica serves the pool" leader_services_healthy
  assert_case HA-RESERVATION-SINGLE-REPLICA "reservation survives on a single replica" reservation_stable
  capture_checkpoint ha-single-replica \
    "one non-terminating helper replica serves the retained reservation"
  guard_case HA-SCALE-UP-SUBMITTED "helper deployment scales back to two replicas" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" scale deployment "${HELPER_DEPLOYMENT}" --replicas=2
  wait_before_deadline HA-SCALE-UP-READY "${deadline}" 120 "second helper replica returns Ready" helper_pods_ready
  wait_before_deadline HA-TWO-REPLICA-LEADERSHIP "${deadline}" 90 "two-replica leadership converges" leader_services_healthy
  assert_case HA-RESERVATION-TWO-REPLICAS "reservation survives scaling back to two replicas" \
    reservation_stable

  log "group ha: leader secondary interface down/up"
  assert_case HA-LEADER-BEFORE-LINK-BOUNCE \
    "leader state consistent before the secondary-interface bounce" leader_consistent
  guard_case HA-LINK-DOWN-SUBMITTED "leader secondary interface accepts DOWN transition" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
      ip link set "${KIH_HELPER_INTERFACE}" down
  wait_before_deadline HA-LINK-DOWN "${deadline}" 30 "leader interface reports DOWN" leader_link_state_is DOWN
  capture_checkpoint ha-link-down "leader secondary interface is DOWN while the reservation remains held"
  guard_case HA-LINK-UP-SUBMITTED "leader secondary interface accepts UP transition" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
      ip link set "${KIH_HELPER_INTERFACE}" up
  wait_before_deadline HA-LINK-UP "${deadline}" 30 "leader interface reports UP" leader_link_state_is UP
  wait_before_deadline HA-HEALTH-AFTER-LINK-BOUNCE "${deadline}" 90 "service remains healthy after interface bounce" \
    leader_services_healthy
  assert_case HA-RESERVATION-AFTER-LINK-BOUNCE "reservation survives the interface bounce" \
    reservation_stable
  capture_checkpoint ha-link-up "leader secondary interface recovered and service remains healthy"

  log "group ha: simultaneous deletion of both replicas"
  old_uids="$(helper_pod_uids || true)"
  guard_case HA-TOTAL-POD-SNAPSHOT "both helper UIDs captured before total pod loss" \
    test "$(printf '%s\n' "${old_uids}" | sed '/^$/d' | wc -l)" -eq 2
  guard_case HA-TOTAL-PODS-DELETE "both helper replicas accept asynchronous deletion" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" delete pods -l "${HELPER_SELECTOR}" --wait=false
  wait_before_deadline HA-BOTH-PODS-REPLACED "${deadline}" 180 \
    "both old helper UIDs disappear and two new non-terminating Ready replicas appear" \
    helper_pods_replaced_since "${old_uids}"
  wait_before_deadline HA-RECONSTRUCT-AFTER-LOSS "${deadline}" 120 "leadership and services reconstruct after total pod loss" \
    leader_services_healthy
  wait_before_deadline HA-RESERVATION-AFTER-LOSS "${deadline}" 90 \
    "reservation reconstructed after total pod loss" reservation_stable
  wait_before_deadline HA-METRICS-AFTER-LOSS "${deadline}" 60 \
    "IPPool metrics reconstructed after total pod loss" metric_pool_equals 1 10
  wait_before_deadline HA-VM-METRIC-AFTER-LOSS "${deadline}" 60 \
    "VM metric reconstructed after total pod loss" metric_vm_ok
  capture_checkpoint 16-ha-after-total-pod-loss \
    "leadership, reservation and metrics reconstructed after total pod loss"
  start_guest_and_assert churn "${deadline}"
  stop_guest churn
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true --timeout=120s
  wait_before_deadline HA-RESERVATION-RELEASED "${deadline}" 120 \
    "guest deletion releases the reservation" cleanup_complete
  wait_before_deadline HA-METRICS-AFTER-RELEASE "${deadline}" 60 \
    "metrics return to the empty pool after the release" metric_pool_equals 0 11
  assert_case HA-VM-METRIC-AFTER-RELEASE "cleanup removes the VM metric" metric_vm_absent
  capture_checkpoint 24-ha-reservation-released \
    "${KIH_VM_NAME} released ${RESERVED_IP} and its metric after helper churn"
  printf 'PASS HA group: reservation retained through churn, scaling, bounce, and total pod loss\n' \
    > "${E2E_ARTIFACTS_DIR}/12-ha-group.txt"
}

console_has_live_dhcp_activity() { # <file>
  grep -qF "E2E_DHCP_EVENT=bound:${RESERVED_IP}" "$1" ||
    grep -qF "E2E_DHCP_EVENT=renew:${RESERVED_IP}" "$1"
}

capture_live_dhcp_activity() { # <label> <absolute deadline>
  local label="$1" deadline="$2" console_log console_budget console_timeout_minutes
  console_log="${E2E_ARTIFACTS_DIR}/console-${label}.log"
  : > "${console_log}"
  console_budget=$((deadline - SECONDS))
  guard_case "LEASE-${label}-CONSOLE-WINDOW" \
    "live DHCP console window remains before the deadline (${label})" \
    test "${console_budget}" -gt 0
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
  wait_before_deadline LEASE-LIVE-DHCP-EVENT "${deadline}" 60 "live guest emits a subsequent DHCP client event" \
    console_has_live_dhcp_activity "${console_log}"
  capture_checkpoint 13-lease-live-dhcp "live guest DHCP activity under a 30 second lease"
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
  report_group lease
  local deadline manifest lease_leader reloads_before
  deadline=$((SECONDS + 420))
  log "group lease: observing live guest DHCP activity with a short lease"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" \
    --ignore-not-found --wait=true --timeout=120s
  wait_before_deadline LEASE-STARTS-EMPTY "${deadline}" 90 "lease group starts from an empty pool" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  assert_case LEASE-LEADER-BEFORE-PATCH "leader state consistent before the short-lease update" \
    leader_consistent
  lease_leader="${LEADER_POD}"
  reloads_before="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${lease_leader}" 2> /dev/null |
    grep -c 'IPPool configuration changes detected, updating the dhcppool' || true)"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p '{"spec":{"ipv4config":{"leasetime":30}}}'
  wait_before_deadline LEASE-SHORT-LEASE-RELOAD "${deadline}" 60 "short-lease update reaches the live DHCP pool" \
    reload_count_exceeds "${lease_leader}" "${reloads_before}"
  wait_before_deadline LEASE-POOL-HEALTH "${deadline}" 90 "short-lease pool remains healthy" leader_services_healthy

  manifest="${E2E_ARTIFACTS_DIR}/16-lease-vm.yaml"
  render_halted_vm "${KIH_VM_NAME}" "${KIH_VM_MAC}" "${manifest}"
  kubectl apply -f "${manifest}" > /dev/null
  wait_before_deadline LEASE-RESERVATION "${deadline}" 120 "short-lease VM reservation" vm_reservation_ready
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert short-lease "${deadline}"
  capture_live_dhcp_activity live-dhcp "${deadline}"
  wait_before_deadline LEASE-RESERVATION-STABLE "${deadline}" 60 "live DHCP activity preserves the reservation" reservation_stable
  stop_guest short-lease
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true --timeout=120s
  wait_before_deadline LEASE-RESERVATION-RELEASED "${deadline}" 120 "lease group releases its reservation" cleanup_complete
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"leasetime\":${E2E_RETAINED_LEASE_SECONDS}}}}"
  wait_before_deadline LEASE-NORMAL-LEASE-RESTORED "${deadline}" 90 "normal lease configuration is restored" \
    leader_services_healthy
  capture_checkpoint 14-lease-restored "normal lease window restored on ${KIH_IPPOOL_NAME}"
  printf 'PASS lease group: live guest repeated DHCP activity retained the reservation\n' \
    > "${E2E_ARTIFACTS_DIR}/13-lease-group.txt"
}

helper_pods_have_interface() { # <interface>
  local pods pod links
  helper_pods_ready || return 1
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || return 1
  [ -n "${pods}" ] || return 1
  for pod in ${pods}; do
    links="$(kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
      ip -o link show 2> /dev/null)" || return 1
    printf '%s\n' "${links}" | grep -Eq "(^| )$1(@|:)" || return 1
  done
}

# The primary-only topology has to be proven, not assumed: both replicas have to
# exist, and neither may still carry the second attachment.
helper_pods_lack_interface() { # <interface>
  local pods pod links
  helper_pods_ready || return 1
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" \
    -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || return 1
  [ -n "${pods}" ] || return 1
  for pod in ${pods}; do
    links="$(kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
      ip -o link show 2> /dev/null)" || return 1
    if printf '%s\n' "${links}" | grep -Eq "(^| )$1(@|:)"; then
      return 1
    fi
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

second_ippool_absent() {
  object_absent_not_found get ippool e2e-pool-second
}

second_server_ip_absent() {
  local addresses
  leader_consistent || return 1
  addresses="$(kubectl -n "${KIH_HELPER_NAMESPACE}" exec "${LEADER_POD}" -- \
    ip -4 addr show dev kihnet1 2> /dev/null)" || return 1
  ! printf '%s\n' "${addresses}" | grep -qF 'inet 10.78.0.2/24'
}

second_nad_absent() {
  object_absent_not_found -n "${KIH_HELPER_NAMESPACE}" \
    get network-attachment-definition "${KIH_NAD_NAME}-second"
}

run_multipool_group() {
  report_group multipool
  local deadline old_vm old_mac helper_snapshot
  deadline=$((SECONDS + 900))
  log "group multipool: attaching an independent second bridge and pool"
  report_case_start MULTI-STALE-RESOURCES-CLEARED \
    "no stale second-pool resources remain from an earlier run"
  if kubectl get ippool e2e-pool-second > /dev/null 2>&1 ||
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg multipool-vm > /dev/null 2>&1 ||
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg multipool-guest > /dev/null 2>&1 ||
    kubectl -n "${KIH_HELPER_NAMESPACE}" get network-attachment-definition \
      kubevirt-ip-helper-e2e-second > /dev/null 2>&1; then
    die "stale multipool resources exist; remove e2e-pool-second, multipool-vm, multipool-guest, and kubevirt-ip-helper-e2e-second before rerunning"
  fi
  report_case_pass "second-pool namespace is clean"
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
  wait_before_deadline MULTI-PODS-RETURN "${deadline}" 180 "two helper pods return after second attachment" \
    helper_pods_ready
  wait_before_deadline MULTI-BOTH-KIHNET1 "${deadline}" 120 "both helper pods contain kihnet1" \
    helper_pods_have_interface kihnet1
  wait_before_deadline MULTI-PRIMARY-RECONSTRUCTS "${deadline}" 120 "primary pool reconstructs after attachment rollout" \
    leader_services_healthy

  wait_before_deadline MULTI-SECOND-INITIALIZED "${deadline}" 120 "second pool initializes independently" \
    pool_initialized_named e2e-pool-second 3
  wait_before_deadline MULTI-SECOND-SERVER "${deadline}" 120 "leader serves the second pool address" \
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
  wait_before_deadline MULTI-SECOND-ALLOCATION "${deadline}" 120 "second pool allocates its own reservation" \
    vmnetcfg_status_is multipool-vm OK
  wait_before_deadline MULTI-SECOND-ISOLATED "${deadline}" 60 "second pool accounting is isolated" \
    pool_counts_equal e2e-pool-second 1 2
  capture_checkpoint 17-second-pool-added "second bridge, pool and reservation are independent"
  wait_before_deadline MULTI-PRIMARY-STAYS-EMPTY "${deadline}" 60 "primary pool remains empty" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg multipool-vm \
    --wait=true --timeout=120s
  wait_before_deadline MULTI-SECOND-RELEASED "${deadline}" 120 "second pool reservation is released" \
    pool_counts_equal e2e-pool-second 0 3

  log "group multipool: real guest on the second bridge"
  render_second_nad_vm multipool-guest 02:00:00:00:02:02 \
    "${E2E_ARTIFACTS_DIR}/20-second-nad-vm.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/20-second-nad-vm.yaml" > /dev/null
  wait_before_deadline MULTI-GUEST-RESERVATION "${deadline}" 120 \
    "second bridge reserves the guest address" vmnetcfg_status_is multipool-guest OK
  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="multipool-guest"
  KIH_VM_MAC="02:00:00:00:02:02"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  report_case_start MULTI-GUEST-SECOND-SUBNET \
    "second bridge hands out its own 10.78.0.x range"
  case "${RESERVED_IP}" in
    10.78.0.*) report_case_pass "reserved ${RESERVED_IP}" ;;
    *)
      die "second-NAD guest got ${RESERVED_IP} instead of a 10.78.0.x address"
      ;;
  esac
  wait_before_deadline MULTI-GUEST-ACCOUNTING "${deadline}" 60 \
    "second pool counts only the guest reservation" pool_counts_equal e2e-pool-second 1 2
  wait_before_deadline MULTI-GUEST-PRIMARY-ISOLATED "${deadline}" 60 \
    "guest on the second bridge leaves the primary pool empty" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  capture_checkpoint 25-second-nad-guest "second bridge holds ${RESERVED_IP} for multipool-guest"
  start_guest_and_assert second-nad "${deadline}"
  stop_guest second-nad
  wait_before_deadline MULTI-GUEST-RESERVATION-HELD "${deadline}" 60 \
    "halted second-NAD guest keeps its address" pool_counts_equal e2e-pool-second 1 2
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true --timeout=120s
  wait_before_deadline MULTI-GUEST-OBJECTS-GONE "${deadline}" 120 \
    "second-NAD guest releases its VMNetCfg" vmnetcfg_absent_named multipool-guest
  wait_before_deadline MULTI-GUEST-RELEASED "${deadline}" 60 \
    "second pool empties after the guest is deleted" pool_counts_equal e2e-pool-second 0 3
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"

  log "group multipool: deleting the second IPPool while both helpers keep serving"
  helper_snapshot="$(helper_pod_runtime_snapshot || true)"
  guard_case MULTI-HELPER-SNAPSHOT \
    "both helper UIDs and restart counts captured before second-pool deletion" \
    test "$(printf '%s\n' "${helper_snapshot}" | sed '/^$/d' | wc -l)" -eq 2
  guard_case MULTI-SECOND-POOL-DELETE \
    "second IPPool deletion is accepted while helpers are live" \
    kubectl delete ippool e2e-pool-second --wait=true --timeout=120s
  wait_before_deadline MULTI-SECOND-POOL-GONE "${deadline}" 90 \
    "second IPPool object is removed while helpers stay live" second_ippool_absent
  wait_before_deadline MULTI-SECOND-SERVER-REMOVED "${deadline}" 90 \
    "leader drops the second pool server address" second_server_ip_absent
  wait_before_deadline MULTI-SECOND-METRICS-GONE "${deadline}" 90 \
    "second pool metrics disappear with its IPPool" metric_ippool_absent e2e-pool-second
  wait_before_deadline MULTI-PRIMARY-HEALTH-WITH-SECOND-REMOVED "${deadline}" 120 \
    "primary DHCP service stays healthy beside the detached second bridge" \
    leader_services_healthy
  wait_before_deadline MULTI-HELPERS-UNCHANGED "${deadline}" 90 \
    "both helper pods stay live with unchanged UIDs and restart counts" \
    helper_pods_unchanged_since "${helper_snapshot}"
  assert_case MULTI-PRIMARY-METRICS-WITH-SECOND-REMOVED \
    "primary pool accounting stays exact after the second pool is removed" \
    metric_pool_equals 0 11
  capture_checkpoint 18-second-pool-removed \
    "second pool objects and metrics are gone while the primary pool keeps serving"

  log "group multipool: restoring the primary-only attachment"
  kubectl -n "${KIH_HELPER_NAMESPACE}" patch deployment "${HELPER_DEPLOYMENT}" \
    --type=merge \
    -p '{"spec":{"template":{"metadata":{"annotations":{"k8s.v1.cni.cncf.io/networks":"[{\"name\":\"kubevirt-ip-helper-e2e\",\"namespace\":\"kubevirt-ip-helper\",\"interface\":\"kihnet0\"}]"}}}}}'
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  wait_before_deadline MULTI-PRIMARY-ONLY-TOPOLOGY "${deadline}" 180 \
    "primary-only helper topology returns" helper_pods_ready
  wait_before_deadline MULTI-KIHNET1-REMOVED "${deadline}" 120 \
    "second interface is gone from both helper pods" helper_pods_lack_interface kihnet1
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete network-attachment-definition \
    kubevirt-ip-helper-e2e-second --ignore-not-found > /dev/null
  wait_before_deadline MULTI-NAD-REMOVED "${deadline}" 90 \
    "second NetworkAttachmentDefinition is removed" second_nad_absent
  wait_before_deadline MULTI-SECOND-METRICS-STAY-GONE "${deadline}" 90 \
    "second pool metrics stay absent after the topology restore" \
    metric_ippool_absent e2e-pool-second
  wait_before_deadline MULTI-PRIMARY-HEALTH-AFTER-REMOVAL "${deadline}" 120 \
    "primary pool remains healthy after second-pool removal" leader_services_healthy
  capture_checkpoint 26-second-resources-absent \
    "primary-only topology with second pool, interface, NAD, and metrics absent"
  printf 'PASS multipool group: second bridge attachment, real guest DHCP, live pool removal, and cleanup\n' \
    > "${E2E_ARTIFACTS_DIR}/14-multipool-group.txt"
}

main() {
  local rendered vm_rendered default_image old_leader old_id octet failover_deadline failover_budget retained_lease_deadline router_original reinit_before
  # versions.env composes E2E_ARTIFACTS_DIR as ${root}/runs/${E2E_RUN_ID},
  # and report/evidence derive the artifact root by stripping that exact
  # suffix. An environment override that does not carry it would make those
  # derivations misbehave, so reject it before any run artifact is written.
  # shellcheck disable=SC2153 # E2E_RUN_ID is assigned in versions.env, which shellcheck cannot follow
  case "${E2E_ARTIFACTS_DIR}" in
    */runs/${E2E_RUN_ID}) ;;
    *)
      die "E2E_ARTIFACTS_DIR must be unset or end with runs/${E2E_RUN_ID}"
      ;;
  esac
  rm -f "${E2E_CLUSTER_STATE_FILE}"
  report_case_start CORE-PREREQUISITES \
    "container runtime, kubectl, GNU timeout, and jq are available"
  resolve_runtime
  report_case_pass "runtime ${RUNTIME}, kubectl, GNU timeout, and jq available"
  report_case_start CORE-IMAGE-BUILT "helper image ${E2E_IMAGE} built from ${ROOT_DIR}"
  "${RUNTIME}" build -t "${E2E_IMAGE}" "${ROOT_DIR}"
  report_case_pass "image ${E2E_IMAGE} present in ${RUNTIME}"
  report_case_start CORE-BOOTSTRAP \
    "disposable cluster bootstrapped with bridge CNI, Multus and KubeVirt"
  REPORT_BOOTSTRAP_STARTED=1
  "${E2E_DIR}/bootstrap.sh"
  report_case_pass "cluster ${E2E_CLUSTER_NAME} up on ${KUBERNETES_VERSION}"
  report_case_start CORE-BOOTSTRAP-JOURNAL \
    "bootstrap gate journal is parseable and imported into the suite report"
  if report_import_bootstrap_cases 1; then
    report_case_pass "bootstrap-cases.jsonl imported"
  else
    die "cannot import bootstrap-cases.jsonl"
  fi
  capture_checkpoint 01-bootstrap "cluster, CNI chain and KubeVirt right after bootstrap"
  assert_case CORE-VIRTCTL-INSTALLED "bootstrap installed ${VIRTCTL}" test -x "${VIRTCTL}"
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
  assert_case CORE-RENDERED-IMAGE "rendered helper image equals ${E2E_IMAGE}" \
    grep -q "image: ${E2E_IMAGE}" "${rendered}"
  kubectl apply -f "${rendered}"
  report_case_start CORE-HELPER-ROLLED-OUT "helper deployment restarted and rolled out"
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout restart \
    "deployment/${HELPER_DEPLOYMENT}"
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  report_case_pass "deployment/${HELPER_DEPLOYMENT} rolled out"
  wait_for CORE-HELPER-PODS-READY 120 "two helper pods Ready with ${KIH_HELPER_INTERFACE}" helper_pods_ready
  wait_for CORE-LEADER-CONSISTENT 120 "one labelled leader, matching Lease, and one metrics endpoint" leader_consistent
  capture_checkpoint 02-helper-ready "helper replicas Ready with ${KIH_HELPER_INTERFACE} and one leader"
  report_case_start CORE-STALE-RESOURCES-CLEARED \
    "expanded-group resources from an interrupted run are removed before core setup"
  cleanup_stale_expanded_resources
  report_case_pass "pool, multipool, and second-NAD resources are absent before core setup"
  capture_checkpoint 02-start-clean \
    "expanded-group resources cleared after helper CRDs and serving objects are ready"

  kubectl apply -f "${E2E_DIR}/manifests/pool.yaml"
  wait_for CORE-POOL-INITIALIZED 120 "IPPool initialized with 11 available addresses" pool_initialized
  wait_for CORE-LEADER-SERVICES 120 "leader owns ${KIH_IPPOOL_SERVER}/24 and UDP/67" leader_services_healthy
  wait_for CORE-METRICS-EMPTY 60 "initial IPPool metrics" metric_pool_equals 0 11
  capture_checkpoint 03-pool-initialized "${KIH_IPPOOL_NAME} initialized with 11 free addresses"

  # manifests/vm.yaml is the current-profile template. Render it into this
  # profile's artifact directory and substitute the profile's guest image, so the
  # dependency-era lane never starts the guest on the current Cirros image.
  vm_rendered="${E2E_ARTIFACTS_DIR}/vm-rendered.yaml"
  sed "s|${KIH_GUEST_IMAGE_TEMPLATE}|${KIH_GUEST_IMAGE}|" \
    "${E2E_DIR}/manifests/vm.yaml" > "${vm_rendered}"
  assert_case CORE-GUEST-IMAGE-RENDERED "rendered guest image equals ${KIH_GUEST_IMAGE}" \
    grep -qF "image: ${KIH_GUEST_IMAGE}" "${vm_rendered}"
  report_case_start CORE-GUEST-IMAGE-SUBSTITUTED \
    "guest manifest replaced the ${KIH_GUEST_IMAGE_TEMPLATE} template for ${E2E_STACK}"
  if [ "${KIH_GUEST_IMAGE}" != "${KIH_GUEST_IMAGE_TEMPLATE}" ] &&
    grep -qF "image: ${KIH_GUEST_IMAGE_TEMPLATE}" "${vm_rendered}"; then
    die "rendered guest manifest kept ${KIH_GUEST_IMAGE_TEMPLATE} for the ${E2E_STACK} profile"
  fi
  report_case_pass "guest runs on ${KIH_GUEST_IMAGE}"
  kubectl apply -f "${vm_rendered}"
  assert_case CORE-VM-CREATED-HALTED "VM ${KIH_VM_NAME} was created with runStrategy Halted" \
    test "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vm "${KIH_VM_NAME}" \
      -o jsonpath='{.spec.runStrategy}')" = "Halted"
  assert_case CORE-NO-VMI-BEFORE-RESERVATION \
    "no VMI exists before the helper reserved an address" vmi_absent
  wait_for CORE-HALTED-RESERVATION 120 "VMNetCfg reservation while VM is halted" vm_reservation_ready
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  octet="${RESERVED_IP##*.}"
  report_case_start CORE-RESERVATION-IN-POOL-RANGE \
    "reserved address stays inside ${KIH_IPPOOL_START}-${KIH_IPPOOL_END}"
  if [ "${RESERVED_IP%.*}" = "10.77.0" ] && [ "${octet}" -ge 100 ] && [ "${octet}" -le 110 ]; then
    report_case_pass "reserved ${RESERVED_IP}"
  else
    die "reservation ${RESERVED_IP} is outside 10.77.0.100-10.77.0.110"
  fi
  wait_for CORE-ALLOCATION-MATCHES 60 "IPPool allocation matches VMNetCfg" pool_allocation_matches
  wait_for CORE-METRICS-RESERVED 60 "IPPool used/available metrics after reservation" metric_pool_equals 1 10
  wait_for CORE-METRICS-VMNETCFG-OK 60 "VMNetCfg OK metric" metric_vm_ok
  capture_checkpoint 04-halted-reservation "halted VM holds ${RESERVED_IP} with IPPool accounting"

  start_guest_and_assert initial
  capture_checkpoint 05-boot-initial "first guest boot answered DHCP for ${RESERVED_IP}"
  stop_guest initial
  start_guest_and_assert restart
  capture_checkpoint 06-boot-restart "second guest boot reused ${RESERVED_IP} after a stop"
  stop_guest restart

  # A router change is restart-class: the controller has to tear the DHCP
  # listener down and reinitialize the whole application, so the reservation,
  # the accounting and both metrics have to survive a full rebuild.
  log "core: router change forces application reinitialization"
  router_original="$(kubectl get ippool "${KIH_IPPOOL_NAME}" \
    -o jsonpath='{.spec.ipv4config.router}' 2> /dev/null)"
  assert_case CORE-ROUTER-BEFORE-PATCH "router reads ${router_original} before the restart-class patch" \
    test -n "${router_original}"
  reinit_before="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs -l "${HELPER_SELECTOR}" --tail=200 2> /dev/null |
    grep -c 'starting application reinitialization' || true)"
  capture_checkpoint 19-router-before-restart "leader serving ${RESERVED_IP} before the restart-class router change"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p '{"spec":{"ipv4config":{"router":"10.77.0.9"}}}'
  wait_for CORE-ROUTER-RESTART-LOG 90 "router change starts application reinitialization" \
    reinit_count_exceeds "${reinit_before}"
  wait_for CORE-ROUTER-RESTART-SERVICES 120 "leader reconstructs server IP, UDP/67, and metrics after reinitialization" \
    leader_services_healthy
  wait_for CORE-ROUTER-RESTART-RESERVATION 90 "reservation survives application reinitialization" reservation_stable
  wait_for CORE-ROUTER-RESTART-METRICS 60 "IPPool accounting survives application reinitialization" \
    metric_pool_equals 1 10
  wait_for CORE-ROUTER-RESTART-VM-METRIC 60 "VM metric survives application reinitialization" metric_vm_ok
  start_guest_and_assert router
  capture_checkpoint 19-router-changed \
    "guest observed router 10.77.0.9 after application reinitialization"
  stop_guest router
  reinit_before="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs -l "${HELPER_SELECTOR}" --tail=200 2> /dev/null |
    grep -c 'starting application reinitialization' || true)"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"router\":\"${router_original}\"}}}"
  wait_for CORE-ROUTER-RESTORE-LOG 90 "restored router starts a second reinitialization" \
    reinit_count_exceeds "${reinit_before}"
  wait_for CORE-ROUTER-RESTORE-SERVICES 120 "services recover from the restored router" leader_services_healthy
  wait_for CORE-ROUTER-RESTORE-RESERVATION 90 "reservation stable after the restored router" reservation_stable
  wait_for CORE-ROUTER-RESTORE-METRICS 60 "metrics stable after the restored router" metric_pool_equals 1 10
  assert_case CORE-ROUTER-RESTORED "router is back at ${router_original}" \
    test "$(kubectl get ippool "${KIH_IPPOOL_NAME}" \
      -o jsonpath='{.spec.ipv4config.router}')" = "${router_original}"
  start_guest_and_assert router-restored
  capture_checkpoint 20-router-after-restore \
    "guest observed restored router ${router_original} after the reverse reinitialization"
  stop_guest router-restored

  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"leasetime\":${E2E_RETAINED_LEASE_SECONDS}}}}"
  wait_for CORE-RELOAD-PROCESSED 60 "reloadable IPPool update processed" reload_processed
  wait_for CORE-HEALTH-AFTER-RELOAD 90 "DHCP and metrics healthy after IPPool reload" leader_services_healthy
  wait_for CORE-STABLE-AFTER-RELOAD 90 "reservation and metrics stable after reload" reservation_stable
  wait_for CORE-METRICS-AFTER-RELOAD 60 "metrics stable after reload" metric_pool_equals 1 10
  capture_checkpoint 07-pool-reload "leader reloaded the patched ${KIH_IPPOOL_NAME} lease window"
  # Start the lease clock before the DHCP boot. The failover proof may use less
  # than its nominal budget after stop latency, but can never pass after expiry.
  retained_lease_deadline=$((SECONDS + E2E_RETAINED_LEASE_SECONDS))
  start_guest_and_assert reload
  capture_checkpoint 08-boot-reload "guest boot after reload kept ${RESERVED_IP}"
  stop_guest reload

  assert_case FAILOVER-LEADER-STABLE-BEFORE \
    "leader state consistent before the active leader is deleted" leader_consistent
  old_leader="${LEADER_POD}"
  old_id="${LEADER_ID}"
  failover_deadline=$((SECONDS + E2E_FAILOVER_DHCP_TIMEOUT))
  [ "${failover_deadline}" -le "${retained_lease_deadline}" ] ||
    failover_deadline="${retained_lease_deadline}"
  failover_budget=$((failover_deadline - SECONDS))
  guard_case FAILOVER-LEASE-WINDOW \
    "retained lease remains before active leader deletion" \
    test "${failover_budget}" -gt 0
  guard_case FAILOVER-LEADER-DELETE \
    "active leader deletion is accepted before the failover deadline" \
    timeout --foreground "${failover_budget}s" kubectl \
      -n "${KIH_HELPER_NAMESPACE}" delete pod "${old_leader}" --wait=false
  wait_before_deadline FAILOVER-LEADER-TRANSFER "${failover_deadline}" 75 "leader label and Lease transfer" \
    new_leader_elected "${old_leader}" "${old_id}"
  wait_before_deadline FAILOVER-SERVICES "${failover_deadline}" 90 \
    "new leader reconstructs server IP, UDP/67, and metrics" leader_services_healthy
  wait_before_deadline FAILOVER-RESERVATION "${failover_deadline}" 90 \
    "new leader reconstructs reservation" reservation_stable
  wait_before_deadline FAILOVER-METRICS "${failover_deadline}" 60 \
    "new leader reconstructs IPPool metrics" metric_pool_equals 1 10
  wait_before_deadline FAILOVER-VM-METRIC "${failover_deadline}" 60 \
    "new leader reconstructs VM metric" metric_vm_ok
  start_guest_and_assert failover "${failover_deadline}"
  capture_checkpoint 09-leader-failover "new leader served the retained lease during failover"

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true
  wait_for CORE-CLEANUP-COMPLETE 120 "VM deletion releases VMNetCfg and IP allocation" cleanup_complete
  wait_for CORE-METRICS-AFTER-CLEANUP 60 "cleanup updates IPPool metrics" metric_pool_equals 0 11
  wait_for CORE-VM-METRIC-REMOVED 60 "cleanup removes VM metric" metric_vm_absent
  capture_checkpoint 10-cleanup "VM deletion released ${RESERVED_IP} and its metric"

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

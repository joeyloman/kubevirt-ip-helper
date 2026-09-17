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
export E2E_ARTIFACTS_DIR E2E_RUN_ID VIRTCTL
export KUBECONFIG="${KUBECONFIG:-${E2E_ARTIFACTS_DIR}/kubeconfig}"
E2E_CLUSTER_STATE_FILE="${E2E_ARTIFACTS_DIR}/cluster-state"
export E2E_CLUSTER_STATE_FILE

KIND="${E2E_BIN_DIR}/kind"
HELPER_DEPLOYMENT="${KIH_HELPER_DEPLOYMENT}"
HELPER_SELECTOR="${KIH_HELPER_SELECTOR}"
LEADER_SELECTOR="${HELPER_SELECTOR},kubevirtiphelper/leader=active"
LEADER_LEASE="${KIH_LEADER_LEASE}"
METRICS_SERVICE="${KIH_METRICS_SERVICE}"
HELPER_REPLICAS=2
export E2E_SECOND_NETWORK_EXPECTED=0
E2E_VM_BOOT_TIMEOUT="${E2E_VM_BOOT_TIMEOUT:-300}"
E2E_PRED_SECONDS="${E2E_PRED_SECONDS:-20}"
# Lease-loss fast-fail: repeated "NO LEASE FOUND" entries for the owner MAC
# inside the storm window are symptom-based evidence of an ownership regression.
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
PREDICATE_PID=""
PREDICATE_WATCHDOG_PID=""
declare -A CAPTURE_PIDS=() CAPTURE_PODS=() CAPTURE_FILES=() CAPTURE_TOKENS=()
GUEST_CONSOLE=""
GUEST_EVENTS=""
GUEST_EXPECTED=""
GUEST_IDENTITY=""
GUEST_BASELINE=""
GUEST_BASELINE_INDEX=-1
GUEST_NODE=""
GUEST_DEADLINE=0
STOPPED_WORKER=""
STOPPED_WORKER_ID=""
SCENARIO_DEADLINE=0
RELOAD_MARKER='IPPool configuration changes detected, updating the dhcppool'
REINIT_MARKER='starting application reinitialization'

# Bound direct API calls too. Poll predicates inherit their shorter process-
# group deadline; explicit long-lived captures use their own outer timeout.
kubectl() {
  local budget="${E2E_WAIT_TIMEOUT}" remaining
  if [ "${SCENARIO_DEADLINE}" -gt 0 ]; then
    remaining=$((SCENARIO_DEADLINE - SECONDS))
    [ "${remaining}" -gt 0 ] || return 124
    [ "${budget}" -le "${remaining}" ] || budget="${remaining}"
  fi
  timeout --foreground --kill-after=1s "${budget}s" kubectl "$@"
}

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

remove_owned_data_network() { # <name>, only after successful owned-cluster deletion
  local name="$1" names object id
  names="$(timeout 20s "${RUNTIME}" network ls --format '{{.Name}}')" || return 1
  grep -Fxq -- "${name}" <<< "${names}" || return 0
  object="$(timeout 20s "${RUNTIME}" network inspect "${name}")" || return 1
  printf '%s\n' "${object}" > "${E2E_ARTIFACTS_DIR}/network-cleanup-${name}.json"
  id="$(jq -er --arg name "${name}" --arg label "${KIH_RUNTIME_NETWORK_OWNER_LABEL}" \
    --arg owner "${E2E_CLUSTER_NAME}" '
    select(length == 1) | .[0]
    | select((.Name // .name) == $name and ((.Labels // .labels)[$label]) == $owner)
    | (.Id // .id) | select(type == "string" and test("^[0-9a-f]{64}$"))
  ' <<< "${object}")" || return 1
  if [ "${RUNTIME}" = podman ]; then
    # Podman accepts IDs for removal but checks container attachments by name.
    # Use the just-verified name so non-force removal retains that safety check.
    timeout 20s "${RUNTIME}" network rm "${name}"
  else
    # Docker checks attachments when removing by immutable identity.
    timeout 20s "${RUNTIME}" network rm "${id}"
  fi
}

worker_record() { # <node>
  local object
  object="$(timeout 20s "${RUNTIME}" inspect "$1")" || return 1
  jq -ce --arg name "$1" --arg cluster "${E2E_CLUSTER_NAME}" '
    select(length == 1) | .[0]
    | select((.Name | ltrimstr("/")) == $name
      and .Config.Labels["io.x-k8s.kind.cluster"] == $cluster)
    | {id:(.Id // .ID), running:.State.Running}
    | select((.id|type) == "string" and (.id|test("^[0-9a-f]{64}$"))
      and (.running|type) == "boolean")
  ' <<< "${object}"
}

restore_stopped_worker() { # <absolute SECONDS deadline>
  local remaining object
  [ -n "${STOPPED_WORKER}" ] || return 0
  object="$(worker_record "${STOPPED_WORKER}")" || return 1
  [ "$(jq -r '.id' <<< "${object}")" = "${STOPPED_WORKER_ID}" ] || return 1
  remaining=$(($1 - SECONDS))
  [ "${remaining}" -gt 0 ] || return 1
  timeout --kill-after=1s "${remaining}s" "${RUNTIME}" start "${STOPPED_WORKER_ID}" || return 1
  remaining=$(($1 - SECONDS))
  [ "${remaining}" -gt 0 ] || return 1
  E2E_RUNTIME="${RUNTIME}" timeout --kill-after=1s "${remaining}s" \
    "${E2E_DIR}/bootstrap.sh" restore-node-networks "${STOPPED_WORKER}"
}

finish() {
  local rc="${1:-$?}" collection_rc=0 report_rc=0 diagnostic_errors cleanup_rc=0 network
  trap - EXIT
  trap '' INT TERM HUP
  set +e
  stop_predicate_attempt
  SCENARIO_DEADLINE=0
  if ! finish_guest_evidence "$((SECONDS + E2E_COLLECT_TOTAL_TIMEOUT))"; then
    report_case_start SUITE-GUEST-EVIDENCE "guest recorders stopped and captures decoded completely"
    report_case_fail "a recorder could not be stopped or its final capture was incomplete"
    rc=1
  fi
  if [ -n "${STOPPED_WORKER}" ]; then
    report_case_start SUITE-WORKER-RECOVERY "intentionally stopped worker restored before diagnostics"
    if restore_stopped_worker "$((SECONDS + E2E_COLLECT_TOTAL_TIMEOUT))"; then
      report_case_pass "worker ${STOPPED_WORKER} and both test uplinks restored"
      STOPPED_WORKER="" STOPPED_WORKER_ID=""
    else
      report_case_fail "could not restore ${STOPPED_WORKER}; retained for diagnosis"
      rc=1
    fi
  fi
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
    else
      for network in "${KIH_DATA_NETWORK}" "${KIH_SECOND_DATA_NETWORK}"; do
        report_case_start "SUITE-NETWORK-CLEANUP-${network}" "owned test network removed after cluster deletion"
        if remove_owned_data_network "${network}"; then
          report_case_pass "${network} absent"
        else
          report_case_fail "network identity/ownership check or non-forced removal failed: ${network}"
          rc=1
        fi
      done
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

# Predicates run in their own process group. A successful leader observation
# crosses that boundary as validated data, never as sourced shell code.
stop_predicate_attempt() {
  local pid
  for pid in "${PREDICATE_PID:-}" "${PREDICATE_WATCHDOG_PID:-}"; do
    [ -n "${pid}" ] || continue
    kill -KILL -- "-${pid}" > /dev/null 2>&1 || true
    wait "${pid}" > /dev/null 2>&1 || true
  done
  PREDICATE_PID=""
  PREDICATE_WATCHDOG_PID=""
}

run_pred_once() { # <seconds> <predicate> [args...]
  local guard="$1" rc=0 monitor_was_on="" pod holder extra
  local PREDICATE_LEADER_STATE="${E2E_ARTIFACTS_DIR}/predicate-leader.tsv"
  shift
  [ "${guard}" -gt 0 ] || return 124
  rm -f "${PREDICATE_LEADER_STATE}"
  case $- in *m*) monitor_was_on=1 ;; esac
  set -m
  ( "$@" ) &
  PREDICATE_PID=$!
  (
    sleep "${guard}"
    kill -KILL -- "-${PREDICATE_PID}" > /dev/null 2>&1 || true
  ) &
  PREDICATE_WATCHDOG_PID=$!
  [ -n "${monitor_was_on}" ] || set +m
  wait "${PREDICATE_PID}" || rc=$?
  stop_predicate_attempt
  if [ "${rc}" -eq 0 ] && [ -e "${PREDICATE_LEADER_STATE}" ]; then
    IFS=$'\t' read -r pod holder extra < "${PREDICATE_LEADER_STATE}" || return 1
    [[ "${pod}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || return 1
    [[ "${holder}" =~ ^[0-9a-f-]+$ ]] || return 1
    [ -z "${extra}" ] && [ "$(wc -l < "${PREDICATE_LEADER_STATE}")" -eq 1 ] || return 1
    LEADER_POD="${pod}"
    LEADER_ID="${holder}"
  fi
  rm -f "${PREDICATE_LEADER_STATE}"
  return "${rc}"
}

wait_until() { # <case-id> <absolute SECONDS> <description> <predicate> [args...]
  local case_id="$1" deadline="$2" description="$3" start="${SECONDS}" remaining attempt rc nap
  shift 3
  report_case_start "${case_id}" "${description}"
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    remaining=$((deadline - SECONDS))
    attempt="${E2E_PRED_SECONDS}"
    [ "${attempt}" -le "${remaining}" ] || attempt="${remaining}"
    run_pred_once "${attempt}" "$@" && rc=0 || rc=$?
    if [ "${rc}" -eq 2 ]; then
      if [ "$1" = boot_network_or_lease_loss ]; then
        die "${description}: repeated DHCP lease lookup failures for guest MAC ${KIH_VM_MAC}"
      else
        die "${description}: predicate $1 exited with status ${rc}"
      fi
    fi
    if [ "${rc}" -eq 0 ] && [ "${SECONDS}" -lt "${deadline}" ]; then
      log "ok: ${description}"
      report_case_pass "satisfied $((SECONDS - start))s after the first attempt"
      return 0
    fi
    remaining=$((deadline - SECONDS))
    [ "${remaining}" -gt 0 ] || break
    nap=2
    [ "${nap}" -le "${remaining}" ] || nap="${remaining}"
    sleep "${nap}"
  done
  die "deadline expired: ${description}"
}

wait_for() { # <case-id> <seconds> <description> <predicate> [args...]
  local case_id="$1" seconds="$2" description="$3"
  shift 3
  wait_until "${case_id}" "$((SECONDS + seconds))" "${description}" "$@"
}

wait_before_deadline() { # <case-id> <absolute SECONDS> <maximum seconds> <description> <predicate> [args...]
  local case_id="$1" deadline="$2" maximum="$3" description="$4" window
  shift 4
  window=$((SECONDS + maximum))
  [ "${window}" -le "${deadline}" ] || window="${deadline}"
  wait_until "${case_id}" "${window}" "${description}" "$@"
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
  type -P kubectl > /dev/null 2>&1 || die "kubectl is required"
  command -v timeout > /dev/null 2>&1 || die "GNU timeout is required"
  command -v jq > /dev/null 2>&1 || die "jq is required for reports and evidence"
  command -v python3 > /dev/null 2>&1 || die "Python 3 is required for passive DHCP evidence"
  command -v openssl > /dev/null 2>&1 || die "OpenSSL is required for webhook TLS qualification"
  export E2E_RUNTIME="${RUNTIME}"
}

# A command failure alone is not admission proof: transport, schema and RBAC
# failures must not masquerade as a webhook's explicit validation denial.
admission_rejects() { # <manifest>
  local response
  if response="$(kubectl create --dry-run=server -f "$1" 2>&1)"; then
    printf '%s\n' "${response}" >&2
    return 1
  fi
  printf '%s\n' "${response}" > "${1}.admission.txt"
  [[ "${response}" == *'admission webhook "'*'denied the request'* ]]
}

webhook_ready() {
  local config service endpoints pods certificate csr ca dns
  dns="${KIH_WEBHOOK_SERVICE}.${KIH_HELPER_NAMESPACE}.svc"
  config="$(kubectl get validatingwebhookconfiguration "${KIH_WEBHOOK_CONFIGURATION}" -o json)" || return 1
  service="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get service "${KIH_WEBHOOK_SERVICE}" -o json)" || return 1
  endpoints="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints "${KIH_WEBHOOK_SERVICE}" -o json)" || return 1
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l app=kubevirt-ip-helper-webhook -o json)" || return 1
  jq -e -n --argjson config "${config}" --argjson service "${service}" \
    --argjson endpoints "${endpoints}" --argjson pods "${pods}" \
    --arg name "${KIH_WEBHOOK_SERVICE}" --arg ns "${KIH_HELPER_NAMESPACE}" '
      ($config.webhooks | length == 3)
      and all($config.webhooks[]; .clientConfig.service.name == $name
        and .clientConfig.service.namespace == $ns and .clientConfig.service.port == 8080
        and (.clientConfig.caBundle | length > 0))
      and $service.spec.selector.app == "kubevirt-ip-helper-webhook"
      and ($service.spec.selector | has("kubevirtiphelper/network") | not)
      and any($service.spec.ports[]; .port == 8080 and .targetPort == 8443)
      and ([$pods.items[] | select(.metadata.deletionTimestamp == null)] | length == 1)
      and ([$endpoints.subsets[]?.addresses[]?] | length == 1)
      and all($endpoints.subsets[]?.addresses[]?;
        .targetRef.uid as $uid | any($pods.items[];
          .metadata.uid == $uid and .metadata.labels.app == "kubevirt-ip-helper-webhook"
          and (.metadata.labels | has("kubevirtiphelper/network") | not)))
    ' > /dev/null || return 1
  certificate="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get secret "${KIH_WEBHOOK_TLS_SECRET}" \
    -o jsonpath='{.data.tls\.crt}')" || return 1
  csr="$(kubectl get csr "${dns}" -o json)" || return 1
  [ "$(jq -r '.status.certificate' <<< "${csr}")" = "${certificate}" ] || return 1
  jq -e 'any(.status.conditions[]; .type == "Approved" and .status == "True")' \
    <<< "${csr}" > /dev/null || return 1
  ca="$(jq -er '.webhooks[0].clientConfig.caBundle' <<< "${config}")" || return 1
  openssl verify -verify_hostname "${dns}" \
    -CAfile <(printf '%s' "${ca}" | base64 -d) \
    <(printf '%s' "${certificate}" | base64 -d) > /dev/null 2>&1 || return 1
  # Exercise Service delivery and TLS from an ordinary cluster client as well
  # as the apiserver admission requests below. Only the public CA is streamed.
  printf '%s' "${ca}" | base64 -d |
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" exec -i "${KIH_NETWORK_POD}" -c "${KIH_NETWORK_CONTAINER}" -- \
      sh -ec '[ "$(curl --fail --silent --show-error --max-time 10 --cacert /dev/stdin "$1")" = ok ]' \
      sh "https://${dns}:8080/readyz"
}

webhook_admission_qualified() {
  local manifest="${E2E_ARTIFACTS_DIR}/webhook-invalid-pool.yaml"
  # This valid CRD shape carries a semantically reversed range, so only the
  # actual webhook (not OpenAPI validation) can reject it.
  sed -e 's/name: e2e-pool/name: e2e-admission-probe/' \
    -e 's/start: 10.77.0.100/start: 10.77.0.110/' \
    -e 's/end: 10.77.0.110/end: 10.77.0.100/' \
    "${E2E_DIR}/manifests/pool.yaml" > "${manifest}"
  admission_rejects "${manifest}"
}

webhook_vmnetcfg_admission_qualified() {
  local manifest="${E2E_ARTIFACTS_DIR}/webhook-invalid-vmnetcfg.json"
  jq -n --arg ns "${KIH_WORKLOAD_NAMESPACE}" --arg network "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}" '
    {apiVersion:"kubevirtiphelper.k8s.binbash.org/v1",kind:"VirtualMachineNetworkConfig",
     metadata:{name:"e2e-admission-probe",namespace:$ns},
     spec:{vmname:"e2e-admission-probe",networkconfig:[
       {macaddress:"02:00:00:00:ff:01",networkname:$network,ipaddress:"10.77.0.99"}]}}
    ' > "${manifest}"
  admission_rejects "${manifest}"
}

# Bash dynamic scope keeps reused predicates network-local without mutating
# the primary failover bookkeeping or watchdog leader-state handoff.
on_secondary() {
  local HELPER_DEPLOYMENT="${KIH_SECOND_HELPER_DEPLOYMENT}"
  local HELPER_SELECTOR="${KIH_SECOND_HELPER_SELECTOR}"
  local LEADER_SELECTOR="${KIH_SECOND_HELPER_SELECTOR},kubevirtiphelper/leader=active"
  local LEADER_LEASE="${KIH_SECOND_LEADER_LEASE}" METRICS_SERVICE="${KIH_SECOND_METRICS_SERVICE}"
  local KIH_NAD_NAME="${KIH_SECOND_NAD_NAME}" KIH_HELPER_INTERFACE="${KIH_SECOND_HELPER_INTERFACE}"
  local KIH_IPPOOL_NAME="e2e-pool-second" KIH_IPPOOL_SERVER="10.78.0.2"
  local HELPER_REPLICAS=1 LEADER_POD="" LEADER_ID="" PREDICATE_LEADER_STATE=""
  "$@"
}

# Two live replicas have to be Ready with the helper interface attached. A pod which
# is already terminating is the Deployment's replacement in flight, so it does not count
# against the two replicas this predicate asserts.
#
# The interface is asserted from the pod's own network-status, which Multus writes when it
# builds the pod sandbox. `kubectl exec` would prove it from inside the pod as well, but
# exec goes through the apiserver's kubelet connection and keeps failing for minutes after
# a worker is stopped and started again, while the pod itself is Ready and serving.
helper_pods_ready() {
  local pods pod ready status deletion
  local -a pod_names
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" -o json 2> /dev/null |
    jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name] | join(" ")')" || return 1
  read -r -a pod_names <<< "${pods}"
  [ "${#pod_names[@]}" -eq "${HELPER_REPLICAS}" ] || return 1
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
#
# A pod that is already terminating does not count: when its node is stopped, its
# kubelet cannot run the app's label cleanup, so the old leader keeps the label while
# it lingers in Terminating, and the surviving pod takes over the Lease in the meantime.
current_leader_pod() {
  local pods pod
  local -a pod_names
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${LEADER_SELECTOR}" -o json 2> /dev/null |
    jq -r '[.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name] | join(" ")')" || return 1
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
    -o jsonpath='{.spec.holderIdentity}' 2> /dev/null)" || return 1
  [ -n "${holder}" ] || return 1
  # The labelled pod's own log records the leader id it generated. `kubectl logs`
  # goes through the apiserver's kubelet connection, which keeps failing for
  # minutes after a worker is stopped and started again, so the id is compared only
  # when the log can be read: the leader label and the endpoint that points at that
  # pod already prove which pod is serving.
  generated="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${LEADER_POD}" 2> /dev/null |
    grep -oE 'generated leader id: [0-9a-f-]+' | awk '{print $4}' | tail -n 1)" || true
  if [ -n "${generated}" ]; then
    [ "${generated}" = "${holder}" ] || return 1
  fi
  endpoint_ips="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints "${METRICS_SERVICE}" \
    -o jsonpath='{.subsets[*].addresses[*].ip}' 2> /dev/null)" || return 1
  read -r -a endpoint_addresses <<< "${endpoint_ips}"
  [ "${#endpoint_addresses[@]}" -eq 1 ] || return 1
  pod_ip="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${LEADER_POD}" \
    -o jsonpath='{.status.podIP}' 2> /dev/null)" || return 1
  [ "${endpoint_addresses[0]}" = "${pod_ip}" ] || return 1
  LEADER_ID="${holder}"
  if [ -n "${PREDICATE_LEADER_STATE:-}" ]; then
    printf '%s\t%s\n' "${LEADER_POD}" "${LEADER_ID}" > "${PREDICATE_LEADER_STATE}" || return 1
  fi
  return 0
}

# Read one initialized API object. Both counters and the empty allocation map
# use omitempty in the production API; an absent key is not a failed GET.
pool_snapshot() { # <pool> -> validated {used,available,allocated,capacity}
  local object
  object="$(kubectl get ippool "$1" -o json)" || return 1
  jq -ce '
    def count_value($key):
      (if has($key) then .[$key] else 0 end)
      | if type == "number" and . >= 0 and . == floor then .
        else error("invalid IPPool counter") end;
    def ip_number:
      if type != "string" then error("invalid IPPool address") else . end
      | split(".")
      | if length != 4 or any(.[]; test("^[0-9]+$") | not)
        then error("invalid IPPool address") else map(tonumber) end
      | if any(.[]; . < 0 or . > 255) then error("invalid IPPool address") else . end
      | .[0] * 16777216 + .[1] * 65536 + .[2] * 256 + .[3];
    if (.status.lastupdate | type) != "string" or .status.lastupdate == ""
       or (.status.ipv4 | type) != "object"
    then error("IPPool has not initialized") else . end
    | (.spec.ipv4config.pool.start | ip_number) as $start
    | (.spec.ipv4config.pool.end | ip_number) as $end
    | if $end < $start then error("invalid IPPool range") else . end
    | .status.ipv4
    | (count_value("used")) as $used
    | (count_value("available")) as $available
    | (if has("allocated") then .allocated else {} end) as $allocated
    | if ($allocated | type) != "object" or any($allocated[]; type != "string")
         or any($allocated | keys[];
           (ip_number) as $address | $address < $start or $address > $end)
         or ($allocated | length) != $used or $used + $available != $end - $start + 1
      then error("IPPool accounting disagrees")
      else {used:$used, available:$available, allocated:$allocated, capacity:($end-$start+1)} end
  ' <<< "${object}"
}

pool_initialized() {
  local snapshot
  snapshot="$(pool_snapshot "${KIH_IPPOOL_NAME}")" || return 1
  jq -e '.used == 0 and .available == .capacity' <<< "${snapshot}" > /dev/null
}

# Exercise the published Service from an ordinary in-cluster client. Per-pod
# localhost observations in diagnostic captures are not this delivery check.
metrics_text() {
  leader_consistent || return 1
  helper_service_metrics "${METRICS_SERVICE}"
}

# Extracts the value of exactly one exposition series: matching lines must
# start with the family name and carry every required label substring,
# exactly one line may match (duplicate series lines are a helper-side
# leak and fail the extraction), and the value must be the sole trailing
# token after the closing brace and strictly numeric.
metric_value_for() { # <scrape> <family> <label-substring> [label-substring...]
  local text="$1" matches count value sub
  shift
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
  local text used available
  text="$(metrics_text)" || return 1
  used="$(metric_value_for "${text}" kubevirtiphelper_ippool_used "ippool=\"${KIH_IPPOOL_NAME}\"" \
    "network=\"${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}\"")" || return 1
  available="$(metric_value_for "${text}" kubevirtiphelper_ippool_available "ippool=\"${KIH_IPPOOL_NAME}\"" \
    "network=\"${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}\"")" || return 1
  [ "${used}" = "$1" ] && [ "${available}" = "$2" ]
}

metric_vm_ok() {
  local text value
  text="$(metrics_text)" || return 1
  value="$(metric_value_for "${text}" kubevirtiphelper_vmnetcfg_status \
    "vm=\"${KIH_WORKLOAD_NAMESPACE}/${KIH_VM_NAME}\"" "mac=\"${KIH_VM_MAC}\"" \
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
    [ "${network}" = "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}" ] && [ "${status}" = "OK" ] &&
    vm_managed_reservation "${KIH_VM_NAME}" OK
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
  local pool snapshot
  pool="$(vmnetcfg_pool_name)" || return 1
  [ -n "${pool}" ] || return 1
  snapshot="$(pool_snapshot "${pool}")" || return 1
  jq -e --arg ip "${RESERVED_IP}" \
    --arg owner "${KIH_WORKLOAD_NAMESPACE}/${KIH_VM_NAME} [${KIH_VM_MAC}]" \
    '.allocated[$ip] == $owner' <<< "${snapshot}" > /dev/null
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


# After duplicate-config cleanup, repeated "NO LEASE FOUND" entries for the
# owner MAC show that the live owner's DHCP lease is missing. The log storm is
# symptom-based evidence of the ownership-regression/lease-loss condition,
# without assuming how production cleanup selected or removed the lease.
#
# The threshold limits transient noise while preserving a fast failure signal.
leader_log_storm_for() { # <mac> <since-minutes>
  local mac="$1" since="$2" pod count
  pod="$(current_leader_pod)" || return 1
  count="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}" --since="${since}m" 2> /dev/null |
    grep -cF "NO LEASE FOUND: hwaddr=${mac}" || true)"
  [ "${count}" -ge "${E2E_LEASE_STORM_COUNT}" ]
}

# Boot predicate for the boot that follows duplicate-config cleanup:
# the awaited marker passes normally; otherwise repeated "NO LEASE FOUND"
# entries for the owner MAC are an ownership-regression/lease-loss symptom
# and fail fast (exit code 2). The marker is checked first so observed boot
# success keeps precedence over symptom-based regression evidence.
boot_network_or_lease_loss() { # <exclusive sample sequence cutoff>
  guest_network_ready "$1" && dhcp_transaction_after 0 0 "${GUEST_LEASE}" initial && return 0
  leader_log_storm_for "${KIH_VM_MAC}" "${E2E_LEASE_STORM_WINDOW}" && return 2
  return 1
}

# One absolute budget covers a one-shot command as well as its completion.
command_before_deadline() { # <case-id> <deadline> <description> <command...>
  local id="$1" deadline="$2" description="$3" remaining
  shift 3
  report_case_start "${id}" "${description}"
  remaining=$((deadline - SECONDS))
  [ "${remaining}" -gt 0 ] || die "deadline expired: ${description}"
  timeout "${remaining}s" "$@" || die "${description}"
  [ "${SECONDS}" -lt "${deadline}" ] || die "deadline expired: ${description}"
  report_case_pass "${description}"
}

guest_identity() {
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmi "${KIH_VM_NAME}" -o json |
    jq -cer 'select(.metadata.deletionTimestamp == null and .status.phase == "Running")
      | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))
      | [.metadata.uid,.status.nodeName]
      | select(all(.[]; type == "string" and length > 0))'
}

guest_samples() {
  # Only newline-terminated records count; a writer's partial final line cannot
  # pass, and malformed complete records cannot fall back to an older success.
  jq -Rsce --arg marker "${E2E_NETWORK_MARKER}" '
    ["seq","uptime","iface","mac","address","router","dns","search",
     "client_pid","client_alive","gateway_ok","target_ip","target_via","target_ok"] as $keys
    | [split("\n")[:-1][] | sub("\r$";"")
      | select(startswith($marker + " ")) | split(" ")[1:]
      | if length != ($keys|length) then error("malformed guest sample") else . end
      | to_entries | map(.key as $i | .value
        | capture("^(?<key>[a-z_]+)=(?<value>[^ =]+)$")
        | if .key != $keys[$i] then error("unexpected guest sample field") else . end)
      | if length != ($keys|length) then error("missing guest sample field") else from_entries end
      | .seq |= (if test("^[0-9]+$") then tonumber else error("invalid sequence") end)
      | .uptime |= (if test("^[0-9]+([.][0-9]+)?$") then tonumber else -1 end)]
  ' "${GUEST_CONSOLE}"
}

guest_sample_healthy() { # <JSON sample>
  jq -e --argjson want "${GUEST_EXPECTED}" --arg mac "${KIH_VM_MAC}" '
    .uptime >= 0 and .mac == $mac and .iface != "missing"
    and (.client_pid | test("^[1-9][0-9]*$")) and .client_alive == "1"
    and .gateway_ok == "1" and .target_ok == "1"
    and .address == $want.address and .router == $want.router
    and .dns == $want.dns and .search == $want.search
    and .target_ip == $want.target_ip and .target_via == $want.router
  ' <<< "$1" > /dev/null
}

guest_sample_cutoff() {
  guest_samples | jq -er '.[-1].seq // 0'
}

guest_network_ready() { # <exclusive sample sequence cutoff>
  local cutoff="$1" records sample
  kill -0 "${CONSOLE_PID}" 2> /dev/null || return 1
  records="$(guest_samples)" || return 1
  sample="$(jq -ce --argjson cutoff "${cutoff}" \
    '.[-1] // empty | select(.seq > $cutoff)' <<< "${records}")" || return 1
  guest_sample_healthy "${sample}" || return 1
  jq -ce '{sample: .[-1], index: (length - 1)}' <<< "${records}" > "${GUEST_CONSOLE}.latest"
}

capture_streams_ready() {
  local node
  for node in "${!CAPTURE_PIDS[@]}"; do
    kill -0 "${CAPTURE_PIDS[$node]}" 2> /dev/null || return 1
    grep -q 'listening on ' "${CAPTURE_FILES[$node]}.stderr" || return 1
  done
  [ "${#CAPTURE_PIDS[@]}" -eq 3 ]
}

start_dhcp_captures() { # <label> <deadline> <bridge>
  local label="$1" deadline="$2" bridge="$3" pods node pod budget file token monitor_was_on
  [ "${#CAPTURE_PIDS[@]}" -eq 0 ] || return 1
  pods="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get pods \
    -l app=kih-network-observer -o json)" || return 1
  pods="$(jq -er '.items | select(length == 3) | .[]
    | select(.metadata.deletionTimestamp == null)
    | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))
    | [.spec.nodeName,.metadata.name] | @tsv' <<< "${pods}")" || return 1
  while IFS=$'\t' read -r node pod; do
    [[ "${node}" =~ ^[a-z0-9][a-z0-9.-]*$ ]] || return 1
    budget=$((deadline - SECONDS))
    [ "${budget}" -gt 0 ] || return 1
    file="${E2E_ARTIFACTS_DIR}/dhcp-${label}-${node}.pcap"
    token="/tmp/kih-capture-${BASHPID}-${SECONDS}"
    CAPTURE_FILES["${node}"]="${file}"
    CAPTURE_PODS["${node}"]="${pod}"
    CAPTURE_TOKENS["${node}"]="${token}"
    # The bridge must be captured promiscuously: -p would miss unicast T1
    # renewals forwarded between a VM tap and the inter-node bridge uplink.
    # Run each known local wrapper in its own process group so teardown can
    # signal both the timeout wrapper and its kubectl child.
    case $- in *m*) monitor_was_on=1 ;; *) monitor_was_on="" ;; esac
    set -m
    # shellcheck disable=SC2016
    timeout --foreground --kill-after=1s "${budget}s" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" exec "${pod}" -c observer -- \
      sh -ec '
        [ ! -e "$1" ]
        printf "%s %s\n" "$$" "$(cut -d " " -f 22 /proc/$$/stat)" > "$1"
        exec timeout -s INT -k 1 "$2" tcpdump -U -n -i "$3" -s 0 -w - \
          "udp port 67 or udp port 68"
      ' sh "${token}" "${budget}" "${bridge}" > "${file}" 2> "${file}.stderr" &
    CAPTURE_PIDS["${node}"]=$!
    [ -n "${monitor_was_on}" ] || set +m
  done <<< "${pods}"
}

# The wrapper PIDs below are created by this shell with job control enabled, so
# each is the leader of an owned process group. Check that group first, while
# also addressing the known wrapper PID in case it has not yet exec'd.
owned_wrapper_group_running() { # <known wrapper PID>
  kill -0 -- "-$1" > /dev/null 2>&1
}

owned_wrapper_reapable() { # <known wrapper PID>
  [ ! -d "/proc/$1" ] && return 0
  [ "$(cut -d ' ' -f 3 "/proc/$1/stat" 2> /dev/null)" = Z ]
}

sleep_before_deadline() { # <absolute SECONDS deadline> <maximum seconds>
  local remaining=$(( $1 - SECONDS )) duration="$2"
  [ "${remaining}" -gt 0 ] || return 1
  [ "${duration}" -le "${remaining}" ] || duration="${remaining}"
  sleep "${duration}"
}

terminate_owned_wrapper() { # <known wrapper PID> <absolute SECONDS deadline>
  local pid="$1" deadline="$2" i
  kill -TERM -- "-${pid}" > /dev/null 2>&1 || true
  kill -TERM "${pid}" > /dev/null 2>&1 || true
  for i in 1 2 3 4 5; do
    owned_wrapper_reapable "${pid}" && return 0
    sleep_before_deadline "${deadline}" 1 || break
  done
  owned_wrapper_reapable "${pid}" && return 0
  kill -KILL -- "-${pid}" > /dev/null 2>&1 || true
  kill -KILL "${pid}" > /dev/null 2>&1 || true
  for i in 1 2 3 4 5; do
    owned_wrapper_reapable "${pid}" && return 1
    sleep_before_deadline "${deadline}" 1 || break
  done
  return 1
}

await_owned_group_gone() { # <known wrapper PID> <absolute SECONDS deadline>
  local pid="$1" deadline="$2" i
  for i in 1 2 3 4 5; do
    owned_wrapper_group_running "${pid}" || return 0
    sleep_before_deadline "${deadline}" 1 || break
  done
  ! owned_wrapper_group_running "${pid}"
}

wait_reaped_wrapper() { # <known wrapper PID>
  owned_wrapper_reapable "$1" || return 1
  wait "$1"
}

stop_dhcp_capture() { # <node> <absolute SECONDS deadline>
  local node="$1" deadline="$2" pid="${CAPTURE_PIDS[$1]}" rc=0 i budget
  # A stale PID file never authorizes killing a reused process.
  budget=$((deadline - SECONDS))
  if [ "${budget}" -gt 0 ]; then
    [ "${budget}" -le 10 ] || budget=10
    # shellcheck disable=SC2016
    timeout --foreground --kill-after=1s "${budget}s" kubectl -n "${KIH_WORKLOAD_NAMESPACE}" exec "${CAPTURE_PODS[$node]}" -c observer -- \
      sh -ec '
        [ -e "$1" ] || exit 0
        read -r pid born < "$1"
        if [ -d "/proc/$pid" ]; then
          [ "$(cut -d " " -f 22 "/proc/$pid/stat")" = "$born" ]
          case "$(tr "\000" " " < "/proc/$pid/cmdline")" in
            *tcpdump*|*timeout*) kill -INT "$pid" ;;
            *) exit 1 ;;
          esac
        fi
        rm "$1"
      ' sh "${CAPTURE_TOKENS[$node]}" || rc=1
  else
    rc=1
  fi
  for i in 1 2 3 4 5; do
    owned_wrapper_reapable "${pid}" && break
    sleep_before_deadline "${deadline}" 1 || break
  done
  if ! owned_wrapper_reapable "${pid}"; then
    terminate_owned_wrapper "${pid}" "${deadline}" || true
    rc=1
  fi
  wait_reaped_wrapper "${pid}" || rc=1
  if owned_wrapper_group_running "${pid}"; then
    kill -KILL -- "-${pid}" > /dev/null 2>&1 || true
    await_owned_group_gone "${pid}" "${deadline}" || rc=1
    rc=1
  fi
  budget=$((deadline - SECONDS))
  if [ "${budget}" -gt 0 ]; then
    [ "${budget}" -le 20 ] || budget=20
    timeout --foreground --kill-after=1s "${budget}s" python3 "${E2E_DIR}/dhcp.py" "${CAPTURE_FILES[$node]}" \
      > "${CAPTURE_FILES[$node]}.jsonl" 2> "${CAPTURE_FILES[$node]}.decode-errors" || rc=1
  else
    rc=1
  fi
  unset 'CAPTURE_PIDS[$node]' 'CAPTURE_PODS[$node]' 'CAPTURE_TOKENS[$node]'
  return "${rc}"
}

finish_guest_evidence() { # <absolute SECONDS deadline>
  local deadline="$1" node rc=0
  for node in "${!CAPTURE_PIDS[@]}"; do
    stop_dhcp_capture "${node}" "${deadline}" || rc=1
  done
  if [ -n "${CONSOLE_PID}" ]; then
    terminate_owned_wrapper "${CONSOLE_PID}" "${deadline}" || rc=1
    if owned_wrapper_reapable "${CONSOLE_PID}"; then
      wait_reaped_wrapper "${CONSOLE_PID}" 2> /dev/null || true
    else
      rc=1
    fi
    if owned_wrapper_group_running "${CONSOLE_PID}"; then
      kill -KILL -- "-${CONSOLE_PID}" > /dev/null 2>&1 || true
      await_owned_group_gone "${CONSOLE_PID}" "${deadline}" || rc=1
    fi
  fi
  if [ -n "${CONSOLE_FEEDER_PID}" ]; then
    terminate_owned_wrapper "${CONSOLE_FEEDER_PID}" "${deadline}" || rc=1
    if owned_wrapper_reapable "${CONSOLE_FEEDER_PID}"; then
      wait_reaped_wrapper "${CONSOLE_FEEDER_PID}" 2> /dev/null || true
    else
      rc=1
    fi
    if owned_wrapper_group_running "${CONSOLE_FEEDER_PID}"; then
      kill -KILL -- "-${CONSOLE_FEEDER_PID}" > /dev/null 2>&1 || true
      await_owned_group_gone "${CONSOLE_FEEDER_PID}" "${deadline}" || rc=1
    fi
  fi
  [ -z "${CONSOLE_FIFO}" ] || rm -f "${CONSOLE_FIFO}"
  CONSOLE_PID="" CONSOLE_FEEDER_PID="" CONSOLE_FIFO=""
  return "${rc}"
}

refresh_dhcp_events() {
  [ -n "${GUEST_NODE}" ] || return 1
  kill -0 "${CAPTURE_PIDS[$GUEST_NODE]}" 2> /dev/null || return 1
  python3 "${E2E_DIR}/dhcp.py" --allow-incomplete "${CAPTURE_FILES[$GUEST_NODE]}" \
    > "${GUEST_EVENTS}.tmp" 2> "${GUEST_EVENTS}.decode-errors" || return 1
  mv "${GUEST_EVENTS}.tmp" "${GUEST_EVENTS}"
}

dhcp_transaction_after() { # <event ordinal> <epoch> <lease seconds> <initial|renewal>
  refresh_dhcp_events || return 1
  jq -se --argjson cutoff "$1" --argjson epoch "$2" --argjson lease "$3" \
    --arg mode "$4" --arg mac "${KIH_VM_MAC}" --arg ip "${RESERVED_IP}" \
    --argjson want "${GUEST_EXPECTED}" '
    .[$cutoff:] | map(select(.mac == $mac and .time >= $epoch)) as $events
    | any(range(0; $events|length);
      . as $i | $events[$i] as $request
      | $request.message == "REQUEST"
      and $request.ciaddr == (if $mode == "renewal" then $ip else "0.0.0.0" end)
      and any($events[($i+1):][];
        .message == "ACK" and .xid == $request.xid and .time >= $request.time
        and .yiaddr == $ip and .lease_seconds == $lease
        and .subnet == $want.subnet and .routers == [$want.router]
        and .dns == ($want.dns | split(",")) and .server_id == $want.server_id))
  ' "${GUEST_EVENTS}" > /dev/null
}

snapshot_guest_continuity() {
  # The caller's readiness phase has already validated and saved a fresh sample;
  # consume its immutable ordinal without demanding a second observer record.
  local latest
  latest="$(jq -ce 'select((.sample | type) == "object"
    and (.index | type) == "number" and .index >= 0 and .index == (.index | floor))' \
    "${GUEST_CONSOLE}.latest")" || return 1
  GUEST_BASELINE="$(jq -ce '.sample' <<< "${latest}")" || return 1
  GUEST_BASELINE_INDEX="$(jq -er '.index' <<< "${latest}")" || return 1
  GUEST_IDENTITY="$(guest_identity)" || return 1
  refresh_dhcp_events || return 1
  GUEST_EVENT_CUTOFF="$(wc -l < "${GUEST_EVENTS}")"
  GUEST_ACTION_EPOCH="$(date +%s.%N)"
}

guest_continuity_after() { # <post-action sample sequence>
  local identity samples sample
  identity="$(guest_identity)" || return 1
  [ "${identity}" = "${GUEST_IDENTITY}" ] || return 1
  kill -0 "${CONSOLE_PID}" 2> /dev/null || return 1
  samples="$(guest_samples | jq -ce --argjson base "${GUEST_BASELINE}" \
    --argjson baseline_index "${GUEST_BASELINE_INDEX}" --argjson cutoff "$1" '
    . as $all
    | select($baseline_index >= 0 and $baseline_index < ($all | length))
    | select($all[$baseline_index] == $base)
    | $all[$baseline_index:] as $samples
    | select(($samples|length) >= 3 and $samples[-1].seq > $cutoff + 1)
    | select(all(range(0;$samples|length);
        . as $i | $samples[$i].seq >= $base.seq
        and ($i == 0 or $samples[$i].seq > $samples[$i-1].seq)
        and $samples[$i].iface == $base.iface
        and $samples[$i].client_pid == $base.client_pid
        and ($i == 0 or ($samples[$i].uptime > $samples[$i-1].uptime
          and $samples[$i].uptime - $samples[$i-1].uptime <= 20))))
    | $samples' )" || return 1
  while IFS= read -r sample; do
    guest_sample_healthy "${sample}" || return 1
  done < <(jq -c '.[]' <<< "${samples}")
}

start_guest_and_assert() { # <label> [absolute SECONDS deadline]
  local label="$1" pool config bridge target boot_deadline budget node sample_cutoff monitor_was_on
  GUEST_DEADLINE="${2:-$((SECONDS + E2E_VM_BOOT_TIMEOUT + 180))}"
  boot_deadline=$((SECONDS + E2E_VM_BOOT_TIMEOUT))
  [ "${boot_deadline}" -le "${GUEST_DEADLINE}" ] || boot_deadline="${GUEST_DEADLINE}"
  pool="$(vmnetcfg_pool_name)"
  config="$(kubectl get ippool "${pool}" -o json)"
  case "${pool}" in
    "${KIH_IPPOOL_NAME}") bridge="${KIH_BRIDGE_NAME}"; target="${KIH_PROBE_IP}" ;;
    e2e-pool-second) bridge="${KIH_SECOND_BRIDGE_NAME}"; target="${KIH_SECOND_PROBE_IP}" ;;
    *) die "no external network fixture for IPPool ${pool}" ;;
  esac
  GUEST_EXPECTED="$(python3 -c '
import ipaddress, json, sys
cfg=json.load(sys.stdin)["spec"]["ipv4config"]
net=ipaddress.IPv4Network(cfg["subnet"])
print(json.dumps(dict(address=sys.argv[1]+"/"+str(net.prefixlen),
    subnet=str(net.netmask), router=cfg["router"], dns=",".join(cfg["dns"]),
    search=cfg["domainname"], target_ip=sys.argv[2], server_id=cfg["serverip"])))
' "${RESERVED_IP}" "${target}" <<< "${config}")"
  GUEST_LEASE="$(jq -er '.spec.ipv4config.leasetime' <<< "${config}")"
  GUEST_CONSOLE="${E2E_ARTIFACTS_DIR}/console-${label}.log"
  GUEST_EVENTS="${E2E_ARTIFACTS_DIR}/dhcp-${label}.jsonl"
  guard_case "BOOT-${label}-CAPTURE-START" "passive captures start before the guest" \
    start_dhcp_captures "${label}" "${GUEST_DEADLINE}" "${bridge}"
  wait_before_deadline "BOOT-${label}-CAPTURE-READY" "${boot_deadline}" 30 \
    "all node bridges are being recorded before DHCP" capture_streams_ready
  command_before_deadline "BOOT-${label}-START" "${boot_deadline}" "guest start accepted (${label})" \
    "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" start "${KIH_VM_NAME}"
  wait_before_deadline "BOOT-${label}-VMI" "${boot_deadline}" 60 "VMI created (${label})" vmi_exists
  budget=$((GUEST_DEADLINE - SECONDS))
  [ "${budget}" -gt 0 ] || die "guest observation deadline expired"
  console_deadline=$((SECONDS + budget))
  case $- in *m*) monitor_was_on=1 ;; *) monitor_was_on="" ;; esac
  set -m
  # virtctl drops the console websocket when the apiserver's connection to the
  # node's kubelet is recycled, which happens when a worker is stopped and started
  # again. The guest keeps emitting its samples, so reattach and keep appending to
  # the same console file instead of freezing the evidence stream for the rest of the
  # run. Samples carry the guest's own sequence numbers, so a reattach is seamless.
  #
  # Each attempt gets its own stdin keeper: virtctl exits on stdin EOF, and a keeper
  # that a previous attempt left behind would make this attempt block on it forever.
  CONSOLE_FIFO="${GUEST_CONSOLE}.stdin"
  : > "${GUEST_CONSOLE}"
  (
    set +m
    while :; do
      remaining=$((console_deadline - SECONDS))
      [ "${remaining}" -gt 0 ] || break
      mkfifo "${CONSOLE_FIFO}"
      tail -f /dev/null > "${CONSOLE_FIFO}" &
      CONSOLE_FEEDER_PID=$!
      timeout --foreground --kill-after=1s "${remaining}s" "${VIRTCTL}" \
        -n "${KIH_WORKLOAD_NAMESPACE}" console "${KIH_VM_NAME}" \
        --timeout="$(((remaining+59)/60))" < "${CONSOLE_FIFO}" >> "${GUEST_CONSOLE}" 2>&1 || true
      kill "${CONSOLE_FEEDER_PID}" 2> /dev/null || true
      wait "${CONSOLE_FEEDER_PID}" 2> /dev/null || true
      rm -f "${CONSOLE_FIFO}"
      [ "${SECONDS}" -lt "${console_deadline}" ] || break
      sleep 1
    done
  ) &
  CONSOLE_PID=$!
  [ -n "${monitor_was_on}" ] || set +m
  wait_before_deadline "BOOT-${label}-READY" "${boot_deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
    "guest is Running and Ready (${label})" guest_identity
  GUEST_IDENTITY="$(guest_identity)"
  GUEST_NODE="$(jq -r '.[1]' <<< "${GUEST_IDENTITY}")"
  [ -n "${CAPTURE_PIDS[$GUEST_NODE]:-}" ] || die "guest node was not recorded before boot"
  for node in "${!CAPTURE_PIDS[@]}"; do
    [ "${node}" = "${GUEST_NODE}" ] || guard_case "BOOT-${label}-CAPTURE-${node}-STOP" \
      "unused node capture closes without lost or malformed records" \
      stop_dhcp_capture "${node}" "${GUEST_DEADLINE}"
  done
  if [ "${label}" = duplicate-owner-after-delete ]; then
    sample_cutoff="$(guest_sample_cutoff)" || die "cannot capture guest sample cutoff"
    wait_before_deadline "BOOT-${label}-DHCP" "${boot_deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
      "original owner still receives DHCP after duplicate deletion" \
      boot_network_or_lease_loss "${sample_cutoff}"
  else
    wait_before_deadline "BOOT-${label}-DHCP" "${boot_deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
      "fresh REQUEST/ACK carries the configured address, mask, router, DNS and lease" \
      dhcp_transaction_after 0 0 "${GUEST_LEASE}" initial
  fi
  sample_cutoff="$(guest_sample_cutoff)" || die "cannot capture guest sample cutoff"
  wait_before_deadline "BOOT-${label}-NETWORK" "${boot_deadline}" "${E2E_VM_BOOT_TIMEOUT}" \
    "native client installs its network and reaches DNS, gateway and routed target" \
    guest_network_ready "${sample_cutoff}"
  guard_case "BOOT-${label}-BASELINE" "live guest identity and client baseline recorded" snapshot_guest_continuity
}

finish_guest_evidence_before_deadline() { # <absolute SECONDS deadline>
  finish_guest_evidence "$1" && test "${SECONDS}" -lt "$1"
}

stop_guest() { # <label> [deadline] [reservation-predicate [args...]]
  local label="$1" deadline="${GUEST_DEADLINE}" predicate=reservation_stable
  shift
  if [[ "${1:-}" =~ ^[0-9]+$ ]]; then deadline="$1"; shift; fi
  if [ "$#" -gt 0 ]; then predicate="$1"; shift; fi
  command_before_deadline "STOP-${label}-SUBMITTED" "${deadline}" "guest stop accepted (${label})" \
    "${VIRTCTL}" -n "${KIH_WORKLOAD_NAMESPACE}" stop "${KIH_VM_NAME}"
  wait_before_deadline "STOP-${label}-VM-GONE" "${deadline}" 120 "VMI stopped" vmi_absent
  guard_case "STOP-${label}-EVIDENCE" "console and packet records close cleanly before the parent deadline" \
    finish_guest_evidence_before_deadline "${deadline}"
  wait_before_deadline "STOP-${label}-RESERVATION-STABLE" "${deadline}" 60 \
    "reservation stable while halted" "${predicate}" "$@"
}

reload_snapshot() { # <log marker> -> pod<TAB>uid<TAB>count
  local pods pod uid logs count
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" -o json)" || return 1
  while IFS=$'\t' read -r pod uid; do
    logs="$(kubectl -n "${KIH_HELPER_NAMESPACE}" logs "${pod}")" || return 1
    count="$(grep -cF "$1" <<< "${logs}" || true)"
    printf '%s\t%s\t%s\n' "${pod}" "${uid}" "${count}"
  done < <(jq -r '.items[] | [.metadata.name,.metadata.uid] | @tsv' <<< "${pods}")
}

reload_processed() { # <baseline snapshot> <log marker>
  local current pod uid count old_pod old_uid old_count
  current="$(reload_snapshot "$2")" || return 1
  while IFS=$'\t' read -r pod uid count; do
    while IFS=$'\t' read -r old_pod old_uid old_count; do
      if [ "${pod}" = "${old_pod}" ] && [ "${uid}" = "${old_uid}" ] &&
        [ "${count}" -gt "${old_count}" ]; then return 0; fi
    done <<< "$1"
  done <<< "${current}"
  return 1
}

new_leader_elected() { # <old pod> <old id>
  local old_pod="$1" old_id="$2" reason="" pods labels endpoints
  if ! leader_consistent; then
    reason="leader state inconsistent"
  elif [ "${LEADER_POD}" = "${old_pod}" ]; then
    reason="labelled leader is still ${old_pod}"
  elif [ "${LEADER_ID}" = "${old_id}" ]; then
    reason="lease holder is still ${old_id}"
  fi
  if [ -n "${reason}" ]; then
    # Record why the wait has not succeeded yet, with the object state that explains
    # it, so a slow CI runner does not have to be guessed at from the final state.
    pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" -o json 2> /dev/null |
      jq -c '[.items[] | {name: .metadata.name, node: .spec.nodeName,
        ready: ([.status.conditions[]? | select(.type == "Ready") | .status]),
        leader: .metadata.labels["kubevirtiphelper/leader"],
        deleting: (.metadata.deletionTimestamp != null)}]')" || pods="?"
    labels="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${LEADER_SELECTOR}" \
      -o jsonpath='{.items[*].metadata.name}' 2> /dev/null)" || labels="?"
    endpoints="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints "${METRICS_SERVICE}" \
      -o jsonpath='{.subsets[*].addresses[*].ip}' 2> /dev/null)" || endpoints="?"
    printf '%s\t%s\tleaders=[%s] endpoints=[%s] pods=%s\n' \
      "$(date -u +%H:%M:%S)" "${reason}" "${labels}" "${endpoints}" "${pods}" \
      >> "${E2E_ARTIFACTS_DIR}/ha-transfer-diagnostic.txt" 2> /dev/null || true
    return 1
  fi
  return 0
}

cleanup_complete() {
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" \
    get vmnetcfg "${KIH_VM_NAME}" || return 1
  pool_initialized
}

pool_counts_equal() { # <pool> <used> <available>
  local snapshot
  snapshot="$(pool_snapshot "$1")" || return 1
  jq -e --argjson used "$2" --argjson available "$3" \
    '.used == $used and .available == $available' <<< "${snapshot}" > /dev/null
}

vmnetcfg_status_is() { # <name> <status>
  [ "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" \
    -o jsonpath='{.status.networkconfig[0].status}' 2> /dev/null)" = "$2" ]
}

vm_managed_reservation() { # <name> <status>
  local vm config
  vm="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vm "$1" -o json)" || return 1
  config="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" -o json)" || return 1
  jq -e -n --arg name "$1" --arg status "$2" --argjson vm "${vm}" --argjson config "${config}" '
    ($vm.spec.template.spec.domain.devices.interfaces // []) as $interfaces
    | ($vm.spec.template.spec.networks // []) as $networks
    | [ $networks[]?
        | select(.multus? | type == "object" and
            (.networkName | type == "string" and length > 0))
        | . as $network
        | ($interfaces[]?
          | select(.name == $network.name and
              (.macAddress | type == "string" and length > 0))
          | {mac: .macAddress, network: $network.multus.networkName})
      ] as $multus_nics
    | ($config.spec.networkconfig // []) as $networkconfig
    | ($config.status.networkconfig // []) as $statusconfig
    | (
        $vm.metadata.name == $name
        and $config.metadata.name == $name
        and $config.spec.vmname == $name
        and ($multus_nics | length) == 1
        and ($networkconfig | length) == 1
        and $networkconfig[0].macaddress == $multus_nics[0].mac
        and $networkconfig[0].networkname == $multus_nics[0].network
        and ($statusconfig | length) == 1
        and $statusconfig[0].status == $status
        and (($config.metadata.finalizers // []) | type == "array"
          and index("kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup") != null)
      )
  ' > /dev/null
}

duplicate_mac_refused() { # <baseline snapshot> <duplicate rejection marker>
  vmnetcfg_absent_named pool-vm-duplicate || return 1
  reload_processed "$1" "$2"
}

# Named counterpart of pool_allocation_matches: a refused duplicate claim must
# leave the original owner's address and accounting entry untouched.
named_reservation_kept() { # <name> <mac>
  local ip snapshot
  ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')" || return 1
  [ -n "${ip}" ] || return 1
  vmnetcfg_status_is "$1" OK || return 1
  vm_managed_reservation "$1" OK || return 1
  snapshot="$(pool_snapshot "${KIH_IPPOOL_NAME}")" || return 1
  jq -e --arg ip "${ip}" --arg owner "${KIH_WORKLOAD_NAMESPACE}/${1} [${2}]" \
    '.allocated[$ip] == $owner' <<< "${snapshot}" > /dev/null
}

vmnetcfg_absent_named() { # <name>
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "$1"
}
vm_absent_named() { # <name>
  object_absent_not_found -n "${KIH_WORKLOAD_NAMESPACE}" get vm "$1"
}
# KubeVirt caps an inline cloudInitNoCloud userData at 2048 bytes, so the guest
# observer script travels as the `userdata` key of a Secret and manifests/vm.yaml
# references it through cloudInitNoCloud.secretRef. The Secret is created before any
# group applies a VM: every group runs this core lifecycle first. The applied bytes are
# compared with the pinned file, because the guest executes the Secret, not the file.
guest_userdata_secret_matches() {
  local script="${E2E_DIR}/manifests/${KIH_GUEST_USERDATA_FILE}"
  [ "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get secret "${KIH_GUEST_USERDATA_SECRET}" \
    -o jsonpath='{.data.userdata}' | base64 -d | sha256sum | awk '{print $1}')" = \
    "$(sha256sum "${script}" | awk '{print $1}')" ]
}

ensure_guest_userdata_secret() {
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" create secret generic "${KIH_GUEST_USERDATA_SECRET}" \
    --from-file=userdata="${E2E_DIR}/manifests/${KIH_GUEST_USERDATA_FILE}" \
    --dry-run=client -o yaml |
    kubectl apply -f - > /dev/null
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
    -e "s|networkName: ${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}|networkName: ${KIH_HELPER_NAMESPACE}/${KIH_SECOND_NAD_NAME}|" \
    -e "s|${KIH_HELPER_INTERFACE}|${KIH_SECOND_HELPER_INTERFACE}|g" \
    -e "s|${KIH_GUEST_IMAGE_TEMPLATE}|${KIH_GUEST_IMAGE}|" \
    "${E2E_DIR}/manifests/vm.yaml" > "${output}"
}

pool_group_allocations_ready() {
  local i name ip all_ips=""
  for i in $(seq 1 11); do
    name="$(printf 'pool-vm-%02d' "${i}")"
    vm_managed_reservation "${name}" OK || return 1
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
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-duplicate pool-vm-reclaim \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg \
    pool-vm-outside \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
}
cleanup_stale_expanded_resources() {
  cleanup_pool_group
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm \
    multipool-vm multipool-guest multipool-shared multipool-primary-guest --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg \
    multipool-vm multipool-guest multipool-shared multipool-primary-guest --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl delete ippool e2e-pool-second \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete network-attachment-definition \
    "${KIH_SECOND_NAD_NAME}" --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete deployment "${KIH_SECOND_HELPER_DEPLOYMENT}" \
    --ignore-not-found --wait=true --timeout=120s > /dev/null
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete service "${KIH_SECOND_METRICS_SERVICE}" \
    --ignore-not-found > /dev/null
}


run_pool_group() {
  report_group pool
  local deadline i name mac manifest refused_ip reclaim_ip old_vm old_mac duplicate_before duplicate_marker
  deadline=$((SECONDS + 720))
  SCENARIO_DEADLINE="${deadline}"
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
    vm_managed_reservation pool-vm-12 ERROR
  wait_before_deadline POOL-REFUSAL-ACCOUNTING "${deadline}" 60 "refusal leaves pool accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="pool-vm-01"
  KIH_VM_MAC="02:00:00:00:01:01"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert exhausted-pool "${deadline}"
  stop_guest exhausted-pool "${deadline}" named_reservation_kept "${KIH_VM_NAME}" "${KIH_VM_MAC}"
  wait_before_deadline POOL-RESERVATION-RETAINED "${deadline}" 60 "served reservation remains allocated" \
    vmnetcfg_status_is "${KIH_VM_NAME}" OK
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-12 --wait=true --timeout=120s
  wait_before_deadline POOL-TWELFTH-CLEANED "${deadline}" 90 \
    "refused twelfth VM and its managed reservation are removed before a slot opens" \
    vmnetcfg_absent_named pool-vm-12
  wait_before_deadline POOL-TWELFTH-CLEANUP-ACCOUNTING "${deadline}" 60 \
    "removing the refused twelfth VM preserves full-pool accounting" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  log "group pool: refusing a duplicate MAC without consuming another address"
  capture_checkpoint 21-duplicate-owner-before "pool filled with pool-vm-01 holding its original address"
  render_halted_vm pool-vm-duplicate 02:00:00:00:01:01 \
    "${E2E_ARTIFACTS_DIR}/13-duplicate-mac.yaml"
  duplicate_marker='belongs to e2e/pool-vm-01 instead of e2e/pool-vm-duplicate'
  duplicate_before="$(reload_snapshot "${duplicate_marker}")"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/13-duplicate-mac.yaml" > /dev/null
  wait_before_deadline POOL-DUPLICATE-REFUSED "${deadline}" 90 \
    "the VM controller refuses the duplicate before creating a reservation" \
    duplicate_mac_refused "${duplicate_before}" "${duplicate_marker}"
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
  start_guest_and_assert duplicate-owner "${deadline}"
  stop_guest duplicate-owner "${deadline}"
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-duplicate \
    --wait=true --timeout=120s
  wait_before_deadline POOL-DUPLICATE-VM-CLEANED "${deadline}" 90 \
    "refused duplicate VM is removed" vm_absent_named pool-vm-duplicate
  wait_before_deadline POOL-DUPLICATE-DELETE-ACCOUNTING "${deadline}" 60 \
    "accounting still shows the eleven original reservations" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0
  # Deleting a refused duplicate must not remove the live owner's DHCP lease.
  # Reboot the original VM after the duplicate VM is gone so cleanup
  # cannot silently make the reservation unreachable.
  old_vm="${KIH_VM_NAME}"
  old_mac="${KIH_VM_MAC}"
  KIH_VM_NAME="pool-vm-01"
  KIH_VM_MAC="02:00:00:00:01:01"
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert duplicate-owner-after-delete "${deadline}"
  stop_guest duplicate-owner-after-delete "${deadline}"
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"
  capture_checkpoint 22-duplicate-owner-after \
    "refused duplicate VM gone with pool-vm-01 still holding its address"

  reclaim_ip="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-11 \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm pool-vm-11 --wait=true --timeout=120s
  wait_before_deadline POOL-DELETE-RELEASES "${deadline}" 90 "deleted VM releases its reservation" \
    vmnetcfg_absent_named pool-vm-11
  wait_before_deadline POOL-CAPACITY-RESTORED "${deadline}" 60 "released address returns to capacity" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 10 1
  refused_ip="${KIH_IPPOOL_START%.*}.99"
  cat > "${E2E_ARTIFACTS_DIR}/15-out-of-range-vmnetcfg.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: VirtualMachineNetworkConfig
metadata:
  name: pool-vm-outside
  namespace: ${KIH_WORKLOAD_NAMESPACE}
spec:
  vmname: pool-vm-outside
  networkconfig:
    - macaddress: "02:00:00:00:01:fe"
      networkname: "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}"
      ipaddress: "${refused_ip}"
EOF
  assert_case POOL-OUT-OF-RANGE-REFUSED \
    "admission rejects the out-of-range record explicitly" admission_rejects \
    "${E2E_ARTIFACTS_DIR}/15-out-of-range-vmnetcfg.yaml"
  assert_case POOL-OUT-OF-RANGE-ABSENT \
    "denied out-of-range request leaves no VMNetCfg" vmnetcfg_absent_named pool-vm-outside
  wait_before_deadline POOL-OUT-OF-RANGE-ACCOUNTING "${deadline}" 60 \
    "out-of-range refusal leaves accounting unchanged" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 10 1
  wait_before_deadline POOL-HEALTH-AFTER-REFUSALS "${deadline}" 60 \
    "helper remains healthy after refusals" leader_services_healthy
  capture_checkpoint 28-pool-refusals-held \
    "out-of-range request refused with accounting at 10 used 1 available before normal reclaim"

  render_halted_vm pool-vm-reclaim 02:00:00:00:01:ee \
    "${E2E_ARTIFACTS_DIR}/14-reclaim-vm.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/14-reclaim-vm.yaml" > /dev/null
  wait_before_deadline POOL-VM-RECLAIM "${deadline}" 90 \
    "a new VM reclaims the only free address through a helper-created record" \
    vm_managed_reservation pool-vm-reclaim OK
  assert_case POOL-VM-RECLAIM-ADDRESS "new VM received released address ${reclaim_ip}" \
    test "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg pool-vm-reclaim \
      -o jsonpath='{.spec.networkconfig[0].ipaddress}')" = "${reclaim_ip}"
  wait_before_deadline POOL-RECLAIM-COUNTS "${deadline}" 60 "reclaim fills the pool again" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 11 0

  cleanup_pool_group
  wait_before_deadline POOL-CLEANUP-CAPACITY "${deadline}" 180 \
    "pool group cleanup returns exact capacity" pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  capture_checkpoint 12-pool-cleaned "pool group cleanup restored exact capacity"
  guard_case POOL-DEADLINE "pool scenarios completed within their original deadline" \
    test "${SECONDS}" -lt "${deadline}"
  SCENARIO_DEADLINE=0
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

worker_is_stopped() {
  local object
  object="$(worker_record "${STOPPED_WORKER}")" || return 1
  jq -e '.running == false' <<< "${object}" > /dev/null || return 1
  kubectl get node "${STOPPED_WORKER}" -o json |
    jq -e 'any(.status.conditions[]; .type == "Ready" and (.status == "False" or .status == "Unknown"))' > /dev/null
}

worker_has_recovered() {
  kubectl get node "${STOPPED_WORKER}" -o json |
    jq -e 'any(.status.conditions[]; .type == "Ready" and .status == "True")' > /dev/null || return 1
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get pods -l "${KIH_OBSERVER_SELECTOR}" \
    --field-selector "spec.nodeName=${STOPPED_WORKER}" -o json |
    jq -e '[.items[] | select(.metadata.deletionTimestamp == null)
      | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))]
      | length == 1' > /dev/null
}

run_ha_group() {
  report_group ha
  local deadline old_leader old_id follower follower_uid pods old_uids workers fault_node survivor placement record cutoff
  # A stopped worker cannot deliver SIGTERM, so the old leader never releases the
  # Lease, the survivor waits out the full lease duration, and the restored worker
  # evicts and replaces its pod before two live replicas are Ready again. The group
  # budget has to cover that whole chain.
  deadline=$((SECONDS + 1200))
  SCENARIO_DEADLINE="${deadline}"
  log "group ha: follower churn"
  assert_case HA-LEADER-BEFORE-CHURN "leader state consistent before HA group" leader_consistent
  fault_node="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${LEADER_POD}" -o jsonpath='{.spec.nodeName}')"
  workers="$(kubectl get nodes -o json | jq -ce '[.items[]
    | select((.metadata.labels|has("node-role.kubernetes.io/control-plane")|not)
      and (.metadata.labels|has("node-role.kubernetes.io/master")|not))]
    | select(length == 2)')"
  jq -e --arg node "${fault_node}" 'any(.[]; .metadata.name == $node)' <<< "${workers}" > /dev/null
  survivor="$(jq -er --arg node "${fault_node}" '.[] | select(.metadata.name != $node) | .metadata.name' <<< "${workers}")"
  placement="$(jq -er --arg node "${survivor}" '.[] | select(.metadata.name == $node)
    | .metadata.labels["kubernetes.io/hostname"] | select(type == "string" and length > 0)' <<< "${workers}")"
  pods="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pods -l "${HELPER_SELECTOR}" -o json)"
  guard_case HA-SURVIVING-HELPER "normal helper placement includes a replica on the surviving worker" \
    jq -e --arg node "${survivor}" 'any(.items[]; .spec.nodeName == $node and .metadata.deletionTimestamp == null)' \
    <<< "${pods}"
  # Every later boundary has to keep a live reservation, its accounting entry,
  # and both metrics intact, so the guest is created before the first transition.
  render_halted_vm "${KIH_VM_NAME}" "${KIH_VM_MAC}" \
    "${E2E_ARTIFACTS_DIR}/20-ha-vm.yaml"
  # Only this workload's ordinary scheduling is constrained; the helper's
  # deployment and scheduling remain byte-for-byte production defaults.
  kubectl patch --local -f "${E2E_ARTIFACTS_DIR}/20-ha-vm.yaml" --type=merge \
    -p "$(jq -nc --arg node "${placement}" '{spec:{template:{spec:{nodeSelector:{"kubernetes.io/hostname":$node}}}}}')" \
    -o yaml > "${E2E_ARTIFACTS_DIR}/20-ha-vm-placed.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/20-ha-vm-placed.yaml" > /dev/null
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
  start_guest_and_assert ha-live "${deadline}"
  assert_case HA-GUEST-ON-SURVIVOR "live VM is on the selected surviving worker" test "${GUEST_NODE}" = "${survivor}"
  assert_case HA-LEADER-BEFORE-WORKER-FAULT "active helper identity remains consistent before worker loss" leader_consistent
  assert_case HA-FAULT-TARGET-STILL-ACTIVE "worker selected for shutdown still hosts the active helper" \
    test "$(kubectl -n "${KIH_HELPER_NAMESPACE}" get pod "${LEADER_POD}" -o jsonpath='{.spec.nodeName}')" = "${fault_node}"
  old_leader="${LEADER_POD}" old_id="${LEADER_ID}"
  snapshot_guest_continuity
  record="$(worker_record "${fault_node}")"
  guard_case HA-WORKER-WAS-RUNNING "fault target is an owned running kind node" \
    jq -e '.running == true' <<< "${record}"
  STOPPED_WORKER="${fault_node}"
  STOPPED_WORKER_ID="$(jq -r '.id' <<< "${record}")"
  printf '%s\n' "${record}" > "${E2E_ARTIFACTS_DIR}/ha-worker-before-stop.json"
  command_before_deadline HA-WORKER-STOP "${deadline}" "active helper worker is stopped, not paused" \
    "${RUNTIME}" stop --time=0 "${STOPPED_WORKER_ID}"
  wait_before_deadline HA-WORKER-DOWN "${deadline}" 90 "runtime and Kubernetes both observe worker loss" worker_is_stopped
  # lease duration (60s) + service rebuild + readiness probe period, and the
  # worker stop also evicts and reschedules its pod, so the surviving helper can
  # need several minutes to take the Lease and the Service endpoint.
  wait_before_deadline HA-WORKER-LEADER-TRANSFER "${deadline}" 450 "surviving helper takes the Lease and Service endpoint" \
    new_leader_elected "${old_leader}" "${old_id}"
  wait_before_deadline HA-WORKER-SERVICES "${deadline}" 90 "helper service recovers on the survivor" leader_services_healthy
  # Discard transactions from before recovery, including any ACK generated by
  # the old helper just before shutdown. The next normal renewal is the proof.
  refresh_dhcp_events
  GUEST_EVENT_CUTOFF="$(wc -l < "${GUEST_EVENTS}")"
  GUEST_ACTION_EPOCH="$(date +%s.%N)"
  wait_before_deadline HA-WORKER-LIVE-RENEWAL "${deadline}" "${E2E_RETAINED_LEASE_SECONDS}" \
    "same native client renews normally while the failed worker stays stopped" \
    dhcp_transaction_after "${GUEST_EVENT_CUTOFF}" "${GUEST_ACTION_EPOCH}" "${GUEST_LEASE}" renewal
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline HA-WORKER-LIVE-NETWORK "${deadline}" 90 \
    "unchanged VMI and client retain successful network samples through worker loss" guest_continuity_after "${cutoff}"
  assert_case HA-WORKER-RESERVATION "reservation survives the worker outage" reservation_stable
  assert_case HA-WORKER-METRICS "Service reports exact reservation accounting during worker outage" metric_pool_equals 1 10
  guard_case HA-WORKER-RESTORE "stopped worker and only its test uplinks are restored" restore_stopped_worker "${deadline}"
  wait_before_deadline HA-WORKER-RECOVERED "${deadline}" 120 "worker and passive observer recover" worker_has_recovered
  # A restored worker re-registers, rebuilds the pod sandbox, and starts the helper
  # again, and that startup can block for its full 30s API timeout while the node's
  # networking settles, so two-replica readiness needs several minutes here.
  wait_before_deadline HA-WORKER-HELPERS-RECOVERED "${deadline}" 420 "normal two-replica readiness returns" helper_pods_ready
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline HA-WORKER-POST-RECOVERY-NETWORK "${deadline}" 90 \
    "live guest stays unchanged after worker recovery" guest_continuity_after "${cutoff}"
  STOPPED_WORKER="" STOPPED_WORKER_ID=""
  assert_case HA-LEADER-AFTER-WORKER-RECOVERY "leader state consistent before follower churn" leader_consistent
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
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline HA-LIVE-AFTER-CHURN "${deadline}" 90 \
    "same live guest retains its network through follower churn, scaling and link bounce" guest_continuity_after "${cutoff}"
  stop_guest ha-live "${deadline}"

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
  stop_guest churn "${deadline}"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true --timeout=120s
  wait_before_deadline HA-RESERVATION-RELEASED "${deadline}" 120 \
    "guest deletion releases the reservation" cleanup_complete
  wait_before_deadline HA-METRICS-AFTER-RELEASE "${deadline}" 60 \
    "metrics return to the empty pool after the release" metric_pool_equals 0 11
  assert_case HA-VM-METRIC-AFTER-RELEASE "cleanup removes the VM metric" metric_vm_absent
  capture_checkpoint 24-ha-reservation-released \
    "${KIH_VM_NAME} released ${RESERVED_IP} and its metric after helper churn"
  guard_case HA-DEADLINE "HA scenarios completed within their original deadline" test "${SECONDS}" -lt "${deadline}"
  SCENARIO_DEADLINE=0
  printf 'PASS HA group: live guest survived worker loss, churn, scaling and bounce; cold boot survived total pod loss\n' \
    > "${E2E_ARTIFACTS_DIR}/12-ha-group.txt"
}

run_lease_group() {
  report_group lease
  local deadline manifest before cutoff marker
  deadline=$((SECONDS + 420))
  SCENARIO_DEADLINE="${deadline}"
  marker='IPPool configuration changes detected, updating the dhcppool'
  wait_before_deadline LEASE-STARTS-EMPTY "${deadline}" 90 "lease group starts from an empty pool" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  before="$(reload_snapshot "${marker}")"
  command_before_deadline LEASE-SHORT-LEASE-PATCH "${deadline}" "short lease update accepted" \
    kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p '{"spec":{"ipv4config":{"leasetime":30}}}'
  wait_before_deadline LEASE-SHORT-LEASE-RELOAD "${deadline}" 60 "short lease update is newly processed" \
    reload_processed "${before}" "${marker}"
  manifest="${E2E_ARTIFACTS_DIR}/16-lease-vm.yaml"
  render_halted_vm "${KIH_VM_NAME}" "${KIH_VM_MAC}" "${manifest}"
  command_before_deadline LEASE-VM-CREATED "${deadline}" "lease test VM created normally" \
    kubectl apply -f "${manifest}"
  wait_before_deadline LEASE-RESERVATION "${deadline}" 120 "short lease VM reservation" vm_reservation_ready
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert short-lease "${deadline}"
  before="$(reload_snapshot "${marker}")"
  snapshot_guest_continuity
  command_before_deadline LEASE-NORMAL-LEASE-PATCH "${deadline}" "normal lease restored with guest still running" \
    kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"leasetime\":${E2E_RETAINED_LEASE_SECONDS}}}}"
  wait_before_deadline LEASE-NORMAL-LEASE-RELOAD "${deadline}" 60 "normal lease update is newly processed" \
    reload_processed "${before}" "${marker}"
  wait_before_deadline LEASE-NORMAL-LEASE-ACK "${deadline}" 90 \
    "same native client naturally renews and receives the restored lease duration" \
    dhcp_transaction_after "${GUEST_EVENT_CUTOFF}" "${GUEST_ACTION_EPOCH}" \
    "${E2E_RETAINED_LEASE_SECONDS}" renewal
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline LEASE-LIVE-CONTINUITY "${deadline}" 90 \
    "unchanged VMI and client retain working network across the lease change" guest_continuity_after "${cutoff}"
  assert_case LEASE-RESERVATION-STABLE "renewal preserves exact reservation and accounting" reservation_stable
  capture_checkpoint 14-lease-restored "live native renewal received the restored lease duration"
  stop_guest short-lease "${deadline}"
  command_before_deadline LEASE-VM-DELETED "${deadline}" "lease VM deletion accepted" \
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=false
  wait_before_deadline LEASE-RESERVATION-RELEASED "${deadline}" 120 "lease group releases its reservation" cleanup_complete
  guard_case LEASE-DEADLINE "lease scenarios completed within their original deadline" \
    test "${SECONDS}" -lt "${deadline}"
  SCENARIO_DEADLINE=0
  printf 'PASS lease group: native renewal confirmed restored lease and uninterrupted sampled networking\n' \
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
  on_secondary leader_services_healthy
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
    get network-attachment-definition "${KIH_SECOND_NAD_NAME}"
}

shared_reservation_ready() {
  local config primary secondary
  config="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg multipool-shared -o json)" || return 1
  primary="$(pool_snapshot "${KIH_IPPOOL_NAME}")" || return 1
  secondary="$(pool_snapshot e2e-pool-second)" || return 1
  jq -e --arg first "${KIH_HELPER_NAMESPACE}/${KIH_NAD_NAME}" \
    --arg second "${KIH_HELPER_NAMESPACE}/${KIH_SECOND_NAD_NAME}" \
    --arg owner "${KIH_WORKLOAD_NAMESPACE}/multipool-shared" \
    --argjson primary "${primary}" --argjson secondary "${secondary}" '
      . as $cfg
      | .spec.vmname == "multipool-shared"
      and (.spec.networkconfig | length == 2)
      and (.status.networkconfig | length == 2)
      and ((.metadata.finalizers // []) | index("kubevirtiphelper.k8s.binbash.org/vmnetcfg-cleanup") != null)
      and all([{net:$first,mac:"02:00:00:00:02:10",pool:$primary},
               {net:$second,mac:"02:00:00:00:02:11",pool:$secondary}][];
        . as $nic
        | any($cfg.spec.networkconfig[];
            .networkname == $nic.net and .macaddress == $nic.mac
            and $nic.pool.allocated[.ipaddress] == ($owner + " [" + $nic.mac + "]"))
        and any($cfg.status.networkconfig[];
            .networkname == $nic.net and .macaddress == $nic.mac and .status == "OK"))
    ' <<< "${config}" > /dev/null
}

secondary_stability_snapshot() {
  local lease endpoints pods metrics
  on_secondary leader_consistent || return 1
  lease="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get lease "${KIH_SECOND_LEADER_LEASE}" -o json)" || return 1
  endpoints="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get endpoints "${KIH_SECOND_METRICS_SERVICE}" -o json)" || return 1
  pods="$(on_secondary helper_pod_runtime_snapshot)" || return 1
  metrics="$(helper_service_metrics "${KIH_SECOND_METRICS_SERVICE}" |
    grep -E '^kubevirtiphelper_(ippool_|vmnetcfg_status)' | LC_ALL=C sort)" || return 1
  # renewTime/resourceVersion are intentionally not stable: healthy leadership
  # renews continuously. Holder, acquisition, transitions, UID and target are.
  jq -Scn --argjson lease "${lease}" --argjson endpoints "${endpoints}" --arg pods "${pods}" --arg metrics "${metrics}" '
    {lease:{uid:$lease.metadata.uid,holder:$lease.spec.holderIdentity,
      acquired:$lease.spec.acquireTime,transitions:($lease.spec.leaseTransitions // 0)},
     endpoints:[$endpoints.subsets[]?.addresses[]? | {ip,targetRef}],pods:$pods,metrics:$metrics}'
}

# The secondary recorder remains live while a second, single-NIC guest proves
# primary DHCP. Keep the primary state local so both consoles/capture ownership
# records coexist, and keep report writes in this shell (not a subshell).
multipool_primary_live_failover() { # <label> <deadline> <secondary stability snapshot>
  local label="$1" deadline="$2" secondary_before="$3" node old_leader old_id cutoff
  local secondary_console_pid="${CONSOLE_PID}" secondary_console_feeder="${CONSOLE_FEEDER_PID}"
  local secondary_console_fifo="${CONSOLE_FIFO}"
  local secondary_guest_identity="${GUEST_IDENTITY}"
  local -A secondary_pids=() secondary_pods=() secondary_files=() secondary_tokens=()
  for node in "${!CAPTURE_PIDS[@]}"; do
    secondary_pids["${node}"]="${CAPTURE_PIDS[$node]}"
    secondary_pods["${node}"]="${CAPTURE_PODS[$node]}"
    secondary_files["${node}"]="${CAPTURE_FILES[$node]}"
    secondary_tokens["${node}"]="${CAPTURE_TOKENS[$node]}"
  done
  local KIH_VM_NAME="multipool-primary-guest" KIH_VM_MAC="02:00:00:00:02:20" RESERVED_IP
  local CONSOLE_PID="" CONSOLE_FEEDER_PID="" CONSOLE_FIFO=""
  local -A CAPTURE_PIDS=() CAPTURE_PODS=() CAPTURE_FILES=() CAPTURE_TOKENS=()
  local GUEST_CONSOLE="" GUEST_EVENTS="" GUEST_EXPECTED="" GUEST_IDENTITY="" GUEST_BASELINE=""
  local GUEST_BASELINE_INDEX=-1 GUEST_NODE="" GUEST_DEADLINE=0 GUEST_LEASE=""
  local GUEST_EVENT_CUTOFF=0 GUEST_ACTION_EPOCH=0
  trap 'finish_multipool_primary "$?"' EXIT
  RESERVED_IP="$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" \
    -o jsonpath='{.spec.networkconfig[0].ipaddress}')"
  start_guest_and_assert "${label}" "${deadline}"
  assert_case MULTI-BOTH-GUESTS-LIVE "primary DHCP completed while the unchanged secondary VMI remains Ready" \
    test "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmi multipool-guest -o json |
      jq -cer 'select(.metadata.deletionTimestamp == null and .status.phase == "Running")
        | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))
        | [.metadata.uid,.status.nodeName]')" = "${secondary_guest_identity}"
  assert_case MULTI-PRIMARY-LEADER-BEFORE-FAILOVER "primary leader is independently resolved" leader_consistent
  old_leader="${LEADER_POD}" old_id="${LEADER_ID}"
  snapshot_guest_continuity
  primary_failover_epoch="$(date +%s.%N)"
  command_before_deadline MULTI-PRIMARY-LEADER-DELETE "${deadline}" "only primary leader is deleted; both guests remain live" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" delete pod "${old_leader}" --wait=false
  wait_before_deadline MULTI-PRIMARY-FAILOVER "${deadline}" 90 "primary transfers its own Lease and metrics endpoint" \
    new_leader_elected "${old_leader}" "${old_id}"
  wait_before_deadline MULTI-PRIMARY-RECOVERED "${deadline}" 90 "primary reconstructs only its own allocations" \
    leader_services_healthy
  assert_case MULTI-SECONDARY-UNCHANGED "secondary Lease, metrics and pod identity stay unchanged during primary failover" \
    secondary_stable_since "${secondary_before}"
  # Discard pre-recovery packets: require an actual native renewal answered
  # after the replacement helper has taken leadership, without restarting VM
  # or DHCP client. The primary and secondary observers both stay attached.
  refresh_dhcp_events
  GUEST_EVENT_CUTOFF="$(wc -l < "${GUEST_EVENTS}")"
  GUEST_ACTION_EPOCH="$(date +%s.%N)"
  wait_before_deadline MULTI-PRIMARY-LIVE-DHCP "${deadline}" "${E2E_FAILOVER_DHCP_TIMEOUT}" \
    "same primary native client renews through the new helper while secondary stays up" \
    dhcp_transaction_after "${GUEST_EVENT_CUTOFF}" "${GUEST_ACTION_EPOCH}" "${GUEST_LEASE}" renewal
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline MULTI-PRIMARY-LIVE-NETWORK "${deadline}" 60 \
    "same primary VMI and client retain successful network samples through leader loss" \
    guest_continuity_after "${cutoff}"
  stop_guest "${label}" "${deadline}"
  trap finish EXIT
}

# On any primary-guest failure, close its own recorders first, then let the
# ordinary suite EXIT handler close the still-running secondary set. No
# guessed process IDs, hidden capture failures, or extra scenario time.
finish_multipool_primary() {
  local rc="$1" node
  trap - EXIT
  set +e
  SCENARIO_DEADLINE=0
  if ! finish_guest_evidence "$((SECONDS + E2E_COLLECT_TOTAL_TIMEOUT))"; then
    report_case_start MULTI-PRIMARY-GUEST-EVIDENCE "primary recorder teardown preserves complete evidence"
    report_case_fail "primary recorder could not close cleanly"
    rc=1
  fi
  CAPTURE_PIDS=() CAPTURE_PODS=() CAPTURE_FILES=() CAPTURE_TOKENS=()
  for node in "${!secondary_pids[@]}"; do
    CAPTURE_PIDS["${node}"]="${secondary_pids[$node]}"
    CAPTURE_PODS["${node}"]="${secondary_pods[$node]}"
    CAPTURE_FILES["${node}"]="${secondary_files[$node]}"
    CAPTURE_TOKENS["${node}"]="${secondary_tokens[$node]}"
  done
  CONSOLE_PID="${secondary_console_pid}"
  CONSOLE_FEEDER_PID="${secondary_console_feeder}"
  CONSOLE_FIFO="${secondary_console_fifo}"
  finish "${rc}"
}
secondary_stable_since() {
  [ "$(secondary_stability_snapshot)" = "$1" ]
}

run_multipool_group() {
  report_group multipool
  local deadline old_vm old_mac helper_snapshot second_snapshot secondary_identity primary_failover_epoch cutoff
  deadline=$((SECONDS + 900))
  SCENARIO_DEADLINE="${deadline}"
  log "group multipool: attaching an independent second bridge and pool"
  report_case_start MULTI-STALE-RESOURCES-CLEARED \
    "no stale second-pool resources remain from an earlier run"
  if kubectl get ippool e2e-pool-second > /dev/null 2>&1 ||
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg multipool-vm > /dev/null 2>&1 ||
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg multipool-guest > /dev/null 2>&1 ||
    kubectl -n "${KIH_HELPER_NAMESPACE}" get network-attachment-definition \
      "${KIH_SECOND_NAD_NAME}" > /dev/null 2>&1; then
    die "stale multipool resources exist; remove e2e-pool-second, multipool-vm, multipool-guest, and ${KIH_SECOND_NAD_NAME} before rerunning"
  fi
  report_case_pass "second-pool namespace is clean"
  cat > "${E2E_ARTIFACTS_DIR}/17-second-nad.yaml" <<EOF
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: ${KIH_SECOND_NAD_NAME}
  namespace: ${KIH_HELPER_NAMESPACE}
spec:
  config: '{"cniVersion":"0.3.1","type":"bridge","bridge":"${KIH_SECOND_BRIDGE_NAME}"}'
EOF
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/17-second-nad.yaml" > /dev/null
  cat > "${E2E_ARTIFACTS_DIR}/18-second-pool.yaml" <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: IPPool
metadata:
  name: e2e-pool-second
  labels:
    kubevirtiphelper/network: ${KIH_SECOND_NAD_NAME}
    kubevirtiphelper/network-namespace: ${KIH_HELPER_NAMESPACE}
spec:
  ipv4config:
    serverip: 10.78.0.2
    subnet: 10.78.0.0/24
    pool:
      start: 10.78.0.100
      end: 10.78.0.102
    router: 10.78.0.1
    dns:
      - ${KIH_SECOND_DNS_SERVER}
    domainname: ${KIH_SECOND_DNS_DOMAIN}
    leasetime: 300
  networkname: ${KIH_HELPER_NAMESPACE}/${KIH_SECOND_NAD_NAME}
  bindinterface: ${KIH_SECOND_HELPER_INTERFACE}
EOF
  export E2E_SECOND_NETWORK_EXPECTED=1
  kubectl kustomize --load-restrictor=LoadRestrictionsNone "${E2E_DIR}/manifests/secondary" \
    > "${E2E_ARTIFACTS_DIR}/secondary-rendered.yaml"
  sed -i "s|image: kubevirt-ip-helper:e2e|image: ${E2E_IMAGE}|" \
    "${E2E_ARTIFACTS_DIR}/secondary-rendered.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/secondary-rendered.yaml"
  command_before_deadline MULTI-ATTACHMENT-ROLLOUT "${deadline}" "independent secondary helper rolls out normally" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${KIH_SECOND_HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  wait_before_deadline MULTI-PODS-RETURN "${deadline}" 180 "primary replicas remain Ready" helper_pods_ready
  wait_before_deadline MULTI-BOTH-KIHNET1 "${deadline}" 120 "secondary helper alone has its contracted interface" \
    on_secondary helper_pods_have_interface "${KIH_SECOND_HELPER_INTERFACE}"
  assert_case MULTI-PRIMARY-ONE-NAD "primary has no secondary interface" \
    helper_pods_lack_interface "${KIH_SECOND_HELPER_INTERFACE}"
  wait_before_deadline MULTI-PRIMARY-RECONSTRUCTS "${deadline}" 120 "primary serves during independent secondary rollout" \
    leader_services_healthy
  command_before_deadline MULTI-SECOND-POOL-CREATED "${deadline}" "second pool created after its interface exists" \
    kubectl apply -f "${E2E_ARTIFACTS_DIR}/18-second-pool.yaml"

  wait_before_deadline MULTI-SECOND-INITIALIZED "${deadline}" 120 "second pool initializes independently" \
    pool_initialized_named e2e-pool-second 3
  wait_before_deadline MULTI-SECOND-SERVER "${deadline}" 120 "leader serves the second pool address" \
    second_pool_services_healthy

  render_second_nad_vm multipool-vm 02:00:00:00:02:01 \
    "${E2E_ARTIFACTS_DIR}/19-second-pool-vm.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/19-second-pool-vm.yaml" > /dev/null
  wait_before_deadline MULTI-SECOND-ALLOCATION "${deadline}" 120 "second pool allocates its own reservation" \
    vm_managed_reservation multipool-vm OK
  wait_before_deadline MULTI-SECOND-ISOLATED "${deadline}" 60 "second pool accounting is isolated" \
    pool_counts_equal e2e-pool-second 1 2
  capture_checkpoint 17-second-pool-added "second bridge, pool and reservation are independent"
  wait_before_deadline MULTI-PRIMARY-STAYS-EMPTY "${deadline}" 60 "primary pool remains empty" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11

  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm multipool-vm \
    --wait=true --timeout=120s
  wait_before_deadline MULTI-SECOND-RELEASED "${deadline}" 120 "second pool reservation is released" \
    pool_counts_equal e2e-pool-second 0 3
  # A single shared VMNetCfg must retain both helpers' concurrent projections.
  render_halted_vm multipool-shared 02:00:00:00:02:10 "${E2E_ARTIFACTS_DIR}/shared-vm.yaml"
  kubectl create --dry-run=client -f "${E2E_ARTIFACTS_DIR}/shared-vm.yaml" -o json |
    jq --arg network "${KIH_HELPER_NAMESPACE}/${KIH_SECOND_NAD_NAME}" '
      .spec.template.spec.domain.devices.interfaces +=
        [{name:"secondary",bridge:{},macAddress:"02:00:00:00:02:11"}]
      | .spec.template.spec.networks += [{name:"secondary",multus:{networkName:$network}}]
    ' > "${E2E_ARTIFACTS_DIR}/shared-vm.json"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/shared-vm.json"
  wait_before_deadline MULTI-SHARED-RESERVATION "${deadline}" 120 \
    "both helpers project one shared VMNetCfg without losing either NIC" shared_reservation_ready
  assert_case MULTI-SIMULTANEOUS-PRIMARY "primary simultaneously serves its shared allocation" metric_pool_equals 1 10
  assert_case MULTI-SIMULTANEOUS-SECONDARY "secondary simultaneously serves its shared allocation" \
    on_secondary metric_pool_equals 1 2

  log "group multipool: real guest on the second bridge"
  render_second_nad_vm multipool-guest 02:00:00:00:02:02 \
    "${E2E_ARTIFACTS_DIR}/20-second-nad-vm.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/20-second-nad-vm.yaml" > /dev/null
  wait_before_deadline MULTI-GUEST-RESERVATION "${deadline}" 120 \
    "second bridge reserves the guest address" vm_managed_reservation multipool-guest OK
  render_halted_vm multipool-primary-guest 02:00:00:00:02:20 \
    "${E2E_ARTIFACTS_DIR}/multipool-primary-guest.yaml"
  kubectl apply -f "${E2E_ARTIFACTS_DIR}/multipool-primary-guest.yaml"
  wait_before_deadline MULTI-PRIMARY-GUEST-RESERVATION "${deadline}" 120 \
    "primary guest holds its own reservation beside the shared VM" \
    vm_managed_reservation multipool-primary-guest OK
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
    "second pool counts guest and shared reservation" pool_counts_equal e2e-pool-second 2 1
  wait_before_deadline MULTI-GUEST-PRIMARY-ISOLATED "${deadline}" 60 \
    "second bridge guest leaves primary shared and guest allocations intact" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 2 9
  capture_checkpoint 25-second-nad-guest "second bridge holds ${RESERVED_IP} for multipool-guest"
  start_guest_and_assert second-nad "${deadline}"
  secondary_identity="$(secondary_stability_snapshot)"
  snapshot_guest_continuity
  multipool_primary_live_failover primary-beside-secondary "${deadline}" "${secondary_identity}"
  wait_before_deadline MULTI-SECONDARY-LIVE-DHCP "${deadline}" 240 \
    "secondary guest naturally renews across primary failover" dhcp_transaction_after \
    "${GUEST_EVENT_CUTOFF}" "${primary_failover_epoch}" "${GUEST_LEASE}" renewal
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline MULTI-SECONDARY-LIVE-NETWORK "${deadline}" 60 \
    "secondary VMI, native client and successful network samples persist through primary loss" \
    guest_continuity_after "${cutoff}"
  assert_case MULTI-SECONDARY-STILL-UNCHANGED "secondary leadership and metrics target remain unchanged after renewal" \
    secondary_stable_since "${secondary_identity}"
  assert_case MULTI-SHARED-AFTER-FAILOVER "both shared NICs and allocations survive primary failover" shared_reservation_ready
  assert_case MULTI-PRIMARY-METRICS-AFTER-FAILOVER "primary metrics retain only its shared and guest allocations" metric_pool_equals 2 9
  assert_case MULTI-SECONDARY-METRICS-AFTER-FAILOVER "secondary metrics are unchanged by primary failover" \
    on_secondary metric_pool_equals 2 1
  stop_guest second-nad "${deadline}"
  wait_before_deadline MULTI-GUEST-RESERVATION-HELD "${deadline}" 60 \
    "halted second-NAD guest keeps its address" pool_counts_equal e2e-pool-second 2 1
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" --wait=true --timeout=120s
  wait_before_deadline MULTI-GUEST-OBJECTS-GONE "${deadline}" 120 \
    "second-NAD guest releases its VMNetCfg" vmnetcfg_absent_named multipool-guest
  wait_before_deadline MULTI-GUEST-RELEASED "${deadline}" 60 \
    "second pool retains only shared NIC after guest deletion" pool_counts_equal e2e-pool-second 1 2
  KIH_VM_NAME="${old_vm}"
  KIH_VM_MAC="${old_mac}"
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm multipool-primary-guest --wait=true --timeout=120s
  wait_before_deadline MULTI-PRIMARY-GUEST-CLEANED "${deadline}" 120 \
    "primary guest releases its retained reservation" vmnetcfg_absent_named multipool-primary-guest
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm multipool-shared --wait=true --timeout=120s
  wait_before_deadline MULTI-SHARED-CLEANED "${deadline}" 120 \
    "both helpers acknowledge shared finalizer cleanup" vmnetcfg_absent_named multipool-shared
  wait_before_deadline MULTI-SHARED-PRIMARY-RELEASED "${deadline}" 60 "primary shared allocation released" \
    pool_counts_equal "${KIH_IPPOOL_NAME}" 0 11
  wait_before_deadline MULTI-SHARED-SECONDARY-RELEASED "${deadline}" 60 "secondary shared allocation released" \
    pool_counts_equal e2e-pool-second 0 3

  log "group multipool: deleting the second IPPool while both helpers keep serving"
  helper_snapshot="$(helper_pod_runtime_snapshot || true)"
  second_snapshot="$(on_secondary helper_pod_runtime_snapshot)"
  guard_case MULTI-HELPER-SNAPSHOT \
    "both helper UIDs and restart counts captured before second-pool deletion" \
    test "$(printf '%s\n' "${helper_snapshot}" | sed '/^$/d' | wc -l)" -eq 2
  guard_case MULTI-SECOND-POOL-DELETE \
    "second IPPool deletion is accepted while helpers are live" \
    kubectl delete ippool e2e-pool-second --wait=true --timeout=120s
  wait_before_deadline MULTI-SECOND-POOL-GONE "${deadline}" 90 \
    "second IPPool object is removed while helpers stay live" second_ippool_absent
  wait_before_deadline MULTI-SECOND-SERVER-REMOVED "${deadline}" 90 \
    "secondary leader drops its pool server address" on_secondary second_server_ip_absent
  wait_before_deadline MULTI-SECOND-METRICS-GONE "${deadline}" 90 \
    "second pool metrics disappear from secondary service" on_secondary metric_ippool_absent e2e-pool-second
  wait_before_deadline MULTI-PRIMARY-HEALTH-WITH-SECOND-REMOVED "${deadline}" 120 \
    "primary DHCP service stays healthy beside the detached second bridge" \
    leader_services_healthy
  wait_before_deadline MULTI-HELPERS-UNCHANGED "${deadline}" 90 \
    "both helper pods stay live with unchanged UIDs and restart counts" \
    helper_pods_unchanged_since "${helper_snapshot}"
  assert_case MULTI-SECONDARY-POD-UNCHANGED "secondary pod does not restart during pool removal" \
    on_secondary helper_pods_unchanged_since "${second_snapshot}"
  assert_case MULTI-PRIMARY-METRICS-WITH-SECOND-REMOVED \
    "primary pool accounting stays exact after the second pool is removed" \
    metric_pool_equals 0 11
  capture_checkpoint 18-second-pool-removed \
    "second pool objects and metrics are gone while the primary pool keeps serving"

  log "group multipool: removing only the secondary helper pair"
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete deployment "${KIH_SECOND_HELPER_DEPLOYMENT}" \
    --wait=true --timeout=120s
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete service "${KIH_SECOND_METRICS_SERVICE}"
  export E2E_SECOND_NETWORK_EXPECTED=0
  wait_before_deadline MULTI-PRIMARY-ONLY-TOPOLOGY "${deadline}" 180 \
    "primary-only helper topology returns" helper_pods_ready
  wait_before_deadline MULTI-KIHNET1-REMOVED "${deadline}" 120 \
    "second interface is gone from both helper pods" helper_pods_lack_interface kihnet1
  kubectl -n "${KIH_HELPER_NAMESPACE}" delete network-attachment-definition \
    "${KIH_SECOND_NAD_NAME}" --ignore-not-found > /dev/null
  wait_before_deadline MULTI-NAD-REMOVED "${deadline}" 90 \
    "second NetworkAttachmentDefinition is removed" second_nad_absent
  wait_before_deadline MULTI-SECOND-METRICS-STAY-GONE "${deadline}" 90 \
    "second pool metrics stay absent after the topology restore" \
    metric_ippool_absent e2e-pool-second
  wait_before_deadline MULTI-PRIMARY-HEALTH-AFTER-REMOVAL "${deadline}" 120 \
    "primary pool remains healthy after second-pool removal" leader_services_healthy
  capture_checkpoint 26-second-resources-absent \
    "primary-only topology with second pool, interface, NAD, and metrics absent"
  guard_case MULTI-DEADLINE "multipool scenarios completed within their original deadline" \
    test "${SECONDS}" -lt "${deadline}"
  SCENARIO_DEADLINE=0
  printf 'PASS multipool group: second bridge attachment, real guest DHCP, live pool removal, and cleanup\n' \
    > "${E2E_ARTIFACTS_DIR}/14-multipool-group.txt"
}

main() {
  local rendered vm_rendered default_image old_leader old_id octet failover_deadline failover_budget retained_lease_deadline router_original reinit_before
  local image_id webhook_image_id repository image_record nodes node loaded before_deployment install_mode reload_before cutoff
  local -a kind_nodes=()
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
  "${RUNTIME}" build -f "${ROOT_DIR}/build/Dockerfile" -t "${E2E_IMAGE}" "${ROOT_DIR}"
  image_record="$("${RUNTIME}" image inspect "${E2E_IMAGE}")"
  image_id="$(jq -er '.[0] | (.Id // .ID) | sub("^sha256:";"")
    | select(test("^[0-9a-f]{64}$"))' <<< "${image_record}")"
  repository="${E2E_IMAGE%@*}"
  case "${repository##*/}" in *:*) repository="${repository%:*}" ;; esac
  "${RUNTIME}" tag "${E2E_IMAGE}" "${repository}:e2e-${image_id}"
  E2E_IMAGE="${repository}:e2e-${image_id}"
  export E2E_IMAGE
  printf '%s\n' "${image_record}" > "${E2E_ARTIFACTS_DIR}/built-image.json"
  report_case_pass "image ${E2E_IMAGE} present in ${RUNTIME}"
  report_case_start CORE-WEBHOOK-IMAGE-BUILT "singleton admission image built from repository root"
  "${RUNTIME}" build -f "${ROOT_DIR}/build/Dockerfile.webhook" -t "${E2E_WEBHOOK_IMAGE}" "${ROOT_DIR}"
  image_record="$("${RUNTIME}" image inspect "${E2E_WEBHOOK_IMAGE}")"
  webhook_image_id="$(jq -er '.[0] | (.Id // .ID) | sub("^sha256:";"")
    | select(test("^[0-9a-f]{64}$"))' <<< "${image_record}")"
  repository="${E2E_WEBHOOK_IMAGE%@*}"
  case "${repository##*/}" in *:*) repository="${repository%:*}" ;; esac
  "${RUNTIME}" tag "${E2E_WEBHOOK_IMAGE}" "${repository}:e2e-${webhook_image_id}"
  E2E_WEBHOOK_IMAGE="${repository}:e2e-${webhook_image_id}"
  export E2E_WEBHOOK_IMAGE
  printf '%s\n' "${image_record}" > "${E2E_ARTIFACTS_DIR}/built-webhook-image.json"
  report_case_pass "webhook image ${E2E_WEBHOOK_IMAGE} present in ${RUNTIME}"
  report_case_start CORE-BOOTSTRAP \
    "disposable cluster bootstrapped with bridge CNI, Multus and KubeVirt"
  REPORT_BOOTSTRAP_STARTED=1
  "${E2E_DIR}/bootstrap.sh"
  report_case_pass "cluster ${E2E_CLUSTER_NAME} up on ${KUBERNETES_VERSION}"
  CLUSTER_STATE="$(cat "${E2E_CLUSTER_STATE_FILE}")"
  case "${CLUSTER_STATE}" in owned|reused) ;; *) die "invalid cluster ownership state" ;; esac
  nodes="$("${KIND}" get nodes --name "${E2E_CLUSTER_NAME}")" ||
    die "cannot list kind nodes for ${E2E_CLUSTER_NAME}"
  while IFS= read -r node; do
    [ -z "${node}" ] || kind_nodes+=("${node}")
  done <<< "${nodes}"
  guard_case CORE-KIND-NODE-COUNT "kind cluster has exactly three nodes for image verification" \
    test "${#kind_nodes[@]}" -eq 3
  "${KIND}" load docker-image --name "${E2E_CLUSTER_NAME}" "${E2E_WEBHOOK_IMAGE}"
  for node in "${kind_nodes[@]}"; do
    loaded="$("${RUNTIME}" exec "${node}" crictl inspecti "${E2E_IMAGE}")"
    assert_case "CORE-IMAGE-${node}" "node ${node} loaded the exact built image content" \
      test "$(jq -er '.status.id | sub("^sha256:";"")' <<< "${loaded}")" = "${image_id}"
    printf '%s\n' "${loaded}" > "${E2E_ARTIFACTS_DIR}/loaded-image-${node}.json"
    loaded="$("${RUNTIME}" exec "${node}" crictl inspecti "${E2E_WEBHOOK_IMAGE}")"
    assert_case "CORE-WEBHOOK-IMAGE-${node}" "node ${node} loaded exact webhook image content" \
      test "$(jq -er '.status.id | sub("^sha256:";"")' <<< "${loaded}")" = "${webhook_image_id}"
    printf '%s\n' "${loaded}" > "${E2E_ARTIFACTS_DIR}/loaded-webhook-image-${node}.json"
  done
  report_case_start CORE-BOOTSTRAP-JOURNAL \
    "bootstrap gate journal is parseable and imported into the suite report"
  if report_import_bootstrap_cases 1; then
    report_case_pass "bootstrap-cases.jsonl imported"
  else
    die "cannot import bootstrap-cases.jsonl"
  fi
  capture_checkpoint 01-bootstrap "cluster, CNI chain and KubeVirt right after bootstrap"
  assert_case CORE-VIRTCTL-INSTALLED "bootstrap installed ${VIRTCTL}" test -x "${VIRTCTL}"
  # CR registration precedes controller startup and all custom-resource access.
  kubectl apply -f "${ROOT_DIR}/deployments/crds.yaml"
  kubectl wait --for=condition=Established --timeout="${E2E_WAIT_TIMEOUT}s" \
    crd/virtualmachinenetworkconfigs.kubevirtiphelper.k8s.binbash.org \
    crd/ippools.kubevirtiphelper.k8s.binbash.org
  # A kept cluster can be rerun, but no reservation from the previous run may
  # leak into this one.
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vm "${KIH_VM_NAME}" \
    --ignore-not-found --wait=true --timeout=120s
    kubectl -n "${KIH_WORKLOAD_NAMESPACE}" delete vmnetcfg "${KIH_VM_NAME}" \
      --ignore-not-found --wait=true --timeout=120s


  rendered="${E2E_ARTIFACTS_DIR}/helper-rendered.yaml"
  kubectl kustomize --load-restrictor=LoadRestrictionsNone "${E2E_DIR}/manifests" > "${rendered}"
  default_image='kubevirt-ip-helper:e2e'
  if [ "${E2E_IMAGE}" != "${default_image}" ]; then
    case "${E2E_IMAGE}" in *'|'* | *'&'*) die "E2E_IMAGE may not contain | or &: ${E2E_IMAGE}" ;; esac
    sed -i "s|image: ${default_image}|image: ${E2E_IMAGE}|" "${rendered}"
  fi
  case "${E2E_WEBHOOK_IMAGE}" in *'|'* | *'&'*) die "invalid E2E_WEBHOOK_IMAGE" ;; esac
  sed -i "s|image: kubevirt-ip-helper-webhook:e2e|image: ${E2E_WEBHOOK_IMAGE}|" "${rendered}"
  assert_case CORE-RENDERED-IMAGE "rendered helper image equals ${E2E_IMAGE}" \
    grep -q "image: ${E2E_IMAGE}" "${rendered}"
  assert_case CORE-NO-LEGACY-HELPER \
    "obsolete unscoped helper is absent; Lease cutover requires explicit stop-old migration" \
    object_absent_not_found -n "${KIH_HELPER_NAMESPACE}" get deployment kubevirt-ip-helper
  before_deployment="$(kubectl -n "${KIH_HELPER_NAMESPACE}" get deployment "${HELPER_DEPLOYMENT}" \
    --ignore-not-found -o json)"
  printf '%s\n' "${before_deployment}" > "${E2E_ARTIFACTS_DIR}/deployment-before-apply.json"
  if [ -z "${before_deployment}" ]; then
    install_mode="first deployment"
  elif [ "$(jq -r '.spec.template.spec.containers[] | select(.name == "kubevirt-ip-helper") | .image' \
      <<< "${before_deployment}")" = "${E2E_IMAGE}" ]; then
    install_mode="existing same-image deployment"
  else
    install_mode="ordinary changed-image rollout"
  fi
  kubectl apply -f "${rendered}"
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${KIH_WEBHOOK_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  wait_for CORE-WEBHOOK-ROUTING-TLS 120 \
    "canonical singleton webhook routes only to admission pods with valid serving TLS" webhook_ready
  assert_case CORE-WEBHOOK-ADMISSION \
    "live API admission rejects invalid input rather than failing open" webhook_admission_qualified
  report_case_start CORE-HELPER-ROLLED-OUT "${install_mode} reaches normal production readiness"
  kubectl -n "${KIH_HELPER_NAMESPACE}" rollout status \
    "deployment/${HELPER_DEPLOYMENT}" --timeout="${E2E_WAIT_TIMEOUT}s"
  report_case_pass "${install_mode}; no forced restart or readiness override"
  wait_for CORE-HELPER-PODS-READY 120 "two helper pods Ready with ${KIH_HELPER_INTERFACE}" helper_pods_ready
  wait_for CORE-LEADER-CONSISTENT 120 "one labelled leader, matching Lease, and one metrics endpoint" leader_consistent
  assert_case CORE-DEPLOYED-IMAGE "deployment template uses the exact built image" \
    test "$(kubectl -n "${KIH_HELPER_NAMESPACE}" get deployment "${HELPER_DEPLOYMENT}" \
      -o jsonpath='{.spec.template.spec.containers[?(@.name=="kubevirt-ip-helper")].image}')" = "${E2E_IMAGE}"
  capture_checkpoint 02-helper-ready "helper replicas Ready with ${KIH_HELPER_INTERFACE} and one leader"
  report_case_start CORE-STALE-RESOURCES-CLEARED \
    "expanded-group resources from an interrupted run are removed before core setup"
  cleanup_stale_expanded_resources
  # Admission deliberately prevents deleting a referenced pool. Drain all
  # primary and shared VMNetCfgs above before replacing its configuration.
  kubectl delete ippool "${KIH_IPPOOL_NAME}" --ignore-not-found --wait=true --timeout=120s
  report_case_pass "pool, multipool, and second-NAD resources are absent before core setup"
  capture_checkpoint 02-start-clean \
    "expanded-group resources cleared after helper CRDs and serving objects are ready"

  kubectl apply -f "${E2E_DIR}/manifests/pool.yaml"
  wait_for CORE-POOL-INITIALIZED 120 "IPPool initialized with 11 available addresses" pool_initialized
  assert_case CORE-WEBHOOK-VMNETCFG-ADMISSION \
    "VMNetCfg range validation uses live admission and the configured pool" webhook_vmnetcfg_admission_qualified
  wait_for CORE-LEADER-SERVICES 120 "leader owns ${KIH_IPPOOL_SERVER}/24 and UDP/67" leader_services_healthy
  wait_for CORE-METRICS-EMPTY 60 "initial IPPool metrics" metric_pool_equals 0 11
  capture_checkpoint 03-pool-initialized "${KIH_IPPOOL_NAME} initialized with 11 free addresses"

  # manifests/vm.yaml is the current-profile template. Render it into this
  # profile's artifact directory and substitute the profile's guest image, so the
  # dependency-era lane never starts the guest on the current Cirros image. The
  # template carries no inline userData: its cloud-init source is the Secret created
  # below, so both lanes execute the same pinned guest observer script.
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
  # KubeVirt caps an inline cloudInitNoCloud userData at 2048 bytes and the guest
  # observer script is larger, so manifests/vm.yaml references it through
  # cloudInitNoCloud.secretRef. The Secret is created here, before the first VM
  # applies, and its bytes are compared with the pinned file because the guest
  # executes the Secret rather than the manifest.
  report_case_start CORE-GUEST-USERDATA-SECRET \
    "guest observer script is delivered through Secret ${KIH_GUEST_USERDATA_SECRET}"
  ensure_guest_userdata_secret
  guest_userdata_secret_matches ||
    die "Secret ${KIH_GUEST_USERDATA_SECRET} does not carry the pinned guest observer script"
  grep -qF "secretRef:" "${vm_rendered}" ||
    die "rendered guest manifest has no cloud-init secretRef"
  grep -qF "name: ${KIH_GUEST_USERDATA_SECRET}" "${vm_rendered}" ||
    die "rendered guest manifest does not reference Secret ${KIH_GUEST_USERDATA_SECRET}"
  report_case_pass "Secret carries manifests/${KIH_GUEST_USERDATA_FILE} and the guest references it"
  kubectl apply -f "${vm_rendered}"
  assert_case CORE-VM-CREATED-HALTED "VM ${KIH_VM_NAME} was created with runStrategy Halted" \
    test "$(kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vm "${KIH_VM_NAME}" \
      -o jsonpath='{.spec.runStrategy}')" = "Halted"
  assert_case CORE-NO-VMI-BEFORE-RESERVATION \
    "no VMI exists before the helper reserved an address" vmi_absent
  wait_for CORE-HALTED-RESERVATION 120 "VMNetCfg reservation while VM is halted" vm_reservation_ready
  assert_case CORE-VM-CONTROLLER-METADATA "helper created the VM reservation and cleanup finalizer" \
    vm_managed_reservation "${KIH_VM_NAME}" OK
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
  kubectl -n "${KIH_WORKLOAD_NAMESPACE}" get vmnetcfg "${KIH_VM_NAME}" -o json |
    jq '.metadata = {name:"e2e-duplicate-admission-probe",namespace:.metadata.namespace}
      | del(.status)' > "${E2E_ARTIFACTS_DIR}/webhook-duplicate-vmnetcfg.json"
  assert_case CORE-WEBHOOK-DUPLICATE-ADMISSION \
    "a distinct object cannot duplicate the same VM and MAC owner" admission_rejects \
    "${E2E_ARTIFACTS_DIR}/webhook-duplicate-vmnetcfg.json"
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
  reinit_before="$(reload_snapshot "${REINIT_MARKER}")"
  capture_checkpoint 19-router-before-restart "leader serving ${RESERVED_IP} before the restart-class router change"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p '{"spec":{"ipv4config":{"router":"10.77.0.9"}}}'
  wait_for CORE-ROUTER-RESTART-LOG 90 "router change starts application reinitialization" \
    reload_processed "${reinit_before}" "${REINIT_MARKER}"
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
  reinit_before="$(reload_snapshot "${REINIT_MARKER}")"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"router\":\"${router_original}\"}}}"
  wait_for CORE-ROUTER-RESTORE-LOG 90 "restored router starts a second reinitialization" \
    reload_processed "${reinit_before}" "${REINIT_MARKER}"
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

  reload_before="$(reload_snapshot "${RELOAD_MARKER}")"
  kubectl patch ippool "${KIH_IPPOOL_NAME}" --type=merge \
    -p "{\"spec\":{\"ipv4config\":{\"leasetime\":${E2E_RETAINED_LEASE_SECONDS}}}}"
  wait_for CORE-RELOAD-PROCESSED 60 "reloadable IPPool update newly processed" \
    reload_processed "${reload_before}" "${RELOAD_MARKER}"
  wait_for CORE-HEALTH-AFTER-RELOAD 90 "DHCP and metrics healthy after IPPool reload" leader_services_healthy
  wait_for CORE-STABLE-AFTER-RELOAD 90 "reservation and metrics stable after reload" reservation_stable
  wait_for CORE-METRICS-AFTER-RELOAD 60 "metrics stable after reload" metric_pool_equals 1 10
  capture_checkpoint 07-pool-reload "leader reloaded the patched ${KIH_IPPOOL_NAME} lease window"
  # Start the lease clock before the DHCP boot. The failover proof may use less
  # than its nominal budget after stop latency, but can never pass after expiry.
  retained_lease_deadline=$((SECONDS + E2E_RETAINED_LEASE_SECONDS))
  start_guest_and_assert reload "${retained_lease_deadline}"
  capture_checkpoint 08-boot-reload "guest boot after reload kept ${RESERVED_IP}"
  snapshot_guest_continuity

  assert_case FAILOVER-LEADER-STABLE-BEFORE \
    "leader state consistent before the active leader is deleted" leader_consistent
  old_leader="${LEADER_POD}"
  old_id="${LEADER_ID}"
  failover_deadline=$((SECONDS + E2E_FAILOVER_DHCP_TIMEOUT))
  [ "${failover_deadline}" -le "${retained_lease_deadline}" ] ||
    failover_deadline="${retained_lease_deadline}"
  SCENARIO_DEADLINE="${failover_deadline}"
  failover_budget=$((failover_deadline - SECONDS))
  guard_case FAILOVER-LEASE-WINDOW \
    "retained lease remains before active leader deletion" \
    test "${failover_budget}" -gt 0
  command_before_deadline FAILOVER-LEADER-DELETE "${failover_deadline}" \
    "active leader deletion accepted while the guest stays running" \
    kubectl -n "${KIH_HELPER_NAMESPACE}" delete pod "${old_leader}" --wait=false
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
  refresh_dhcp_events
  GUEST_EVENT_CUTOFF="$(wc -l < "${GUEST_EVENTS}")"
  GUEST_ACTION_EPOCH="$(date +%s.%N)"
  wait_before_deadline FAILOVER-LIVE-RENEWAL "${failover_deadline}" "${E2E_FAILOVER_DHCP_TIMEOUT}" \
    "unchanged native client naturally renews through the replacement helper" \
    dhcp_transaction_after "${GUEST_EVENT_CUTOFF}" "${GUEST_ACTION_EPOCH}" \
    "${E2E_RETAINED_LEASE_SECONDS}" renewal
  cutoff="$(guest_samples | jq -er '.[-1].seq')"
  wait_before_deadline FAILOVER-LIVE-NETWORK "${failover_deadline}" 90 \
    "same VMI and client retain successful network samples across helper loss" guest_continuity_after "${cutoff}"
  stop_guest reload "${failover_deadline}"
  start_guest_and_assert failover "${failover_deadline}"
  capture_checkpoint 09-leader-failover "new leader served the retained lease during failover"
  guard_case FAILOVER-EVIDENCE-CLOSED "cold-start packet and console evidence closes cleanly before lease expiry" \
    finish_guest_evidence_before_deadline "${failover_deadline}"
  guard_case FAILOVER-DEADLINE "live and cold failover checks completed before lease expiry" \
    test "${SECONDS}" -lt "${failover_deadline}"
  SCENARIO_DEADLINE=0

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

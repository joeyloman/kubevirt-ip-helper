#!/usr/bin/env bash
# shellcheck shell=bash
# Schema-v1 case journal and report renderer for the E2E suite.
#
# Sourced by test/e2e/run.sh. Every executed assertion appends one JSON object to
# ${E2E_RUN_DIR}/cases.jsonl as it closes, so partial results survive an abrupt
# exit, and report_finalize renders report.json plus report.txt from the same
# in-process records. Each execution gets its own directory under
# ${E2E_ARTIFACTS_ROOT}/runs: ${E2E_ARTIFACTS_DIR} already is that directory, and
# `latest` is a one-line text pointer to the newest run, so the previous run's
# journal can be compared against this one without overwriting earlier artifacts.
#
# Reporting is part of the result rather than a decoration on it: a failed report
# write, move, checksum walk, jq validation, or `latest` publication makes
# report_finalize return non-zero so the suite cannot go green on a truncated
# artifact set. The contracted calls below are what run.sh needs.
#
# Prerequisites: bash, date, find, sort, sha256sum, and jq.

REPORT_SCHEMA_VERSION=1
REPORT_SUITE="kubevirt-ip-helper-e2e"

# Current group label, applied to every case that starts after it.
REPORT_GROUP="core"
# Case that is currently open, if any.
REPORT_CASE_ID=""
REPORT_CASE_NAME=""
REPORT_CASE_GROUP=""
REPORT_CASE_T0=""
# Run identity and timing.
REPORT_ROOT=""
REPORT_STARTED_AT=""
REPORT_T0=""
REPORT_PREVIOUS_RUN_DIR=""

# Aggregated records, keyed in first-seen order.
declare -a REPORT_IDS=()
declare -a REPORT_NAMES=()
declare -a REPORT_GROUPS=()
declare -a REPORT_STATUS=()
declare -a REPORT_MS=()
declare -a REPORT_DETAILS=()
declare -A REPORT_INDEX=()
declare -A REPORT_GROUP_SEEN=()
declare -a REPORT_NOTE_NAMES=()
declare -a REPORT_NOTE_DETAILS=()

REPORT_BOOTSTRAP_IMPORTED=0
export REPORT_BOOTSTRAP_IMPORT_ATTEMPTED=0
REPORT_JOURNAL_FAILURE=0
_report_utc() {
  date -u +%Y-%m-%dT%H:%M:%SZ 2> /dev/null || printf 'unknown'
}

# Milliseconds, for durations only. GNU date supports %3N; when it does not, the
# bash SECONDS counter keeps the pair of readings on one consistent baseline.
_report_now_ms() {
  local stamp
  stamp="$(date +%s%3N 2> /dev/null)" || stamp=""
  case "${stamp}" in
    '' | *[!0-9]*) printf '%s' "$((SECONDS * 1000))" ;;
    *) printf '%s' "${stamp}" ;;
  esac
}

_report_escape() {
  local text="$1"
  text="${text//\\/\\\\}"
  text="${text//\"/\\\"}"
  text="${text//$'\t'/ }"
  text="${text//$'\r'/}"
  text="${text//$'\n'/ }"
  printf '%s' "${text}"
}

_report_run_dir() {
  # ${E2E_ARTIFACTS_DIR} is already ${E2E_ARTIFACTS_ROOT}/runs/${E2E_RUN_ID}, so it
  # is used verbatim. Appending the suffix here as well would nest a second run
  # directory inside the one versions.env composed.
  if [ -n "${E2E_RUN_DIR:-}" ]; then
    printf '%s' "${E2E_RUN_DIR}"
  elif [ -n "${E2E_ARTIFACTS_DIR:-}" ]; then
    printf '%s' "${E2E_ARTIFACTS_DIR}"
  else
    printf '%s' "${E2E_ARTIFACTS_ROOT:-${PWD}/_artifacts/e2e}/runs/${E2E_RUN_ID:-standalone}"
  fi
}
# Report paths are resolved from the report directory, not the runner's absolute
# filesystem. This keeps report.json useful after a CI artifact is downloaded.
_report_portable_path() { # <path>
  local target="$1" dir root
  dir="$(_report_run_dir)"
  root="${REPORT_ROOT:-${E2E_ARTIFACTS_ROOT:-}}"
  if [ -z "${target}" ]; then
    return 0
  fi
  if [ "${target}" = "${dir}" ]; then
    printf '.'
  elif [ -n "${root}" ] && [ "${target}" = "${root}" ]; then
    printf '../..'
  elif [ -n "${root}" ] && [[ "${target}" == "${root}/"* ]]; then
    printf '../../%s' "${target#"${root}/"}"
  else
    printf '%s' "${target}"
  fi
}


# Publishes the one-line `latest` pointer by atomic replacement. The pointer is a
# plain text file holding the run directory relative to the artifact root, never a
# symlink, so an artifact download that does not carry symlink targets still keeps
# a readable pointer. `_evidence_read_latest` still accepts an older symlink.
_report_publish_latest() { # <root> <pointer-name> <relative-target>
  local link="${1}/${2}" tmp
  tmp="${link}.tmp.${E2E_RUN_ID:-$$}.${BASHPID:-$$}"
  mkdir -p "${1}" || return 1
  # Prepare the replacement before touching the old pointer. If the filesystem
  # rejects the write, the previous completed run remains the comparison basis.
  printf '%s\n' "${3}" > "${tmp}" || {
    rm -f "${tmp}" > /dev/null 2>&1 || true
    return 1
  }
  # `-T` replaces a legacy symlink itself instead of following it as a directory.
  # It also leaves an unexpected real directory untouched when replacement fails.
  if ! mv -Tf "${tmp}" "${link}" > /dev/null 2>&1; then
    rm -f "${tmp}" > /dev/null 2>&1 || true
    return 1
  fi
  return 0
}

# Prepares this execution's run directory. A directory that already holds files is
# rejected instead of truncated: the previous run's journal, checkpoints, and
# reports are evidence for the comparison, and an overwritten set silently destroys
# the baseline. An empty directory is fine because versions.env and run.sh create
# it before sourcing this file.
_report_prepare_run_dir() { # <run-dir>
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
  elif ! mkdir -p "${dir}" > /dev/null 2>&1; then
    printf '[e2e] ERROR: cannot create run directory %s\n' "${dir}" >&2
    return 1
  fi
  return 0
}
_report_previous_eligible() { # <run-dir>
  local dir="$1"
  [ -d "${dir}" ] || return 1
  # Pre-report artifact trees have no report.json; their complete evidence is
  # still a usable legacy baseline. Once a report exists, only a successful
  # finalized run may become the next comparison basis.
  if [ ! -s "${dir}/report.json" ]; then
    return 0
  fi
  command -v jq > /dev/null 2>&1 &&
    jq -e '.schemaVersion == 1 and .status == "passed" and .exitCode == 0' \
      "${dir}/report.json" > /dev/null 2>&1
}

# Prepares this execution's artifact directory. versions.env already composed
# ${E2E_ARTIFACTS_DIR} as ${E2E_ARTIFACTS_ROOT}/runs/${E2E_RUN_ID}, so both values
# are only adopted here: ${REPORT_ROOT} stays ${E2E_ARTIFACTS_ROOT}, the directory
# CI uploads, and ${E2E_RUN_DIR} becomes ${E2E_ARTIFACTS_DIR} without a second
# composition. The previous `latest` pointer is read before anything is written, so
# the baseline always names an earlier run and earlier artifacts are never
# overwritten. The value comes from ${E2E_PREVIOUS_RUN_DIR} when a layer already
# resolved it, and the shared evidence reader is reused when evidence.sh is loaded.
# The pointer itself is only written later by report_finalize, so it never names a
# run whose report set is still incomplete. Initialization fails when the run
# directory already holds another run's artifacts.
report_init() { # [artifact-root]
  local root="${1:-${E2E_ARTIFACTS_ROOT:-}}" previous="" stamp="" link
  local runs="${E2E_RUNS_SUBDIR:-runs}" name="${E2E_LATEST_NAME:-latest}"
  case "${root}" in
    '') root="${PWD}/_artifacts/e2e" ;;
    /*) ;;
    *) root="${PWD}/${root}" ;;
  esac
  REPORT_ROOT="${root}"
  if ! mkdir -p "${root}/${runs}" > /dev/null 2>&1; then
    printf '[e2e] ERROR: cannot create artifact root %s\n' "${root}" >&2
    return 1
  fi
  stamp="$(date -u +%Y%m%dT%H%M%SZ 2> /dev/null)" || stamp=""
  [ -n "${stamp}" ] || stamp="$((SECONDS))-$$"
  E2E_RUN_ID="${E2E_RUN_ID:-${stamp}-$$}"
  E2E_RUN_DIR="${E2E_ARTIFACTS_DIR:-${root}/${runs}/${E2E_RUN_ID}}"
  E2E_PREVIOUS_RUN_DIR="${E2E_PREVIOUS_RUN_DIR:-}"
  if [ -z "${E2E_PREVIOUS_RUN_DIR}" ]; then
    if declare -F _evidence_read_latest > /dev/null 2>&1; then
      previous="$(_evidence_read_latest "${root}")"
    else
      link="${root}/${name}"
      if [ -L "${link}" ]; then
        previous="$(readlink "${link}" 2> /dev/null || true)"
      elif [ -s "${link}" ]; then
        read -r previous < "${link}" || previous=""
      fi
      case "${previous}" in
        '' | /*) ;;
        *) previous="${root}/${previous}" ;;
      esac
    fi
    if [ -n "${previous}" ] && _report_previous_eligible "${previous}"; then
      E2E_PREVIOUS_RUN_DIR="${previous}"
    fi
  fi
  if [ -n "${E2E_PREVIOUS_RUN_DIR}" ] &&
    ! _report_previous_eligible "${E2E_PREVIOUS_RUN_DIR}"; then
    E2E_PREVIOUS_RUN_DIR=""
  fi
  REPORT_PREVIOUS_RUN_DIR="${E2E_PREVIOUS_RUN_DIR}"
  if ! _report_prepare_run_dir "${E2E_RUN_DIR}"; then
    return 1
  fi
  if ! mkdir -p "${E2E_RUN_DIR}/evidence/checkpoints" > /dev/null 2>&1; then
    printf '[e2e] ERROR: cannot create %s/evidence/checkpoints\n' "${E2E_RUN_DIR}" >&2
    return 1
  fi
  : > "${E2E_RUN_DIR}/cases.jsonl" || {
    printf '[e2e] ERROR: cannot truncate %s/cases.jsonl\n' "${E2E_RUN_DIR}" >&2
    return 1
  }
  REPORT_STARTED_AT="$(_report_utc)"
  REPORT_T0="$(_report_now_ms)"
  REPORT_GROUP="core"
  export E2E_RUN_ID E2E_RUN_DIR
  export E2E_PREVIOUS_RUN_DIR
  return 0
}

report_group() { # <group>
  REPORT_GROUP="${1:-core}"
}

# Opens a case. Re-opening the same id keeps the original start time so a nested
# wait is not counted twice.
report_case_start() { # <case-id> <name>
  local id="${1:-case}" name
  name="${2:-${1:-case}}"
  if [ "${REPORT_CASE_ID}" != "${id}" ] || [ -z "${REPORT_CASE_T0}" ]; then
    REPORT_CASE_T0="$(_report_now_ms)"
  fi
  REPORT_CASE_ID="${id}"
  REPORT_CASE_NAME="${name}"
  REPORT_CASE_GROUP="${REPORT_GROUP}"
}

_report_case_close() { # <status> <detail>
  local status="$1" detail="$2" id name group idx now elapsed rc=0
  id="${REPORT_CASE_ID:-SUITE}"
  name="${REPORT_CASE_NAME:-${REPORT_GROUP} run}"
  group="${REPORT_CASE_GROUP:-${REPORT_GROUP}}"
  now="$(_report_now_ms)"
  elapsed=$((now - ${REPORT_CASE_T0:-${now}}))
  [ "${elapsed}" -ge 0 ] || elapsed=0
  if [ -n "${REPORT_INDEX[${id}]+x}" ]; then
    idx="${REPORT_INDEX[${id}]}"
    REPORT_MS[idx]=$((REPORT_MS[idx] + elapsed))
    if [ "${REPORT_STATUS[idx]}" != "failed" ]; then
      REPORT_STATUS[idx]="${status}"
    fi
    if [ -n "${REPORT_DETAILS[idx]}" ]; then
      REPORT_DETAILS[idx]="${REPORT_DETAILS[idx]} | ${detail}"
    else
      REPORT_DETAILS[idx]="${detail}"
    fi
  else
    idx=${#REPORT_IDS[@]}
    REPORT_INDEX[${id}]=${idx}
    REPORT_IDS[idx]="${id}"
    REPORT_NAMES[idx]="${name}"
    REPORT_GROUPS[idx]="${group}"
    REPORT_STATUS[idx]="${status}"
    REPORT_MS[idx]="${elapsed}"
    REPORT_DETAILS[idx]="${detail}"
  fi
  REPORT_GROUP_SEEN[${group}]=1
  if ! _report_append "{\"kind\":\"case\",\"id\":\"$(_report_escape "${id}")\"," \
    "\"name\":\"$(_report_escape "${name}")\",\"group\":\"$(_report_escape "${group}")\"," \
    "\"status\":\"${status}\",\"durationMs\":${elapsed}," \
    "\"detail\":\"$(_report_escape "${detail}")\"}"; then
    rc=1
    REPORT_JOURNAL_FAILURE=1
  fi
  REPORT_CASE_ID=""
  REPORT_CASE_NAME=""
  REPORT_CASE_GROUP=""
  REPORT_CASE_T0=""
  return "${rc}"
}
report_case_record() { # <id> <name> <group> <status> <duration-ms> <detail>
  local id="$1" name="$2" group="$3" status="$4" elapsed="$5" detail="$6"
  local idx rc=0
  case "${status}" in passed | failed) ;; *) return 1 ;; esac
  case "${elapsed}" in '' | *[!0-9]*) elapsed=0 ;; esac
  if [ -n "${REPORT_INDEX[${id}]+x}" ]; then
    idx="${REPORT_INDEX[${id}]}"
    REPORT_MS[idx]=$((REPORT_MS[idx] + elapsed))
    if [ "${REPORT_STATUS[idx]}" != "failed" ]; then
      REPORT_STATUS[idx]="${status}"
    fi
    if [ -n "${REPORT_DETAILS[idx]}" ]; then
      REPORT_DETAILS[idx]="${REPORT_DETAILS[idx]} | ${detail}"
    else
      REPORT_DETAILS[idx]="${detail}"
    fi
  else
    idx=${#REPORT_IDS[@]}
    REPORT_INDEX[${id}]=${idx}
    REPORT_IDS[idx]="${id}"
    REPORT_NAMES[idx]="${name}"
    REPORT_GROUPS[idx]="${group}"
    REPORT_STATUS[idx]="${status}"
    REPORT_MS[idx]="${elapsed}"
    REPORT_DETAILS[idx]="${detail}"
  fi
  REPORT_GROUP_SEEN[${group}]=1
  if ! _report_append "{\"kind\":\"case\",\"id\":\"$(_report_escape "${id}")\"," \
    "\"name\":\"$(_report_escape "${name}")\",\"group\":\"$(_report_escape "${group}")\"," \
    "\"status\":\"${status}\",\"durationMs\":${elapsed}," \
    "\"detail\":\"$(_report_escape "${detail}")\"}"; then
    rc=1
    REPORT_JOURNAL_FAILURE=1
  fi
  return "${rc}"
}

report_import_bootstrap_cases() { # [require-record]
  local required="${1:-0}" file="${2:-${E2E_ARTIFACTS_DIR}/bootstrap-cases.jsonl}"
  local errors="${E2E_ARTIFACTS_DIR}/bootstrap-journal-errors.txt"
  local records id name group status elapsed detail
  [ "${REPORT_BOOTSTRAP_IMPORTED}" -eq 1 ] && return 0
  [ "${REPORT_BOOTSTRAP_IMPORT_ATTEMPTED}" -eq 1 ] && return 1
  REPORT_BOOTSTRAP_IMPORT_ATTEMPTED=1
  if [ ! -e "${file}" ] && [ "${required}" -eq 0 ]; then
    REPORT_BOOTSTRAP_IMPORTED=1
    return 0
  fi
  [ ! -s "${errors}" ] || return 1
  [ -s "${file}" ] || return 1
  command -v jq > /dev/null 2>&1 || return 1
  jq -e -s '
    length > 0
    and all(.[];
      (.kind == "bootstrap-case"
       and (.id | type) == "string"
       and (.name | type) == "string"
       and (.group | type) == "string"
       and (.status == "passed" or .status == "failed")
       and (.durationMs | type) == "number"
       and (.detail | type) == "string"))
  ' "${file}" > /dev/null 2>&1 || return 1
  records="$(jq -s -r '
    .[]
    | [.id, .name, .group, .status, (.durationMs | tostring), .detail]
    | @tsv
  ' "${file}")" || return 1
  while IFS=$'\t' read -r id name group status elapsed detail; do
    [ -n "${id}" ] || continue
    report_case_record "${id}" "${name}" "${group}" "${status}" \
      "${elapsed}" "${detail}" || return 1
  done <<< "${records}"
  REPORT_BOOTSTRAP_IMPORTED=1
}

_report_append() { # <json-object>
  local dir
  dir="$(_report_run_dir)"
  if [ ! -d "${dir}" ]; then
    mkdir -p "${dir}" > /dev/null 2>&1 || return 1
  fi
  printf '%s\n' "$*" >> "${dir}/cases.jsonl" || return 1
  return 0
}

report_case_pass() { # <detail>
  _report_case_close passed "${1:-verified}"
}

report_case_fail() { # <detail>
  _report_case_close failed "${1:-assertion failed}"
}

# Reports whether a case is currently open. `die` in run.sh closes only an open
# case, so a `report_case_fail` that already closed the failing step is not
# followed by a second generic suite record. With no case open, the close above
# records under the one generic `SUITE` id, which every later generic close merges
# into instead of appending another record.
report_case_is_open() {
  [ -n "${REPORT_CASE_ID}" ]
}

report_note() { # <name> <detail>
  local name="${1:-note}" detail="${2:-}"
  REPORT_NOTE_NAMES[${#REPORT_NOTE_NAMES[@]}]="${name}"
  REPORT_NOTE_DETAILS[${#REPORT_NOTE_DETAILS[@]}]="${detail}"
  if ! _report_append "{\"kind\":\"note\",\"name\":\"$(_report_escape "${name}")\"," \
    "\"detail\":\"$(_report_escape "${detail}")\"}"; then
    REPORT_JOURNAL_FAILURE=1
    return 1
  fi
}

_report_parse_field() { # <line> <key>
  local rest="${1#*\""${2}"\":\"}"
  printf '%s' "${rest%%\"*}"
}

# Finalizes the run: collected files are copied into diagnostics, the journal is
# rendered as JSON and text, the previous run is compared, and a checksum manifest
# indexes everything this run produced. Called from the EXIT trap so both success
# and failure always reach it.
report_finalize() { # <exit-code>
  local rc="${1:-0}" dir root now status passed failed notes groups problems=0
  local id idx file name detail previous
  if [ -z "${E2E_RUN_DIR:-}" ]; then
    report_init || return 1
  fi
  dir="$(_report_run_dir)"
  if [ "${E2E_EVIDENCE_COMPLETE:-1}" = 0 ] && [ "${rc}" -eq 0 ]; then
    report_case_start SUITE-EVIDENCE-COMPLETENESS \
      "required Kubernetes evidence is complete before publishing the run"
    report_case_fail "evidence finalization did not certify a complete run"
    rc=1
  fi
  root="${REPORT_ROOT:-${E2E_ARTIFACTS_ROOT:-}}"
  if [ -z "${root}" ]; then
    case "${dir}" in
      */runs/*) root="${dir%/runs/*}" ;;
      *) root="${PWD}/_artifacts/e2e" ;;
    esac
  fi
  if ! mkdir -p "${dir}/diagnostics" > /dev/null 2>&1; then
    problems=1
  fi
  for file in "${dir}"/*.txt "${dir}"/*.log "${dir}"/*.yaml "${dir}/kubeconfig" \
    "${dir}/cluster-state"; do
    [ -e "${file}" ] || continue
    cp -p "${file}" "${dir}/diagnostics/" > /dev/null 2>&1 || problems=1
  done
  now="$(_report_utc)"
  [ -n "${REPORT_STARTED_AT}" ] || REPORT_STARTED_AT="${now}"
  [ -n "${REPORT_T0}" ] || REPORT_T0="$(_report_now_ms)"
  # An assertion that started but never closed is the failing step: record it
  # with the exit status instead of leaving the journal short.
  if [ -n "${REPORT_CASE_ID}" ]; then
    if [ "${rc}" -eq 0 ]; then
      report_case_pass "reached the end of the run"
    else
      report_case_fail "run stopped here with exit ${rc}"
    fi
  fi
  if [ "${#REPORT_IDS[@]}" -eq 0 ]; then
    report_case_start SUITE "suite prologue and prerequisite check"
    if [ "${rc}" -eq 0 ]; then
      report_case_pass "no assertion was needed"
    else
      report_case_fail "suite aborted before any assertion ran (exit ${rc})"
    fi
  fi
  passed=0
  failed=0
  groups=0
  for idx in "${!REPORT_IDS[@]}"; do
    case "${REPORT_STATUS[idx]}" in
      passed) passed=$((passed + 1)) ;;
      *) failed=$((failed + 1)) ;;
    esac
  done
  for name in "${!REPORT_GROUP_SEEN[@]}"; do
    groups=$((groups + 1))
  done
  notes=${#REPORT_NOTE_NAMES[@]}
  if [ "${REPORT_JOURNAL_FAILURE}" -ne 0 ]; then
    rc=1
  fi
  if [ "${rc}" -eq 0 ] && [ "${failed}" -eq 0 ]; then
    status="passed"
  else
    status="failed"
  fi
  {
    printf '{\n'
    printf '  "schemaVersion": %s,\n' "${REPORT_SCHEMA_VERSION}"
    printf '  "suite": "%s",\n' "$(_report_escape "${REPORT_SUITE}")"
    printf '  "name": "%s",\n' "$(_report_escape "${E2E_NAME_SUFFIX:-e2e}")"
    printf '  "stack": "%s",\n' "$(_report_escape "${E2E_STACK:-}")"
    printf '  "group": "%s",\n' "$(_report_escape "${E2E_GROUP:-all}")"
    printf '  "cluster": "%s",\n' "$(_report_escape "${E2E_CLUSTER_NAME:-}")"
    printf '  "image": "%s",\n' "$(_report_escape "${E2E_IMAGE:-}")"
    printf '  "runId": "%s",\n' "$(_report_escape "${E2E_RUN_ID:-}")"
    printf '  "startedAt": "%s",\n' "$(_report_escape "${REPORT_STARTED_AT}")"
    printf '  "finishedAt": "%s",\n' "$(_report_escape "${now}")"
    printf '  "durationMs": %s,\n' "$(( $(_report_now_ms) - REPORT_T0 ))"
    printf '  "status": "%s",\n' "${status}"
    printf '  "exitCode": %s,\n' "${rc}"
    printf '  "counts": {"cases": %s, "passed": %s, "failed": %s, "notes": %s, "groups": %s},\n' \
      "${#REPORT_IDS[@]}" "${passed}" "${failed}" "${notes}" "${groups}"
    printf '  "artifacts": {"root": "%s", "run": "%s", "reportJson": "report.json",' \
      "$(_report_escape "$(_report_portable_path "${root}")")" \
      "$(_report_escape "$(_report_portable_path "${dir}")")"
    printf ' "reportTxt": "report.txt", "cases": "cases.jsonl",'
    printf ' "bootstrapCases": "bootstrap-cases.jsonl",'
    printf ' "bootstrapJournalErrors": "bootstrap-journal-errors.txt",'
    printf ' "manifest": "artifact-manifest.sha256", "evidence": "evidence/checkpoints",'
    printf ' "diagnostics": "diagnostics", "previousRunDir": "%s"},\n' \
      "$(_report_escape "$(_report_portable_path "${REPORT_PREVIOUS_RUN_DIR}")")"
    _report_previous_block
    printf '  "cases": [\n'
    for idx in "${!REPORT_IDS[@]}"; do
      printf '    {"id": "%s", "name": "%s", "group": "%s", "status": "%s", "durationMs": %s, "detail": "%s"}' \
        "$(_report_escape "${REPORT_IDS[idx]}")" "$(_report_escape "${REPORT_NAMES[idx]}")" \
        "$(_report_escape "${REPORT_GROUPS[idx]}")" "${REPORT_STATUS[idx]}" "${REPORT_MS[idx]}" \
        "$(_report_escape "${REPORT_DETAILS[idx]}")"
      if [ "${idx}" -lt "$(( ${#REPORT_IDS[@]} - 1 ))" ]; then
        printf ','
      fi
      printf '\n'
    done
    printf '  ],\n'
    printf '  "notes": [\n'
    for idx in "${!REPORT_NOTE_NAMES[@]}"; do
      printf '    {"name": "%s", "detail": "%s"}' \
        "$(_report_escape "${REPORT_NOTE_NAMES[idx]}")" \
        "$(_report_escape "${REPORT_NOTE_DETAILS[idx]}")"
      if [ "${idx}" -lt "$(( ${#REPORT_NOTE_NAMES[@]} - 1 ))" ]; then
        printf ','
      fi
      printf '\n'
    done
    printf '  ]\n'
    printf '}\n'
  } > "${dir}/report.json.tmp" || problems=1
  mv "${dir}/report.json.tmp" "${dir}/report.json" || problems=1

  {
    printf 'kubevirt-ip-helper e2e report\n'
    printf 'run:       %s (%s / %s)\n' "${E2E_RUN_ID:-}" "${E2E_STACK:-}" "${E2E_GROUP:-}"
    printf 'status:    %s (exit %s)\n' "${status}" "${rc}"
    printf 'started:   %s\nfinished:  %s\n' "${REPORT_STARTED_AT}" "${now}"
    printf 'counts:    cases=%s passed=%s failed=%s notes=%s groups=%s\n' \
      "${#REPORT_IDS[@]}" "${passed}" "${failed}" "${notes}" "${groups}"
    if [ -n "${REPORT_PREVIOUS_RUN_DIR}" ]; then
      printf 'previous:  %s\n' "$(_report_portable_path "${REPORT_PREVIOUS_RUN_DIR}")"
    else
      printf 'previous:  none\n'
    fi
    printf 'artifacts: . (paths are relative to this report directory)\n\n'
    printf '%-7s %-26s %-11s %-7s %s\n' 'STATUS' 'CASE' 'GROUP' 'TIME' 'DETAIL'
    for idx in "${!REPORT_IDS[@]}"; do
      case "${REPORT_STATUS[idx]}" in
        passed) name='PASS' ;;
        *) name='FAIL' ;;
      esac
      printf '%-7s %-26s %-11s %-7s %s / %s\n' "${name}" "${REPORT_IDS[idx]}" \
        "${REPORT_GROUPS[idx]}" "$((REPORT_MS[idx]))ms" "${REPORT_NAMES[idx]}" \
        "${REPORT_DETAILS[idx]}"
    done
    if [ "${notes}" -gt 0 ]; then
      printf '\nnotes:\n'
      for idx in "${!REPORT_NOTE_NAMES[@]}"; do
        printf '  - %s: %s\n' "${REPORT_NOTE_NAMES[idx]}" "${REPORT_NOTE_DETAILS[idx]}"
      done
    fi
  } > "${dir}/report.txt.tmp" || problems=1
  mv "${dir}/report.txt.tmp" "${dir}/report.txt" || problems=1

  # The manifest closes the run so it covers report.json, report.txt, cases.jsonl,
  # the checkpoints, and every collected diagnostic. The evidence layer owns the
  # checksum walk when it is loaded, so whichever finalizer runs last produces one
  # identical manifest; both are safe to call more than once.
  if declare -F _evidence_manifest > /dev/null 2>&1; then
    _evidence_manifest || problems=1
  else
    _report_manifest "${dir}" || problems=1
  fi
  if [ "${REPORT_JOURNAL_FAILURE}" -ne 0 ]; then
    printf '[e2e] cases.jsonl append failed during the run\n' >&2
    problems=1
  fi
  if ! command -v jq > /dev/null 2>&1; then
    printf '[e2e] jq is required to validate report.json\n' >&2
    problems=1
  elif ! jq -e . "${dir}/report.json" > /dev/null 2>&1; then
    printf '[e2e] report.json failed jq validation\n' >&2
    problems=1
  fi
  if ! jq -e -s --argjson expected "${#REPORT_IDS[@]}" '
    all(.[];
      ((.kind == "case"
        and (.id | type) == "string"
        and (.status == "passed" or .status == "failed")
        and (.durationMs | type) == "number"
        and (.detail | type) == "string")
       or (.kind == "note"
        and (.name | type) == "string"
        and (.detail | type) == "string")))
    and ([.[] | select(.kind == "case") | .id] | unique | length) == $expected
  ' "${dir}/cases.jsonl" > /dev/null 2>&1; then
    printf '[e2e] cases.jsonl failed schema or aggregate validation\n' >&2
    problems=1
  fi
  if [ "${problems}" -eq 0 ] &&
    [ "${rc}" -eq 0 ] &&
    [ "${failed}" -eq 0 ] &&
    [ "${E2E_EVIDENCE_COMPLETE:-1}" != 0 ]; then
    # Only a successful run becomes the next comparison baseline. Failed runs
    # remain immutable under runs/ for diagnosis but cannot make a partial test
    # execution look like the newest complete result.
    _report_publish_latest "${root}" "${E2E_LATEST_NAME:-latest}" \
      "${E2E_RUNS_SUBDIR:-runs}/${E2E_RUN_ID}" || problems=1
  fi
  printf '[e2e] report: %s (%s passed, %s failed)\n' "${dir}/report.txt" "${passed}" "${failed}"
  if [ -s "${dir}/report.txt" ]; then
    cat "${dir}/report.txt" || true
  fi
  return "${problems}"
}

# Case-level diff against the previous run's journal. Only ids and statuses are
# read back, so the comparison stays exact without a JSON parser.
_report_previous_block() {
  local previous="${REPORT_PREVIOUS_RUN_DIR}" file count=0 added='' removed='' changed=''
  local line id state idx seen=''
  if [ -z "${previous}" ]; then
    printf '  "previousRun": null,\n'
    return 0
  fi
  file="${previous}/cases.jsonl"
  if [ ! -s "${file}" ]; then
    printf '  "previousRun": {"run": "%s", "journal": null},\n' \
      "$(_report_escape "$(_report_portable_path "${previous}")")"
    return 0
  fi
  while IFS= read -r line || [ -n "${line}" ]; do
    case "${line}" in
      *'"kind":"case"'*) ;;
      *) continue ;;
    esac
    id="$(_report_parse_field "${line}" id)"
    state="$(_report_parse_field "${line}" status)"
    [ -n "${id}" ] || continue
    count=$((count + 1))
    case " ${seen} " in
      *" ${id} "*) continue ;;
    esac
    seen="${seen} ${id}"
    if [ -z "${REPORT_INDEX[${id}]+x}" ]; then
      removed="${removed},\"${id}\""
    else
      idx="${REPORT_INDEX[${id}]}"
      if [ "${REPORT_STATUS[idx]}" != "${state}" ]; then
        changed="${changed},{\"id\":\"${id}\",\"from\":\"${state}\",\"to\":\"${REPORT_STATUS[idx]}\"}"
      fi
    fi
  done < "${file}"
  for idx in "${!REPORT_IDS[@]}"; do
    id="${REPORT_IDS[idx]}"
    case " ${seen} " in
      *" ${id} "*) ;;
      *) added="${added},\"${id}\"" ;;
    esac
  done
  printf '  "previousRun": {"run": "%s", "caseCount": %s, "added": [%s], "removed": [%s], "statusChanged": [%s], "evidenceComparison": "%s"},\n' \
    "$(_report_escape "$(_report_portable_path "${previous}")")" "${count}" "${added#,}" "${removed#,}" "${changed#,}" \
    "$(_report_compare_file)"
  return 0
}

_report_compare_file() {
  local dir
  dir="$(_report_run_dir)"
  if [ -s "${dir}/evidence/comparison-to-previous-run.json" ]; then
    printf 'evidence/comparison-to-previous-run.json'
  fi
}

# Fallback checksum walk for a run without the evidence layer: same exclusions as
# _evidence_manifest, so the two writers cannot disagree about the file set.
_report_manifest() { # <run-dir>
  local dir="$1" sums
  sums="$(
    cd "${dir}" || exit 1
    find . -type f ! -name artifact-manifest.sha256 ! -name artifact-manifest.sha256.tmp -print0 |
      LC_ALL=C sort -z |
      xargs -0 sha256sum
  )" || return 1
  printf '%s\n' "${sums}" > "${dir}/artifact-manifest.sha256" || return 1
  return 0
}

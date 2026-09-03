# shellcheck shell=bash
# Checkpoint evidence for the kubevirt-ip-helper E2E suite.
#
# Sourced by test/e2e/run.sh after versions.env, and sourced again by
# test/e2e/collect.sh so the previous-run comparison and the checksum manifest
# cover the collected diagnostics. Every capture is read-only against the cluster
# and writes only inside the current run directory.
#
# Contracted API:
#   evidence_init                      prepare this run directory and latest link
#   evidence_capture ID DESCRIPTION    raw JSON, normalized aggregate, changes
#   evidence_finalize                  previous-run comparison plus sha256 manifest
#
# A capture that cannot produce its required evidence is recorded in
# evidence/capture-errors.txt, surfaced to the reporting API when one is loaded,
# and returned as a non-zero status so the calling assertion path fails instead of
# continuing on an empty checkpoint.

: "${E2E_RUNS_SUBDIR:=runs}"
: "${E2E_LATEST_NAME:=latest}"

# Schema stamped into normalized.json and every comparison file. A prior run whose
# aggregate carries an older or missing schemaVersion is not compared: it is
# reported as incompatible instead of being read with guesses.
EVIDENCE_SCHEMA_VERSION=1

# What each checkpoint writes:
#   raw.json                     every captured Kubernetes document, keyed by group
#   normalized.json              one deterministic object record per object
#   changes-from-previous.json   added/removed/changed keys plus a unified diff
#   observations.txt             console markers, leader interface, listener, metrics
_evidence_now() { date -u +%Y-%m-%dT%H:%M:%SZ 2> /dev/null || date; }

_evidence_root() {
  # ${E2E_ARTIFACTS_DIR} is ${root}/runs/${E2E_RUN_ID} by construction in
  # versions.env, so stripping that suffix keeps whatever the caller made of the
  # path (already absolute by then) instead of deriving a second root. The
  # stripped suffix includes its leading slash, so the root stays slash-free and
  # joining it later cannot produce a doubled separator.
  local suffix="/${E2E_RUNS_SUBDIR}/${E2E_RUN_ID}"
  printf '%s\n' "${E2E_ARTIFACTS_DIR%"${suffix}"}"
}

_evidence_checkpoints_dir() { printf '%s\n' "${E2E_ARTIFACTS_DIR}/evidence/checkpoints"; }

_evidence_record_error() { # <subject> <detail>
  mkdir -p "${E2E_ARTIFACTS_DIR}/evidence" || return 1
  printf '%s\t%s\t%s\n' "$(_evidence_now)" "$1" "$2" >> "${E2E_ARTIFACTS_DIR}/evidence/capture-errors.txt"
}

# _evidence_note makes an evidence problem visible even when no reporting layer
# is loaded, and forwards it when one is.
_evidence_note() { # <subject> <detail>
  if declare -F report_note > /dev/null 2>&1; then
    report_note "$1" "$2"
  elif declare -F report_case_fail > /dev/null 2>&1; then
    report_case_fail "$1: $2"
  fi
  printf '[e2e] evidence: %s: %s\n' "$1" "$2" >&2
}

_evidence_fail() { # <subject> <detail>
  _evidence_record_error "$1" "$2" || true
  _evidence_note "$1" "$2"
  return 1
}

_evidence_tools() {
  local tool
  for tool in jq kubectl timeout; do
    command -v "${tool}" > /dev/null 2>&1 || {
      _evidence_fail "${tool}" "required by evidence capture but not available"
      return 1
    }
  done
  return 0
}

# _evidence_read_latest prints the run directory that ${root}/latest pointed at.
# The pointer is written as a one-line text file; an older symlink is still read so
# artifact trees from earlier builds keep a usable comparison basis.
_evidence_read_latest() { # <root>
  local root="$1" link="${1}/${E2E_LATEST_NAME}" value=""
  if [ -L "${link}" ]; then
    value="$(readlink "${link}" 2> /dev/null || true)"
  elif [ -f "${link}" ]; then
    value="$(cat "${link}" 2> /dev/null || true)"
  fi
  [ -n "${value}" ] || return 0
  case "${value}" in
    /*) ;;
    *) value="${root}/${value}" ;;
  esac
  if [ -s "${value}/report.json" ] &&
    { ! command -v jq > /dev/null 2>&1 ||
      ! jq -e '.schemaVersion == 1 and .status == "passed" and .exitCode == 0' \
        "${value}/report.json" > /dev/null 2>&1; }; then
    # A failed report is never a comparison baseline, even if an older version
    # left its directory in the latest pointer.
    return 0
  fi
  printf '%s\n' "${value}"
}

# _evidence_link_latest writes the one-line `latest` pointer by atomic replacement.
# The pointer is a plain text file holding the run directory relative to the
# artifact root, never a symlink, so an artifact download that drops symlink
# targets still carries a readable pointer. `_evidence_read_latest` accepts both
# shapes. Standalone collect finalization uses this fallback; run.sh report
# finalization publishes the same pointer through `_report_publish_latest`.
_evidence_link_latest() { # <root> <relative-target>
  local root="$1" target="$2" link tmp
  link="${root}/${E2E_LATEST_NAME}"
  tmp="${link}.tmp.${E2E_RUN_ID:-$$}.${BASHPID:-$$}"
  mkdir -p "${root}" || return 1
  # Prepare the replacement before touching the old pointer. If the filesystem
  # rejects the write, the previous completed run remains the comparison basis.
  printf '%s\n' "${target}" > "${tmp}" || {
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

# evidence_init prepares this execution's directory and exports the directory the
# previous run left behind as the comparison basis. It only reads ${root}/latest:
# the pointer is moved once, by report finalization, so it never names a run whose
# report set is still incomplete.
evidence_init() {
  local root previous order
  mkdir -p "${E2E_ARTIFACTS_DIR}/evidence/checkpoints" || {
    _evidence_fail "init" "cannot create ${E2E_ARTIFACTS_DIR}"
    return 1
  }
  root="$(_evidence_root)"
  mkdir -p "${root}/${E2E_RUNS_SUBDIR}" || {
    _evidence_fail "init" "cannot create ${root}/${E2E_RUNS_SUBDIR}"
    return 1
  }
  if [ -z "${E2E_PREVIOUS_RUN_DIR:-}" ]; then
    previous="$(_evidence_read_latest "${root}")"
    case "${previous}" in
      "" | "${E2E_ARTIFACTS_DIR}") : ;;
      *)
        [ -d "${previous}" ] || previous=""
        E2E_PREVIOUS_RUN_DIR="${previous}"
        ;;
    esac
    export E2E_PREVIOUS_RUN_DIR
  fi
  order="$(_evidence_order_file)"
  if [ ! -e "${order}" ]; then
    : > "${order}" || {
      _evidence_fail "init" "cannot create ${order}"
      return 1
    }
  fi
  # The pointer itself is written later, once, by report finalization, so a
  # collection that stops halfway cannot move `latest` onto an unfinished run.
  return 0
}

# _evidence_order_file is the append-only capture order. Numeric ${NN}- prefixes
# sort correctly on their own, but the file also survives unpadded ids and repeats.
_evidence_order_file() { printf '%s\n' "${E2E_ARTIFACTS_DIR}/evidence/checkpoints/.order"; }

_evidence_order_previous() { # <current-id>
  local order previous="" candidate idx
  local -a entries=()
  order="$(_evidence_order_file)"
  [ -s "${order}" ] || return 0
  mapfile -t entries < "${order}" || return 1
  for ((idx = ${#entries[@]} - 1; idx >= 0; idx--)); do
    candidate="${entries[idx]}"
    if [ "${candidate}" != "$1" ]; then
      previous="${candidate}"
      break
    fi
  done
  printf '%s\n' "${previous}"
}

_evidence_order_append() { # <id>
  local order previous
  order="$(_evidence_order_file)"
  previous="$(tail -n 1 "${order}" 2> /dev/null || true)"
  [ "${previous}" = "$1" ] || printf '%s\n' "$1" >> "${order}"
}
_evidence_checkpoint_complete() { # <checkpoint-dir>
  local dir="$1"
  for file in raw.json normalized.json changes-from-previous.json \
    comparison-to-previous-run.json observations.txt; do
    [ -f "${dir}/${file}" ] || return 1
  done
  _evidence_compatible "${dir}/normalized.json"
}

_evidence_order_final() {
  local order final="" candidate dir
  order="$(_evidence_order_file)"
  if [ -e "${order}" ]; then
    if [ -s "${order}" ]; then
      while IFS= read -r candidate || [ -n "${candidate}" ]; do
        dir="${E2E_ARTIFACTS_DIR}/evidence/checkpoints/${candidate}"
        _evidence_checkpoint_complete "${dir}" || continue
        final="${candidate}"
      done < "${order}"
    fi
    printf '%s\n' "${final:-}"
    return 0
  fi
  # Compatibility fallback for older artifact trees that predate .order.
  for dir in "${E2E_ARTIFACTS_DIR}"/evidence/checkpoints/*/; do
    [ -d "${dir}" ] || continue
    _evidence_checkpoint_complete "${dir}" || continue
    final="$(basename "${dir}")"
  done
  printf '%s\n' "${final:-}"
}


# _evidence_group runs one kubectl capture into a part file. The document has to
# look like a Kubernetes object or list; an empty or truncated response is an
# explicit failure rather than a silently short checkpoint.
_evidence_group() { # <dir> <label> <kubectl arguments...>
  local dir="$1" label="$2" rc=0 message
  shift 2
  timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl "$@" \
    > "${dir}/parts/${label}.json" 2> "${dir}/parts/${label}.err" || rc=$?
  if [ "${rc}" -ne 0 ]; then
    if [ "${EVIDENCE_ALLOW_MISSING_API:-}" = "1" ] &&
      grep -Eq "doesn't have a resource type|could not find the requested resource|no matches for kind" \
        "${dir}/parts/${label}.err"; then
      jq -n --arg message "$(tr '\n' ' ' < "${dir}/parts/${label}.err")" \
        '{apiVersion:"v1", kind:"List", items:[],
          evidence:{state:"api-not-installed", message:$message}}' \
        > "${dir}/parts/${label}.json"
      rm -f "${dir}/parts/${label}.err"
      return 0
    fi
    _evidence_fail "${label}" "kubectl exited ${rc}: $(tr '\n' ' ' < "${dir}/parts/${label}.err")"
    rm -f "${dir}/parts/${label}.json" "${dir}/parts/${label}.err"
    return 1
  fi
  if [ ! -s "${dir}/parts/${label}.json" ]; then
    if [ "${EVIDENCE_ALLOW_MISSING_API:-}" = "1" ]; then
      jq -n --arg message "resource is not installed or has no list envelope" \
        '{apiVersion:"v1", kind:"List", items:[],
          evidence:{state:"api-not-installed", message:$message}}' \
        > "${dir}/parts/${label}.json"
      return 0
    fi
    _evidence_fail "${label}" "response is empty"
    rm -f "${dir}/parts/${label}.json"
    return 1
  fi
  if ! jq -e 'has("items") or has("kind")' "${dir}/parts/${label}.json" > /dev/null 2>&1; then
    if [ "${EVIDENCE_ALLOW_MISSING_API:-}" = "1" ] &&
      grep -Eq '^[[:space:]]*No resources found' "${dir}/parts/${label}.json"; then
      message="$(tr '\n' ' ' < "${dir}/parts/${label}.json")"
      jq -n --arg message "${message}" \
        '{apiVersion:"v1", kind:"List", items:[],
          evidence:{state:"api-not-installed", message:$message}}' \
        > "${dir}/parts/${label}.json.tmp" || {
        rm -f "${dir}/parts/${label}.json.tmp"
        _evidence_fail "${label}" "cannot normalize no-resource response"
        return 1
      }
      mv -f "${dir}/parts/${label}.json.tmp" "${dir}/parts/${label}.json" || return 1
      return 0
    fi
    _evidence_fail "${label}" "response is not a Kubernetes JSON document"
    rm -f "${dir}/parts/${label}.json"
    return 1
  fi
  return 0
}

# contract: the full CRD inventory, the custom resources, the attached workloads,
# and the helper's own serving objects. Namespaces and events stay raw-only
# evidence.
_evidence_object_captures() { # <dir>
  local dir="$1" rc=0 allow_custom_api=0 checkpoint
  # The complete CRD inventory proves which APIs exist at every checkpoint; the
  # custom-resource groups below then record the instances and their status.
  _evidence_group "${dir}" crds get crd -o json || rc=1
  checkpoint="$(basename "${dir}")"
  case "${checkpoint}" in
    01-bootstrap) allow_custom_api=1 ;;
  esac
  _evidence_group "${dir}" namespaces get namespaces -o json || rc=1
  _evidence_group "${dir}" events get events -A --sort-by=.lastTimestamp -o json || rc=1
  _evidence_group "${dir}" networkattachmentdefinitions \
    get network-attachment-definitions -A -o json || rc=1
  # The KubeVirt CR carries the emulation setting the whole suite depends on.
  _evidence_group "${dir}" kubevirt get kubevirt -A -o json || rc=1
  _evidence_group "${dir}" helper-deployment \
    -n "${KIH_HELPER_NAMESPACE}" get deployments -o json || rc=1
  _evidence_group "${dir}" helper-pods \
    -n "${KIH_HELPER_NAMESPACE}" get pods -o json || rc=1
  _evidence_group "${dir}" helper-leases \
    -n "${KIH_HELPER_NAMESPACE}" get leases -o json || rc=1
  _evidence_group "${dir}" helper-services \
    -n "${KIH_HELPER_NAMESPACE}" get services -o json || rc=1
  _evidence_group "${dir}" helper-endpointslices \
    -n "${KIH_HELPER_NAMESPACE}" get endpointslices -o json || rc=1
  _evidence_group "${dir}" workloads \
    -n "${KIH_WORKLOAD_NAMESPACE}" get pods -o json || rc=1
  if [ "${allow_custom_api}" -eq 1 ]; then
    # The helper CRDs are intentionally absent at the first bootstrap checkpoint.
    EVIDENCE_ALLOW_MISSING_API=1 _evidence_group "${dir}" ippools \
      get ippools -A -o json || rc=1
    EVIDENCE_ALLOW_MISSING_API=1 _evidence_group "${dir}" virtualmachinenetworkconfigs \
      get virtualmachinenetworkconfigs -A -o json || rc=1
  else
    _evidence_group "${dir}" ippools \
      get ippools -A -o json || rc=1
    _evidence_group "${dir}" virtualmachinenetworkconfigs \
      get virtualmachinenetworkconfigs -A -o json || rc=1
  fi
  _evidence_group "${dir}" virtualmachines get virtualmachines -A -o json || rc=1
  _evidence_group "${dir}" virtualmachineinstances \
    get virtualmachineinstances -A -o json || rc=1
  return "${rc}"
}
# Paths in checkpoint observations are relative to the downloaded run directory.
# This keeps human diagnostics useful outside the original runner filesystem.
_evidence_portable_path() {
  local path="$1"
  case "${path}" in
    "${E2E_ARTIFACTS_DIR}")
      printf '.'
      ;;
    "${E2E_ARTIFACTS_DIR}/"*)
      printf '%s' "${path#${E2E_ARTIFACTS_DIR}/}"
      ;;
    "${E2E_ARTIFACTS_ROOT}")
      printf '../..'
      ;;
    "${E2E_ARTIFACTS_ROOT}/"*)
      printf '../../%s' "${path#${E2E_ARTIFACTS_ROOT}/}"
      ;;
    *)
      printf '%s' "${path}"
      ;;
  esac
}


# _evidence_observations keeps the non-object proof that belongs to a checkpoint:
# guest console markers, the artifact layout, and per-helper-pod interface and
# route samples. The labelled leader additionally contributes its UDP listener
# and metrics scrape; followers intentionally do not expose the listener.
_evidence_observations() { # <dir>
  local dir="$1" rc=0 leaders pods pod leader artifact output
  local leader_err pod_err write_error=0
  local observations="${dir}/observations.txt"
  {
    printf '# observations for checkpoint %s at %s\n' \
      "$(basename "${dir}")" "$(_evidence_now)" || write_error=1
    printf '\n===== artifacts =====\n' || write_error=1
    for artifact in "${E2E_ARTIFACTS_DIR}"/*; do
      [ -f "${artifact}" ] || continue
      printf -- '-- %s\n' "$(_evidence_portable_path "${artifact}")" || write_error=1
    done
    printf '\n===== console markers =====\n' || write_error=1
    for artifact in "${E2E_ARTIFACTS_DIR}"/console-*.log; do
      [ -f "${artifact}" ] || continue
      printf -- '-- %s\n' "$(_evidence_portable_path "${artifact}")" || write_error=1
      grep -o 'E2E_DHCP_[A-Z]*[^[:space:]]*' "${artifact}" || true
    done
  } > "${observations}" 2>&1 || write_error=1
  if [ "${write_error}" -ne 0 ]; then
    _evidence_fail "observations" "cannot write ${observations}"
    return 1
  fi

  leaders="$(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
    -n "${KIH_HELPER_NAMESPACE}" get pods -l "${LEADER_SELECTOR:-kubevirtiphelper/leader=active}" \
    -o jsonpath='{.items[*].metadata.name}' 2> "${dir}/.leader.err")" || rc=1
  leader_err="$(cat "${dir}/.leader.err" 2> /dev/null || true)"
  rm -f "${dir}/.leader.err" || rc=1
  pods="$(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
    -n "${KIH_HELPER_NAMESPACE}" get pods \
    -o jsonpath='{.items[*].metadata.name}' 2> "${dir}/.pods.err")" || rc=1
  pod_err="$(cat "${dir}/.pods.err" 2> /dev/null || true)"
  rm -f "${dir}/.pods.err" || rc=1
  if [ -z "${pods}" ]; then
    if ! printf '\n===== helper pods: none =====\n' >> "${observations}"; then
      _evidence_fail "observations" "cannot append helper pod listing to ${observations}"
      return 1
    fi
    if [ "${rc}" -ne 0 ]; then
      _evidence_fail "observations" \
        "cannot list helper pods: ${pod_err:-${leader_err}}"
      return 1
    fi
    return 0
  fi
  if [ -z "${leaders}" ]; then
    rc=1
    if ! printf '\n===== labelled leader: none =====\n' >> "${observations}"; then
      _evidence_record_error "observations" "cannot append missing-leader marker" || true
    fi
    _evidence_record_error "observations" \
      "helper pod list has no labelled leader${leader_err:+: ${leader_err}}" || true
  else
    if ! printf '\n===== labelled leader: %s =====\n' "${leaders}" >> "${observations}"; then
      rc=1
      _evidence_record_error "observations" "cannot append leader marker" || true
    fi
  fi
  for pod in ${pods}; do
    leader=0
    case " ${leaders} " in
      *" ${pod} "*) leader=1; printf '\n===== helper pod %s (leader) =====\n' "${pod}" ;;
      *) printf '\n===== helper pod %s =====\n' "${pod}" ;;
    esac >> "${observations}" || {
      rc=1
      _evidence_record_error "observations" "cannot append helper pod ${pod} header" || true
    }
    if ! output="$(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
      -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
      sh -c 'set -eu; ip addr; ip route; cat /proc/net/udp')"; then
      rc=1
      if ! printf -- '-- pod %s: interface, route, or UDP observation failed\n' \
        "${pod}" >> "${observations}"; then
        _evidence_record_error "observations" "cannot append failed-pod marker" || true
      fi
      _evidence_record_error "observations" \
        "helper pod ${pod} interface/route/UDP observation failed" || true
      continue
    fi
    if ! printf '%s\n' "${output}" >> "${observations}"; then
      rc=1
      _evidence_record_error "observations" \
        "cannot append helper pod ${pod} interface/route/UDP output" || true
    fi
    if [ "${leader}" -eq 1 ]; then
      if ! output="$(timeout --foreground "${E2E_CAPTURE_TIMEOUT}s" kubectl \
        -n "${KIH_HELPER_NAMESPACE}" exec "${pod}" -- \
        wget -qO- http://127.0.0.1:8080/)"; then
        rc=1
        if ! printf -- '-- pod %s: leader metrics scrape failed\n' \
          "${pod}" >> "${observations}"; then
          _evidence_record_error "observations" "cannot append metrics failure marker" || true
        fi
        _evidence_record_error "observations" \
          "leader pod ${pod} metrics scrape failed" || true
      elif ! printf '%s\n' "${output}" >> "${observations}"; then
        rc=1
        _evidence_record_error "observations" \
          "cannot append leader pod ${pod} metrics output" || true
      fi
    elif ! printf -- '-- pod %s: follower metrics scrape intentionally skipped (no listener)\n' \
      "${pod}" >> "${observations}"; then
      rc=1
      _evidence_record_error "observations" \
        "cannot append follower metrics note for ${pod}" || true
    fi
  done
  if [ "${rc}" -ne 0 ]; then
    _evidence_fail "observations" \
      "at least one helper pod observation or observation-file write failed"
  fi
  return "${rc}"
}

# _evidence_merge collapses the captured parts into raw.json with a deterministic
# group order, then drops the scratch parts so the checkpoint holds only its
# contracted files.
_evidence_merge() { # <dir>
  local dir="$1"
  jq -S -n '
    [inputs
     | {key: (input_filename | sub(".*/"; "") | sub("\\.json$"; "")), value: .}]
    | sort_by(.key)
    | from_entries
  ' "${dir}/parts"/*.json > "${dir}/raw.json.tmp" 2> "${dir}/.merge.err" || {
    _evidence_fail "merge" "cannot assemble raw.json: $(tr '\n' ' ' < "${dir}/.merge.err")"
    rm -f "${dir}/raw.json.tmp" "${dir}/.merge.err"
    return 1
  }
  rm -f "${dir}/.merge.err"
  mv -f "${dir}/raw.json.tmp" "${dir}/raw.json" || return 1
  rm -rf "${dir}/parts"
  return 0
}

# _evidence_normalize builds the object aggregate. Everything is sorted by
# resource, namespace, name so two captures of the same state are byte-identical.
# Only bookkeeping is dropped: resourceVersion, managedFields, selfLink,
# creationTimestamp, and condition/renewal timestamps. Kept on purpose: uid and
# generation (in-place update versus re-create), deletionTimestamp and finalizers
# (the deletion handshake), every spec, ownership labels and annotations, every
# finalizer and owner reference, and the whole status including pool accounting.
_evidence_normalize() { # <dir> <id>
  local dir="$1" id="$2"
  jq -S --arg id "${id}" --argjson schema "${EVIDENCE_SCHEMA_VERSION}" '
    def volatile_status:
      ["lastTransitionTime", "lastHeartbeatTime", "lastProbeTime", "lastUpdateTime",
       "lastupdate", "lastupdatebeforestart", "startTime", "renewTime", "acquireTime"];
    def trim_status:
      walk(if type == "object"
           then with_entries(select(.key as $k | volatile_status | index($k) | not))
           else . end);
    def trim_lease_spec:
      if type == "object"
      then with_entries(select(.key != "renewTime" and .key != "acquireTime"))
      else .
      end;
    def trim_annotations:
      if type == "object"
      then with_entries(select(.key != "endpoints.kubernetes.io/last-change-trigger-time"))
      else .
      end;
    def expand:
      if type == "object" and ((.items | type) == "array") then (.items // []) else [.] end;
    def record($group):
      (.metadata // {}) as $m |
      {
        group: $group,
        resource: (.kind // "unknown"),
        apiVersion: (.apiVersion // ""),
        namespace: ($m.namespace // ""),
        name: ($m.name // ""),
        uid: ($m.uid // ""),
        annotations: (($m.annotations // {}) | trim_annotations),
        generation: ($m.generation // 0),
        created: (($m.creationTimestamp // "") | . != ""),
        deleted: ($m.deletionTimestamp // null),
        gracePeriod: ($m.deletionGracePeriodSeconds // null),
        labels: ($m.labels // {}),
        finalizers: (($m.finalizers // []) | .),
        owners: (($m.ownerReferences // []) | .),
        spec: (if (.kind // "") == "Lease"
               then ((.spec // null) | trim_lease_spec)
               else (.spec // null)
               end),
        status: ((.status // null) | trim_status)
      };
    # Events are kept as raw evidence only: their payload is a message with a
    # counter and timestamps, which would add noise to every object comparison
    # without describing object state.
    {schemaVersion: $schema, checkpoint: $id,
     objects:
       [to_entries[]
        | select(.key != "events")
        | select(.value != null)
        | .key as $group
        | (.value | expand)[]
        | record($group)]
       | sort_by([.resource, .namespace, .name])}
  ' "${dir}/raw.json" > "${dir}/normalized.json.tmp" 2> "${dir}/.normalize.err" || {
    _evidence_fail "normalize" "cannot build normalized.json: $(tr '\n' ' ' < "${dir}/.normalize.err")"
    rm -f "${dir}/normalized.json.tmp" "${dir}/.normalize.err"
    return 1
  }
  rm -f "${dir}/.normalize.err"
  mv -f "${dir}/normalized.json.tmp" "${dir}/normalized.json" || return 1
  return 0
}

# _evidence_compare classifies one normalized state against another. added and
# removed carry the full record; changed names the sections that moved with their
# before and after value. The unified diff of the two aggregates is the same
# information in a form a person can read top to bottom.
_evidence_compare() { # <from> <to> <out> <checkpoint> <previous-label> <status>
  local from="$1" to="$2" out="$3" checkpoint="$4" previous="$5" status="$6"
  local placeholder="${out}.empty" difftmp="${out}.diff.tmp"
  local difffrom="${out}.from.tmp" diffto="${out}.to.tmp" notice="" rc=0 diff_rc=0
  if [ -n "${from}" ] && jq -e 'has("objects")' "${from}" > /dev/null 2>&1; then
    :
  else
    printf '{"checkpoint": "empty", "objects": []}\n' > "${placeholder}" || return 1
    from="${placeholder}"
  fi
  jq -e 'has("objects")' "${to}" > /dev/null 2>&1 || {
    rm -f "${placeholder}"
    _evidence_fail "compare" "${to} has no object aggregate"
    return 1
  }
  jq 'del(.checkpoint)' "${from}" > "${difffrom}" 2> /dev/null || {
    rm -f "${placeholder}" "${difffrom}"
    _evidence_fail "compare" "cannot prepare semantic diff input ${from}"
    return 1
  }
  jq 'del(.checkpoint)' "${to}" > "${diffto}" 2> /dev/null || {
    rm -f "${placeholder}" "${difffrom}" "${diffto}"
    _evidence_fail "compare" "cannot prepare semantic diff input ${to}"
    return 1
  }
  diff -u "${difffrom}" "${diffto}" > "${difftmp}" 2> /dev/null || diff_rc=$?
  if [ "${diff_rc}" -gt 1 ]; then
    rm -f "${placeholder}" "${difffrom}" "${diffto}" "${difftmp}"
    _evidence_fail "compare" "cannot create unified diff for ${checkpoint}"
    return 1
  fi
  notice="$(jq -S -n \
    --argjson schema "${EVIDENCE_SCHEMA_VERSION}" \
    --arg checkpoint "${checkpoint}" \
    --arg previous "${previous}" \
    --arg status "${status}" \
    --rawfile diff "${difftmp}" \
    --slurpfile a "${from}" \
    --slurpfile b "${to}" '
    def keyed:
      reduce ((.objects // [])[]) as $o ({};
        . + {($o | [.resource, .namespace, .name] | join("/")): $o});
    def union($x; $y):
      [$x, $y] | map(keys_unsorted) | add | unique;
    def sections($x; $y):
      [union($x; $y)[]
       | . as $k
       | select($x[$k] != $y[$k])
       | {section: $k, before: $x[$k], after: $y[$k]}];
    ($a[0] | keyed) as $A |
    ($b[0] | keyed) as $B |
    {schemaVersion: $schema,
     checkpoint: $checkpoint,
     previousCheckpoint: (if $previous == "" then null else $previous end),
     status: $status,
     counts: {before: ($A | length), after: ($B | length)},
     added: [($B | keys_unsorted[])
             | . as $k
             | select(($A | has($k)) | not)
             | {key: $k, record: $B[$k]}],
     removed: [($A | keys_unsorted[])
               | . as $k
               | select(($B | has($k)) | not)
               | {key: $k, record: $A[$k]}],
     changed: [($B | keys_unsorted[])
               | . as $k
               | select(($A | has($k)) and ($A[$k] != $B[$k]))
               | {key: $k, sections: sections($A[$k]; $B[$k])}],
    unifiedDiff: (($diff | rtrimstr("\n")) | split("\n"))}
  ' 2>&1 > "${out}.tmp")" || rc=$?
  if [ "${rc}" -ne 0 ]; then
    rm -f "${out}.tmp" "${placeholder}" "${difftmp}" "${difffrom}" "${diffto}"
    _evidence_fail "compare" "cannot write ${out}: $(printf '%s' "${notice}" | tr '\n' ' ')"
    return 1
  fi
  mv -f "${out}.tmp" "${out}" || return 1
  rm -f "${placeholder}" "${difftmp}" "${difffrom}" "${diffto}"
  return 0
}

# _evidence_compatible reports whether a completed normalized aggregate carries this
# build's schemaVersion. A missing or older version, or the synthetic aggregate used
# for a run with no successful checkpoints, is not a comparison baseline.
_evidence_compatible() { # <file>
  jq -e --argjson schema "${EVIDENCE_SCHEMA_VERSION}" \
    '.schemaVersion == $schema
     and (.checkpoint // "") != "none"
     and (.state // "complete") == "complete"' "$1" > /dev/null 2>&1
}

# _evidence_prior_compare compares one checkpoint with the same checkpoint id of the
# run that held `latest` before this one. The status is explicit: available when the
# earlier aggregate exists and speaks this schema, no-previous-run when the baseline
# is absent, incompatible when it exists but cannot be trusted. The file is written
# for every checkpoint, so each state transition has a cross-run witness.
_evidence_prior_compare() { # <dir> <id>
  local dir="$1" id="$2" previous="${E2E_PREVIOUS_RUN_DIR:-}" from="" \
    previous_label="" status="no-previous-run"
  if [ -n "${previous}" ] && [ -d "${previous}" ]; then
    if [ ! -d "${previous}/evidence/checkpoints/${id}" ]; then
      from=""
      status="incompatible"
    elif ! _evidence_checkpoint_complete "${previous}/evidence/checkpoints/${id}"; then
      from=""
      status="incompatible"
    else
      from="${previous}/evidence/checkpoints/${id}/normalized.json"
      status="available"
      previous_label="${id}"
    fi
  fi
  _evidence_compare "${from}" "${dir}/normalized.json" \
    "${dir}/comparison-to-previous-run.json" "${id}" \
    "${previous_label}" "${status}"
}

# evidence_capture writes one checkpoint: raw documents, the deterministic object
# aggregate, what moved since the previous checkpoint, and the observations that
# objects alone cannot show.
evidence_capture() { # <id> <description>
  local id="$1" description="${2:-}" checkpoints dir previous status rc=0
  [ -n "${id}" ] || {
    _evidence_fail "capture" "checkpoint id must not be empty"
    return 1
  }
  _evidence_tools || return 1
  [ -d "$(_evidence_checkpoints_dir)" ] || evidence_init || return 1
  checkpoints="$(_evidence_checkpoints_dir)"
  previous="$(_evidence_order_previous "${id}")"
  if [ -n "${previous}" ]; then
    status="available"
  else
    status="first-checkpoint"
  fi
  dir="${checkpoints}/${id}"
  mkdir -p "${dir}/parts" || {
    _evidence_fail "${id}" "cannot create ${dir}"
    return 1
  }
  _evidence_object_captures "${dir}" || rc=1
  _evidence_merge "${dir}" || rc=1
  _evidence_normalize "${dir}" "${id}" || rc=1
  _evidence_observations "${dir}" || rc=1
  _evidence_compare "${checkpoints}/${previous}/normalized.json" \
    "${dir}/normalized.json" "${dir}/changes-from-previous.json" "${id}" "${previous}" \
    "${status}" || rc=1
  _evidence_prior_compare "${dir}" "${id}" || rc=1
  if [ "${rc}" -eq 0 ]; then
    if ! _evidence_order_append "${id}"; then
      _evidence_fail "${id}" "cannot append ${id} to the checkpoint order file"
      rc=1
    fi
  fi
  if [ "${rc}" -eq 0 ]; then
    printf '[e2e] evidence: %s %s captured\n' "${id}" "${description}"
  fi
  return "${rc}"
}

# evidence_finalize compares this run's final object state with the final state of
# the run that held ${root}/latest before this one, then checksums every artifact.
# Comparison status stays explicit: available, no-previous-run, incompatible.
evidence_finalize() {
  # Every previous-run holder starts empty: collect.sh runs under `set -u`, and a
  # run without any earlier run must reach the compare call with all of them set.
  local checkpoints="" final="" target="" no_checkpoint="" previous_dir=""
  local previous_final="" previous_label="" status="" rc=0
  command -v jq > /dev/null 2>&1 ||
    _evidence_fail "jq" "required by evidence finalization but not available" ||
    return 1
  [ -d "$(_evidence_checkpoints_dir)" ] || evidence_init || return 1
  checkpoints="$(_evidence_checkpoints_dir)"
  final="$(_evidence_order_final)"
  previous_dir="${E2E_PREVIOUS_RUN_DIR:-}"
  status="no-previous-run"
  if [ -n "${previous_dir}" ] && [ -d "${previous_dir}" ]; then
    previous_final="$(_evidence_run_final_normalized "${previous_dir}")"
    if [ -n "${previous_final}" ] && _evidence_compatible "${previous_final}"; then
      status="available"
      previous_label="$(jq -r '.checkpoint // ""' "${previous_final}" 2> /dev/null || true)"
    else
      status="incompatible"
      previous_final=""
    fi
  else
    previous_dir=""
  fi
  if [ -n "${final}" ] && [ -f "${checkpoints}/${final}/normalized.json" ]; then
    target="${checkpoints}/${final}/normalized.json"
  else
    # Keep the cross-run comparison useful, but do not turn an aborted run into
    # a successful empty checkpoint. The temporary aggregate is removed before
    # the manifest is written, and the recorded error keeps latest unchanged.
    no_checkpoint="${E2E_ARTIFACTS_DIR}/evidence/.no-checkpoints.normalized.json"
    printf '{"schemaVersion": %s, "checkpoint": "none", "objects": []}\n' \
      "${EVIDENCE_SCHEMA_VERSION}" > "${no_checkpoint}" || return 1
    target="${no_checkpoint}"
    _evidence_fail "finalize" "no successful evidence checkpoints were captured" || rc=1
    final="none"
  fi
  _evidence_compare "${previous_final}" "${target}" \
    "${E2E_ARTIFACTS_DIR}/evidence/comparison-to-previous-run.json" \
    "${final}" "${previous_label}" "${status}" || rc=1
  if [ -n "${no_checkpoint}" ]; then
    rm -f "${no_checkpoint}" || rc=1
  fi
  _evidence_manifest || rc=1
  return "${rc}"
}

# _evidence_compatible and _evidence_prior_compare are defined above evidence_capture.
# _evidence_run_final_normalized finds the newest usable normalized aggregate of an
# earlier run through its order file, falling back to the directory order.
_evidence_run_final_normalized() { # <run-dir>
  local dir="$1" order last="" candidate
  order="${dir}/evidence/checkpoints/.order"
  if [ -e "${order}" ]; then
    if [ -s "${order}" ]; then
      while IFS= read -r candidate || [ -n "${candidate}" ]; do
        _evidence_checkpoint_complete \
          "${dir}/evidence/checkpoints/${candidate}" || continue
        last="${dir}/evidence/checkpoints/${candidate}"
      done < "${order}"
    fi
    [ -n "${last}" ] || return 1
    printf '%s\n' "${last}/normalized.json"
    return 0
  fi
  # Compatibility fallback for older artifact trees that predate .order.
  for candidate in "${dir}"/evidence/checkpoints/*/; do
    [ -d "${candidate}" ] || continue
    _evidence_checkpoint_complete "${candidate}" || continue
    last="${candidate}"
  done
  [ -n "${last}" ] || return 1
  printf '%s\n' "${last%/}/normalized.json"
}

# _evidence_manifest checksums every artifact of this run except the manifest
# itself, with a stable path order.
_evidence_manifest() {
  local manifest="${E2E_ARTIFACTS_DIR}/artifact-manifest.sha256"
  (
    cd -- "${E2E_ARTIFACTS_DIR}" &&
      find . -type f ! -name artifact-manifest.sha256 ! -name artifact-manifest.sha256.tmp -print0 \
        | LC_ALL=C sort -z \
        | xargs -0 sha256sum
  ) > "${manifest}.tmp" || {
    rm -f "${manifest}.tmp"
    _evidence_fail "manifest" "cannot checksum ${E2E_ARTIFACTS_DIR}"
    return 1
  }
  mv -f "${manifest}.tmp" "${manifest}" || return 1
  return 0
}

# kind, Multus, and KubeVirt end-to-end suite

This suite builds `kubevirt-ip-helper` from the checkout and exercises it in a disposable two-node kind cluster: one control-plane and one worker. It installs the real CNI and virtualization stack: kindnet, Multus thick, the bridge CNI plugin, and KubeVirt with QEMU software emulation. No production Go code or production manifest is replaced.

Two stack profiles run side by side. `E2E_STACK=current` is the default and reports whether the helper is compatible with a current stable stack; `E2E_STACK=dependency-era` mirrors the library generation in `go.mod` and is the attribution lane. `E2E_GROUP` selects `all`, `core`, `pool`, `lease`, `ha`, or `multipool`; invalid stack or group values are rejected before cluster mutation.

## Run

From the repository root:

```sh
./test/e2e/run.sh
```

A bare invocation runs `E2E_STACK=current E2E_GROUP=all`. CI shards the current stack while the compatibility lane keeps the bounded core:

```sh
E2E_STACK=current E2E_GROUP=core ./test/e2e/run.sh
E2E_STACK=current E2E_GROUP=pool ./test/e2e/run.sh
E2E_STACK=current E2E_GROUP=lease ./test/e2e/run.sh
E2E_STACK=current E2E_GROUP=ha ./test/e2e/run.sh
E2E_STACK=current E2E_GROUP=multipool ./test/e2e/run.sh
E2E_STACK=dependency-era E2E_GROUP=core ./test/e2e/run.sh
```

Required locally:

- Linux on `amd64`.
- `bash`, `kubectl`, GNU `timeout`, `jq`, and `sha256sum`. `jq` parses the generated reports, `sha256sum` verifies the artifact manifest.
- A running container runtime with enough capacity for kind, KubeVirt, and one 256 MiB guest. Hardware virtualization is not required.
- Outbound HTTPS access to GitHub, GHCR, Quay, and the Kubernetes registry.

The harness downloads checksum-pinned `kind`, `virtctl`, and CNI plugin assets into a per-profile cache, `${XDG_CACHE_HOME:-$HOME/.cache}/kubevirt-ip-helper-e2e/current` for the default lane and `${XDG_CACHE_HOME:-$HOME/.cache}/kubevirt-ip-helper-e2e/dependency-era` for the attribution lane, so the two lanes never overwrite each other's binaries or downloaded manifests. It does not add binaries to the checkout or system paths.

Each profile also owns its cluster and artifact names so a retained lane cannot collide with the other:

| Profile | kind cluster | Diagnostics directory |
| --- | --- | --- |
| `current` | `kubevirt-ip-helper-e2e-current` | `_artifacts/e2e/current` |
| `dependency-era` | `kubevirt-ip-helper-e2e-dependency-era` | `_artifacts/e2e/dependency-era` |

Non-`all` groups append the group to each name, for example `current-pool`. This prevents parallel group jobs from sharing a cluster, cache, or artifact directory.

Optional environment variables:

```sh
E2E_STACK=current
E2E_GROUP=all
E2E_CLUSTER_NAME=kubevirt-ip-helper-e2e-current
E2E_IMAGE=kubevirt-ip-helper:e2e
E2E_ARTIFACTS_ROOT=_artifacts/e2e/current
E2E_KEEP_CLUSTER=1
E2E_VM_BOOT_TIMEOUT=300
```

A cluster created by `run.sh` is deleted after diagnostics on success or failure; if bounded deletion itself fails, the report records `SUITE-CLUSTER-CLEANUP` and leaves the cluster for diagnosis. A compatible cluster that already existed is reused but never deleted by the harness. `E2E_KEEP_CLUSTER=1` retains a newly created cluster; resolve `${E2E_ARTIFACTS_ROOT}/latest` to the current run before reading its kubeconfig:

```sh
root=_artifacts/e2e/current
pointer="$(readlink "${root}/latest" 2> /dev/null || cat "${root}/latest")"
run="${root}/${pointer}"
export KUBECONFIG="${run}/kubeconfig"
```

## Pinned stacks

Every downloaded artifact, manifest, and image is pinned by an exact version plus its SHA256 or registry digest. The two lanes are enumerated in full; nothing is inherited from the other column.

| Component | `current` (default) | `dependency-era` |
| --- | --- | --- |
| kind | `v0.33.0`, Linux amd64 SHA256 `aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d` | `v0.20.0`, Linux amd64 SHA256 `513a7213d6d3332dd9ef27c24dab35e5ef10a04fa27274fe1c14d8a246493ded` |
| Kubernetes node | `kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed` | `kindest/node:v1.24.15@sha256:7db4f8bea3e14b82d12e044e25e34bd53754b7f2b0e9d56df21774e6f66a70ab` |
| Kubernetes | `v1.36.4` | `v1.24.15` |
| KubeVirt | `v1.9.0`; operator manifest SHA256 `f11307caafc3c23ffedf9887d8beb5a4419e2694da242fa68f63d1ec820de2e0` | `v0.54.0`; operator manifest SHA256 `b731e17f6d07bcc3bb02b7f68031f17040f4c9399164d082ee1bacd8cda754a4` |
| virtctl | `v1.9.0`, Linux amd64 SHA256 `40ede2ee37c98a1aeed71c9c219616a05247ce2be109e1edddf0477572e8b978` | `v0.54.0`, Linux amd64 SHA256 `a46bf15c4213520c0f969554e87c44e1f277214b1a7e4a460ec39ed72fc65164` |
| guest | `quay.io/kubevirt/cirros-container-disk-demo:v1.9.0@sha256:ebdb8d8b9b480f6ee7664ed3fdde8428767664f507d98f94090edeff04d7ebf2` | `quay.io/kubevirt/cirros-container-disk-demo:v0.54.0@sha256:00d08fb2f4f3dfc36b43fec4bc8d2d6fd71712377d029dc9927adf7e78ce80ec` |

Both lanes install the same CNI generation: bridge CNI plugins `v1.9.1`, Linux amd64 SHA256 `b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303`, and the Multus `v4.3.0` thick manifest, SHA256 `2d622f697809644a12704497bbf5c3256adc1e7f5b5504655e4993743651b585`, whose moving `snapshot-thick` image tag `bootstrap.sh` rewrites to `ghcr.io/k8snetworkplumbingwg/multus-cni:v4.3.0-thick@sha256:a922a39a78049991d03178c07afc45a198326481049bd7d84626097402fa14bb`. Every download URL, digest, name, address, and timeout lives in `versions.env`, which is what each profile reads; `KIH_GUEST_IMAGE` is the guest image for the selected profile.

Each lane answers a different question:

- **`current` is the default and the deployment-compatibility signal.** It runs the helper against a current stable kind, Kubernetes, and KubeVirt generation, which is what the helper actually has to survive in a live cluster. A green `current` job is the answer to "can this build be deployed".
- **`dependency-era` is the attribution lane.** It matches `go.mod` only: `kubevirt.io/api` and `kubevirt.io/client-go` `v0.54.0`, with the forced `k8s.io/apimachinery`, `k8s.io/client-go`, and `k8s.io/api` replacements at `v0.24.0`, so the API server stays in the 1.24 generation. When only one lane is green, that lane tells you whether a failure comes from this checkout's own logic or from the generation gap between the compiled client libraries and a modern cluster. It is a reference point for reading the other lane, not a target to upgrade toward.

No lane floats on a `latest` tag. Unattended CI must be able to re-run an old commit and get the same schema, CNI chain, and guest image, so every pin pairs a version with the digest of the bytes it downloads, and a version bump has to change its digest in the same commit. The published helper image is addressed the same way: CI builds the checkout under the short-SHA tag `${GITHUB_SHA:0:8}` and loads it straight into kind rather than resolving a moving tag.

## Topology and ordering

`bootstrap.sh` performs these gates in order:

1. Create the pinned kind cluster with one control-plane and one worker, then inspect the existing kindnet CNI configuration on every node.
2. Install the pinned bridge plugin in every node's `/opt/cni/bin`.
3. Apply the pinned Multus thick DaemonSet and verify that its generated master configuration chains to the retained kindnet delegate.
4. Create the bridge NetworkAttachmentDefinition `kubevirt-ip-helper/kubevirt-ip-helper-e2e` with no CNI IPAM.
5. Start a plain pod and an NAD-attached pod. Both must become Ready; the second must report interface `kihnet0` in `network-status`, and the node must contain bridge `br-kih-e2e`.
6. Delete both probes.
7. Install KubeVirt with `spec.configuration.developerConfiguration.useEmulation: true` already present on the first KubeVirt reconciliation.

The preflight deliberately finishes before KubeVirt installation. A broken kindnet-to-Multus chain is therefore classified as a CNI setup failure, before KubeVirt downloads and a software-emulated guest consume time.

`run.sh` then renders the E2E Kustomize overlay with:

```sh
kubectl kustomize --load-restrictor=LoadRestrictionsNone test/e2e/manifests
```

The unrestricted loader is required because the overlay imports `deployments/crds.yaml` and `deployments/deployment.yaml` as the production sources of truth. The render deletes only the unsupported `ServiceMonitor`, pins the locally loaded image, and attaches both helper replicas to the NAD as `kihnet0`. `manifests/vm.yaml` is the template for the `current` profile: `run.sh` renders it into that profile's artifact directory, substitutes the profile's `KIH_GUEST_IMAGE`, and rejects a render that still carries the template image, so a `dependency-era` run never boots the guest on the `current` Cirros image.

The IPPool is intentionally separate from the overlay. `run.sh` applies it only after both helper pod sandboxes prove that `kihnet0` exists. The pool contract is:

- network: `kubevirt-ip-helper/kubevirt-ip-helper-e2e`
- subnet/server: `10.77.0.0/24`, `10.77.0.2`
- allocation range: `10.77.0.100` through `10.77.0.110`
- bind interface: `kihnet0`

The guest has a normal masqueraded pod-network NIC for KubeVirt management and a bridge-bound Multus NIC with explicit MAC `02:00:00:00:00:11`. Its userdata locates the helper-served NIC by MAC, acquires DHCP there, and writes `E2E_DHCP_OK=<address>` plus DHCP event and router markers to the serial console.

## Assertions

A run fails on any missing invariant:

1. Both helper replicas are Ready, both contain `kihnet0`, and both report that interface in Multus `network-status`.
2. Exactly one pod has `kubevirtiphelper/leader=active`.
3. The `kubevirt-ip-helper-lock` Lease holder UUID is the UUID logged by that labelled pod.
4. The metrics Service has exactly one endpoint and it is the labelled leader pod.
5. The leader owns `10.77.0.2/24` on `kihnet0`, listens on UDP/67, and serves metrics.
6. Creating halted VM `e2e/e2e-vm` produces a VMNetCfg reservation and an IPPool allocation before any VMI exists.
7. The reservation is in range, belongs to the explicit MAC, has VMNetCfg status `OK`, and is reflected by the IPPool and VM metrics.
8. The first guest boot prints `E2E_DHCP_OK=<reserved address>` and matching DHCP/router event markers; deconfiguration markers may report `unset` before the successful bound event.
9. A stop/start cycle keeps the reservation and reacquires the same address.
10. Changing `leasetime` from 300 to 600 drives the live DHCP-pool reload path without losing the allocation, DHCP listener, or metrics; a subsequent guest boot gets the same address.
11. Changing `router` from `10.77.0.1` to `10.77.0.9` drives the full application-reinitialization path, updates the server interface and the router option observed by a real guest, and preserves its reservation. Restoring `10.77.0.1` repeats the path and restores the guest-visible option.
12. Deleting the active leader transfers both label and Lease UUID. Election, state reconstruction, and guest reacquisition share a 540-second maximum, but their absolute deadline is capped by the 600-second lease window started before the reload boot; VM stop latency therefore reduces the failover budget instead of moving it past lease expiry.
13. Deleting the VM removes its VMNetCfg, releases the IPPool allocation, returns metrics to used `0` / available `11`, and removes the VM metric.
14. The `pool` group fills all eleven addresses, refuses a twelfth allocation without disrupting an existing DHCP lease, exercises both duplicate-MAC refusal paths (VM precreation guard and VMNetCfg `ERROR` status), reclaims a released address through an explicit VMNetCfg request, and rejects an out-of-range address without changing pool accounting.
15. The `ha` group maintains a live reservation while replacing a follower, scaling from two replicas to one and back, bouncing the leader's `kihnet0`, and deleting both replicas simultaneously. Leadership, listener, address, metrics, pool state, and a real guest DHCP acquisition must converge after the total-loss transition.
16. The `lease` group shortens the lease, keeps the guest running, observes a subsequent DHCP client event on the serial console, and proves the reservation and accounting remain stable without assuming an exact BusyBox T1 timer.
17. The `multipool` group adds a second bridge attachment and helper interface, initializes an independent non-overlapping pool, proves a real VM gets a `10.78.0.x` lease from it, then deletes that IPPool while helpers are live. The second server address, pool metric, attachment, and interface must disappear while the primary pool stays healthy.

These assertions are the complete contract of the emulated suite and every executed item receives a stable case ID in `cases.jsonl`, `report.json`, and `report.txt`. The scope is deliberately narrower than physical deployment qualification: kind uses KubeVirt software emulation on Linux `amd64`, so this suite does not claim KVM acceleration, external or VLAN-backed L2 reachability, cross-node broadcast behavior, control-plane network partitions, clock skew, or non-`amd64` coverage. It verifies the assigned address and router DHCP option rather than every optional DHCP field. Those boundaries are explicit exclusions, not silently skipped test cases.

## Diagnostics

When cluster state, the kind binary, and a kubeconfig are available, `collect.sh` is invoked from the exit trap before deletion of a cluster owned by the run. Each command and the complete collection have hard deadlines, so best-effort diagnostic command failures are recorded without replacing an earlier test failure; failures to finalize required reports, evidence comparisons, or checksums do fail an otherwise successful run. The collection is written into the execution directory of the current run and contains:

- generated kubeconfig and rendered helper manifest
- cluster nodes, pods, API resources, and events
- Multus DaemonSet state and logs
- KubeVirt CR, workloads, and component logs
- helper Deployment, pods, Lease, Service, Endpoints, logs, and previous logs
- IPPool, VMNetCfg, VM, VMI, workload events, and virt-launcher logs
- leader interface, route, UDP socket, and labelled-leader metrics state
- the per-bootstrap-gate `bootstrap-cases.jsonl` journal
- kind-node CNI files, bridge state, and container-runtime information
- `console-*.log` for each guest boot and `10-guest-markers.txt` as a compact marker index

For a retained cluster in the default lane:

```sh
root=_artifacts/e2e/current
pointer="$(readlink "${root}/latest" 2> /dev/null || cat "${root}/latest")"
run="${root}/${pointer}"
export KUBECONFIG="${run}/kubeconfig"
kubectl get pods -A
kubectl -n kube-system get ds kube-multus-ds
kubectl -n kubevirt get kubevirt kubevirt -o yaml
kubectl -n kubevirt-ip-helper get pods,lease,endpoints
kubectl get ippool e2e-pool -o yaml
kubectl -n e2e get vm,vmnetcfg,vmi -o yaml
```

Delete it when finished:

```sh
${XDG_CACHE_HOME:-$HOME/.cache}/kubevirt-ip-helper-e2e/current/bin/kind delete cluster --name kubevirt-ip-helper-e2e-current
```

The attribution lane uses the same commands with `dependency-era` in place of `current`.

## Run history

Each execution gets its own directory under the profile's artifact root, so repeated local runs are compared instead of overwritten. `E2E_ARTIFACTS_ROOT` is the directory CI uploads, `E2E_ARTIFACTS_DIR` is the current run inside it, and `latest` is the pointer between them:

```text
_artifacts/e2e/current-core/                     # E2E_ARTIFACTS_ROOT
├── latest                                       # pointer to the newest successful run
└── runs/
    ├── 20260903T071422Z/                        # earlier execution
    └── 20260903T085931Z/                        # newest execution = E2E_ARTIFACTS_DIR
        ├── report.json
        ├── report.txt
        ├── bootstrap-cases.jsonl                    # bootstrap gate cases imported into report
        ├── bootstrap-journal-errors.txt               # empty normally; nonempty on journal append failure
        ├── artifact-manifest.sha256
        ├── diagnostics/                         # captured diagnostics for this run
        ├── evidence/checkpoints/01-bootstrap/
        │   ├── raw.json
        │   ├── normalized.json
        │   ├── changes-from-previous.json
        │   ├── comparison-to-previous-run.json
        │   └── observations.txt
        ├── evidence/capture-errors.txt
        └── evidence/comparison-to-previous-run.json
```

- The default run id is the UTC second plus the harness process id (`20260903T085931Z-18422`), so directory names sort in execution order while simultaneous starts remain isolated. An explicit `E2E_RUN_ID` is useful for external correlation, but it must be unique: selecting an existing nonempty run directory is rejected rather than overwriting history.
- `latest` is an atomically replaced one-line text pointer to the newest successful finalized run. An interrupted, unfinalized, failed, or incomplete-evidence run therefore never becomes the baseline, and the pointer survives `actions/upload-artifact` unchanged. Resolve it from the repository root:

```sh
root=_artifacts/e2e/current-core
pointer="$(readlink "${root}/latest" 2> /dev/null || cat "${root}/latest")"
run="${root}/${pointer}"
jq -r '.status, .exitCode' "${run}/report.json"
```

- Initialization reads the old `latest` value before writing any run data. If that directory still exists it becomes `E2E_PREVIOUS_RUN_DIR`; otherwise the variable stays empty. Only finalization publishes the new pointer, after reports, evidence comparison, and checksums are written. Earlier runs are never deleted, so the immutable baseline stays next to the new result.
- The numbered `collect.sh` captures, the generated `kubeconfig`, the rendered helper manifest, and the per-boot `console-*.log` files belong to the current run, and finalize mirrors them into `diagnostics/` so a single directory holds the whole execution. `artifact-manifest.sha256` indexes that run directory.

## Reports

Four files describe one execution, from machine-readable to human-readable, with a separate bootstrap journal for the pre-report gates:

| File | Contents |
| --- | --- |
| `report.json` | Schema-version-1 document assembled from every executed assertion and imported bootstrap gate |
| `cases.jsonl` | One JSON object per closed case or note, appended in execution order, so partial results survive an abrupt exit |
| `report.txt` | The same result as text, also echoed to standard output when the harness exits |
| `bootstrap-cases.jsonl` | One JSON object per bootstrap gate, written as each gate closes and imported into the report |

`report.json` keys:

| Key | Contents |
| --- | --- |
| `schemaVersion` | `1` |
| `suite`, `name`, `stack`, `group`, `cluster`, `image` | Identity of the lane: fixed suite name, artifact-name suffix, selected stack and group, kind cluster, helper image |
| `runId`, `startedAt`, `finishedAt`, `durationMs` | Run directory name, UTC bracket, elapsed milliseconds |
| `status`, `exitCode` | Overall `passed` or `failed` plus the exit status the harness returns |
| `counts` | `cases`, `passed`, `failed`, `notes`, `groups` |
| `artifacts` | Paths relative to the report directory: `root` (`../..`), `run` (`.`), then `reportJson`, `reportTxt`, `cases`, `bootstrapCases`, `bootstrapJournalErrors`, `manifest`, `evidence`, `diagnostics`, `previousRunDir` |
| `previousRun` | `null` when the profile had no earlier run, otherwise `run` (relative to this report directory), `caseCount`, `added`, `removed`, `statusChanged`, and the path of the evidence comparison |
| `cases` | Ordered array; each entry has `id`, `name`, `group`, `status`, `durationMs`, `detail` |
| `notes` | `name` and `detail` pairs recorded during the run, such as captured environment facts |

After `report_init` succeeds, reporting opens before the first test action and is finalized from the `EXIT` trap, so a failure in an early gate still yields a complete `report.json` containing exactly the assertions that ran, in order, each once. Bootstrap gates are journaled independently and imported into the same case stream; a journal append failure creates `bootstrap-journal-errors.txt` and makes bootstrap report import fail rather than silently accepting a missing gate. A failed evidence capture is reported as a failed case and also listed in `evidence/capture-errors.txt`, never silently skipped, so the counts always explain the exit code.
TERM, INT, and HUP are converted into a failed signal case before finalization so CI cancellation still leaves a report and evidence attempt; SIGKILL cannot be intercepted by a shell.

Because `report.txt` is echoed on the way out for both outcomes, the console and the CI job log already show the result without parsing anything. To read the stored files instead:

```sh
jq -r '.status, .exitCode, .durationMs' "${run}/report.json"
jq -r '.counts | to_entries[] | "\(.key)=\(.value)"' "${run}/report.json"
jq -r '.cases[] | [.id, .group, .status, (.durationMs | tostring), .detail] | @tsv' "${run}/report.json"
jq -r 'select(.kind == "case") | [.id, .status] | @tsv' "${run}/cases.jsonl"
cat "${run}/report.txt"
```

## Kubernetes evidence and checkpoint diffs

Each checkpoint is a named point in the suite where cluster state matters, recorded as five files under `evidence/checkpoints/<NN-name>/`:

- `raw.json` holds every captured Kubernetes document, keyed by its resource group and emitted in a fixed, lexically sorted group order. It preserves the complete API list envelopes and every returned object field; only JSON object-key ordering is canonicalized by `jq -S`.
- `normalized.json` is the deterministic projection of the same capture: `checkpoint` plus an `objects` array sorted by resource, namespace, and name. Each record preserves `spec`, `status`, finalizers, deletion state, labels, annotations, owners, generation, and UID while removing resource versions, managed fields, known server timestamps (including Lease heartbeat/acquire times and IPPool allocation update times), and the generated EndpointSlice last-change annotation. Consequently a cross-run comparison exposes both semantic drift and intentional object recreation instead of hiding a changed identity.
- `changes-from-previous.json` compares this checkpoint with the preceding checkpoint of the same run: `checkpoint`, `previousCheckpoint`, `status` (`available`, or `first-checkpoint` when there is no earlier checkpoint), `counts.before` and `counts.after`, then `added`, `removed`, and `changed` object keys — `changed` entries name the section that moved with its before and after values — plus a unified diff of the two normalized files. It shows the mutation a transition caused, such as the leader label moving between pods or an IPPool allocation count changing, without re-reading two full captures.
- `observations.txt` records what objects alone cannot show: guest console markers, every helper pod's interface address and route, and the labelled leader pod's UDP listener and metrics scrape.

The stored report, evidence index, and checkpoint observations use paths relative to the downloaded run directory (`.` for the run, `../..` for the artifact root, and `../../runs/<id>` for a prior run); they do not depend on the original checkout path.

The captured object groups include the full cluster CRD inventory, including both helper CRDs (`ippools.kubevirtiphelper.k8s.binbash.org` and `virtualmachinenetworkconfigs.kubevirtiphelper.k8s.binbash.org`) and their instances, plus the KubeVirt CR, VMs/VMIs, NADs, namespaces/events, helper Deployment/pods/Lease/Service/EndpointSlices, and workload pods. This makes CRD installation/removal, resource creation/deletion, spec/status changes, finalizer handshakes, owner references, labels, annotations, generations, UIDs, and pool accounting available for post-run inspection.

## Verify and compare runs

`artifact-manifest.sha256` lists sorted relative paths and checksums for every file in the run directory, evidence included, and is written last so it covers the whole directory:

```sh
cd "${run}" && sha256sum -c artifact-manifest.sha256
```

That makes a downloaded CI artifact self-checking before anyone reads it. To compare the newest run with the one before it:

```sh
pointer="$(readlink "${root}/latest" 2> /dev/null || cat "${root}/latest")"
now="${root}/${pointer}"
prev_ref="$(jq -r '.previousRun.run // empty' "${now}/report.json")"
if [ -n "${prev_ref}" ]; then
  prev="${now}/${prev_ref}"
  diff <(jq -r 'select(.kind == "case") | [.id, .status] | @tsv' "${prev}/cases.jsonl") \
       <(jq -r 'select(.kind == "case") | [.id, .status] | @tsv' "${now}/cases.jsonl")
else
  printf 'no previous completed run recorded\n'
fi
jq '.previousRun | {caseCount, added, removed, statusChanged}' "${now}/report.json"
```

For the cluster-state side of the same comparison, read the cross-run diff directly:

```sh
jq . "${now}/evidence/comparison-to-previous-run.json"
```

## CI

`.github/workflows/e2e-kind-kubevirt.yaml` runs the same `test/e2e/run.sh` entry point for pull requests and manual dispatches. Its matrix runs `dependency-era/core` plus `current/core`, `current/pool`, `current/lease`, `current/ha`, and `current/multipool`, with `fail-fast: false`. Each group derives isolated cluster, cache, image, and artifact names from its stack and group.

Each job builds the checkout image locally under the short commit SHA and loads it directly with `kind load docker-image`; it performs no registry login or pull. The job has a 50-minute ceiling, while the E2E step has a 40-minute execution budget so collection, bounded cluster cleanup, and report finalization still have time to finish.

The upload step is guarded by `if: always()`, so reports, evidence, and diagnostics are retained for a green job and for a red one alike: the step runs after the suite regardless of its exit status, uploads the whole artifact root `_artifacts/e2e/<stack>-<group>` (pointer file and every `runs/<id>/` directory with its reports, evidence, diagnostics, rendered inputs, and console logs), and keeps it for 14 days. Missing files only warn, so an artifact problem never overrides the suite verdict. Because `run.sh` itself prints `report.txt` on the way out, the job log already shows the case list for both outcomes; no follow-up step is needed to reproduce it, and no follow-up step can change the exit status the suite returned. Downloads unpack with the run-history root intact.

Before the suite starts, each CI lane makes a best-effort `gh`/GitHub Actions API lookup for the newest successful prior workflow run on the same branch and downloads that lane's artifact into the same history root. If no prior artifact exists or the API is unavailable, the run is still executed as the first baseline and reports `previousRun: null`. A successful completed run is the only one allowed to replace `latest`, so failed runs remain downloadable without becoming a comparison baseline.

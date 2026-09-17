# kind, Multus, and KubeVirt end-to-end suite

This suite builds `kubevirt-ip-helper` and its standalone admission webhook from the checkout and exercises them in a disposable three-node kind cluster: one control-plane and two workers. It installs the real CNI and virtualization stack—kindnet, Multus thick, the bridge CNI plugin, and KubeVirt with QEMU software emulation—on two owned shared-L2 data networks attached to every node. Each helper Deployment serves exactly one NAD; admission remains one shared service. No production Go code is replaced.

Two stack profiles are available. `E2E_STACK=current` is the default and reports whether the helper is compatible with a current stable stack; `E2E_STACK=dependency-era` mirrors the library generation in `go.mod` and is the attribution lane. `E2E_GROUP` selects `all`, `core`, `pool`, `lease`, `ha`, or `multipool`; invalid stack or group values are rejected before cluster mutation.

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
- `bash`, `kubectl`, GNU `timeout`, `jq`, `sha256sum`, and `python3`. Python 3 runs the stdlib-only DHCP pcap decoder; `jq` parses generated reports and `sha256sum` verifies the artifact manifest.
- A running Docker or Podman daemon with enough capacity for the three-node kind cluster, KubeVirt, the test fixtures, and guests. Hardware virtualization is not required.
- Outbound HTTPS access to GitHub, Docker Hub, GHCR, Quay, the Kubernetes registry, and the Go module proxy (both checkout images use the repository-root build context, and their builder stages download Go modules through `go mod download`).

The harness downloads checksum-pinned `kind`, `virtctl`, and CNI plugin assets into a per-profile cache, `${XDG_CACHE_HOME:-$HOME/.cache}/kubevirt-ip-helper-e2e/current` for the default lane and `${XDG_CACHE_HOME:-$HOME/.cache}/kubevirt-ip-helper-e2e/dependency-era` for the attribution lane, so the two lanes never overwrite each other's binaries or downloaded manifests. It does not add binaries to the checkout or system paths.

Each profile also owns its cluster and artifact names:

| Profile | kind cluster | Diagnostics directory |
| --- | --- | --- |
| `current` | `kubevirt-ip-helper-e2e-current` | `_artifacts/e2e/current` |
| `dependency-era` | `kubevirt-ip-helper-e2e-dependency-era` | `_artifacts/e2e/dependency-era` |

The owned shared data networks use fixed transport CIDRs `10.77.0.0/16` and `10.78.0.0/16`. Therefore only one retained or running E2E cluster may use a Docker or Podman daemon at a time, even when profile, group, cluster, cache, and artifact names differ. Run parallel lanes only on separate daemons or VMs. The CI matrix satisfies that requirement because each matrix job runs in its own VM.

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
New knobs: `E2E_PRED_SECONDS` (default 20; see Assertions), `E2E_LEASE_STORM_COUNT` (default 3) and `E2E_LEASE_STORM_WINDOW` (default 10, minutes), which bound the lease-storm scenario.

A cluster created by `run.sh` is deleted after diagnostics on success or failure; if bounded deletion itself fails, the report records `SUITE-CLUSTER-CLEANUP` and leaves the cluster for diagnosis. Its two owned data networks are removed only after that owned-cluster deletion succeeds. A compatible cluster that already existed is reused but never deleted by the harness, and its data networks are retained; `E2E_KEEP_CLUSTER=1` likewise retains a newly created cluster and its networks. Resolve `${E2E_ARTIFACTS_ROOT}/latest` to the current run before reading its kubeconfig:

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

Both lanes install the same CNI generation: bridge CNI plugins `v1.9.1`, Linux amd64 SHA256 `b98f74a0f8522f0a83867178729c1aa70f2158f90c45a2ca8fa791db1c76b303`, and the Multus `v4.3.0` thick manifest, SHA256 `2d622f697809644a12704497bbf5c3256adc1e7f5b5504655e4993743651b585`, whose moving `snapshot-thick` image tag `bootstrap.sh` rewrites to `ghcr.io/k8snetworkplumbingwg/multus-cni:v4.3.0-thick@sha256:a922a39a78049991d03178c07afc45a198326481049bd7d84626097402fa14bb`. The network fixture and three passive observers use `docker.io/nicolaka/netshoot:v0.14@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2`; its DNS sidecar uses `registry.k8s.io/coredns/coredns:v1.12.1@sha256:e8c262566636e6bc340ece6473b0eed193cad045384401529721ddbe6463d31c`. Every download URL, digest, name, address, and timeout lives in `versions.env`, which is what each profile reads; `KIH_GUEST_IMAGE` is the guest image for the selected profile.

Each lane answers a different question:

- **`current` is the default and the deployment-compatibility signal.** It runs the helper against a current stable kind, Kubernetes, and KubeVirt generation, which is what the helper actually has to survive in a live cluster. A green `current` job is the answer to "can this build be deployed".
- **`dependency-era` is the attribution lane.** It matches `go.mod` only: `kubevirt.io/api` and `kubevirt.io/client-go` `v0.54.0`, with the forced `k8s.io/apimachinery`, `k8s.io/client-go`, and `k8s.io/api` replacements at `v0.24.0`, so the API server stays in the 1.24 generation. When only one lane is green, that lane tells you whether a failure comes from this checkout's own logic or from the generation gap between the compiled client libraries and a modern cluster. It is a reference point for reading the other lane, not a target to upgrade toward.

No lane floats on a `latest` tag. Unattended CI must be able to re-run an old commit and get the same schema, CNI chain, and guest image, so every pin pairs a version with the digest of the bytes it downloads, and a version bump has to change its digest in the same commit. The helper and standalone webhook are built locally using `build/Dockerfile` and `build/Dockerfile.webhook`, respectively, both with repository root as build context. Each image is retagged with its full image content ID as `${repository}:e2e-${content-id}`, loaded into all three kind nodes, and verified there by content ID before deployment.

## Topology and ordering

`bootstrap.sh` performs these gates in order:

1. Create or reuse the pinned kind cluster with one control-plane and two workers.
2. Create or verify the two owned runtime data networks, attach every kind node to both, and make each new uplink part of the corresponding node-local bridge without changing the kind management route.
3. Inspect the existing kindnet CNI configuration on every node, install the pinned bridge plugin in every node's `/opt/cni/bin`, and apply the pinned Multus thick DaemonSet. Its generated master configuration must chain to the retained kindnet delegate.
4. Create the primary bridge NetworkAttachmentDefinition `kubevirt-ip-helper/kubevirt-ip-helper-e2e` with no CNI IPAM, then start the control-plane network-services Pod and its pinned CoreDNS sidecar. The fixtures provide `10.77.0.1`, `10.77.0.9`, `10.78.0.1`, off-subnet probe targets, and the primary and secondary DNS names.
5. Start the passive host-network observer DaemonSet on all three nodes and prove fixture/DNS reachability on each shared L2 segment. Start and remove plain and NAD-attached CNI probes; the latter must report `kihnet0` in `network-status` and its node must contain `br-kih-e2e`.
6. Install KubeVirt with `spec.configuration.developerConfiguration.useEmulation: true` already present on the first KubeVirt reconciliation.

The preflight deliberately finishes before KubeVirt installation. A broken kindnet-to-Multus chain or unreachable fixture is therefore classified as setup failure before a software-emulated guest consumes time.

`run.sh` next applies `deployments/crds.yaml` and waits for both helper CRDs to become `Established`, then renders the E2E Kustomize overlay:

```sh
kubectl kustomize --load-restrictor=LoadRestrictionsNone test/e2e/manifests
```

The unrestricted loader imports the production deployment manifests; CRDs are applied separately before the overlay. The overlay replaces the production example network identities with the E2E network pairs, one-NAD attachments, and locally loaded images while retaining the production resource names, probes, resources, scheduling, and environment. The optional `ServiceMonitor` is not installed in kind. Primary Deployment `helper-management` explicitly uses two replicas for HA qualification (the production per-network default is one), both attached only to the primary NAD as `kihnet0`. The ordinary first Deployment apply and normal rollout readiness are used—there is no forced restart or readiness bypass. `manifests/vm.yaml` is rendered into the profile artifact directory with the selected `KIH_GUEST_IMAGE`, and the render rejects an unreplaced template image. KubeVirt caps an inline `cloudInitNoCloud` userData at 2048 bytes and the guest observer script is larger, so the script lives in `manifests/guest-userdata.sh` and travels as the `userdata` key of the `kih-guest-userdata` Secret: `run.sh` creates that Secret before the first VM applies, the guest manifest references it through `cloudInitNoCloud.secretRef`, and the applied bytes are compared with the pinned file because the guest executes the Secret.

The primary IPPool is intentionally separate from the overlay and is applied only after both helper pod sandboxes prove that `kihnet0` exists. Its contract is:

- network: `kubevirt-ip-helper/kubevirt-ip-helper-e2e`
- subnet/server: `10.77.0.0/24`, `10.77.0.2`
- allocation range: `10.77.0.100` through `10.77.0.110`
- bind interface: `kihnet0`
- identity labels: `kubevirtiphelper/network=kubevirt-ip-helper-e2e` and `kubevirtiphelper/network-namespace=kubevirt-ip-helper`

The primary Lease is `kubevirt-ip-helper-lock-kubevirt-ip-helper-e2e`; Service `metrics-management` selects only this network's active leader. The `multipool` group temporarily creates a separate one-replica secondary helper that reuses the production `helper-storage` Deployment and `metrics-storage` Service names with NAD `kubevirt-ip-helper-e2e2` and Lease `kubevirt-ip-helper-lock-kubevirt-ip-helper-e2e2`. Both NADs and helper Deployments live in `kubevirt-ip-helper`; tenant VMs in `e2e` use qualified NAD references. The secondary helper attaches only to its own NAD, never as a second interface on the primary helper. Primary-only lanes do not deploy this secondary pair.

The shared standalone webhook is deployed and qualified alongside the helpers: ValidatingWebhookConfiguration `kubevirt-ip-helper-validator` targets `kubevirt-ip-helper-webhook.kubevirt-ip-helper.svc` on Service port `8080`, forwarding to listener `8443`. Its TLS Secret is `kubevirt-ip-helper-webhook-tls` and CSR name is `kubevirt-ip-helper-webhook.kubevirt-ip-helper.svc`. Admission has no helper network selector or Multus attachment, and its Service must never select helper pods. CREATE validates all VMNetCfg rows; main-resource UPDATE validates added or modified rows (all remaining rows when `spec.vmname` changes), preserving untouched foreign rows, and a VMNetCfg with an empty `metadata.name` or `spec.vmname` is admitted without row validation. `/status` writes remain outside the webhook rules. Explicit admission denials remain failures even where the registration uses `failurePolicy: Ignore`; the harness does not disable admission to submit an invalid fixture.

The guest has only the helper-served Multus bridge NIC with explicit MAC `02:00:00:00:00:11`. It uses stock persistent CirrOS DHCP on that NIC. The observation-only script in `manifests/guest-userdata.sh`, delivered through the `kih-guest-userdata` Secret, reports the guest's single non-loopback interface with the MAC the kernel assigned it and repeatedly writes a fresh `E2E_NET_SAMPLE` containing the native client, address, route, DNS, gateway, and routed-target observations; the runner compares that MAC with the one the helper reserved. The script neither configures networking nor invokes, replaces, signals, or restarts the DHCP client.

## Assertions

A run fails on any missing invariant:

1. The explicitly two-replica primary helper Deployment is Ready with `kihnet0` in each pod's Multus `network-status`; exactly one active-leader label for that network, matching network Lease holder, and `metrics-management` endpoint identify its leader.
2. The leader owns `10.77.0.2/24` on `kihnet0`, listens on UDP/67, and exposes the expected metrics through the Kubernetes Service. The suite does not install or qualify Prometheus Operator discovery.
3. Creating halted VM `e2e/e2e-vm` causes the controller to create an in-range VMNetCfg reservation, its cleanup finalizer, IPPool allocation, and VM metric before any VMI exists.
4. Before the guest starts, passive tcpdump captures run on all three observer nodes. The guest-node capture is decoded with the Python 3 stdlib decoder and proves a matching DHCP `REQUEST` followed by `ACK` for the real guest MAC and reservation, including address, mask, router, DNS, server ID, and lease duration.
5. Fresh native samples prove the same guest address, installed gateway, normal-resolver DNS result, and routed off-subnet target reachability. Samples are continuity observations, not synthetic DHCP event hooks.
6. A stop/start preserves the controller-created reservation and obtains the same address. A live IPPool lease change is then proven by a later native renewal: the pcap must contain a `REQUEST` with `ciaddr` equal to the reservation and the matching `ACK` with the restored lease duration. The client remains running; neither a client restart nor a forced acquisition is accepted.
7. Changing the primary router between `10.77.0.1` and `10.77.0.9` verifies the guest-visible route and preserves the reservation.
8. Deleting the active leader transfers label and Lease state while the original VM remains live. The unchanged VMI and native client must renew normally, retain healthy samples, and remain within the existing lease deadline; a cold-start check after failover is retained.
9. Deleting the VM removes its controller-created VMNetCfg, releases the IPPool allocation, returns Service-delivered metrics to used `0` / available `11`, and removes the VM metric.
10. The `pool` group fills all eleven addresses and refuses a twelfth without changing existing reservations. It retains the duplicate-VM ownership and owner's DHCP probes, including the duplicate VMNetCfg controller error path. After a normal VM deletion leaves one free slot, a raw invalid `.99` VMNetCfg without a finalizer must receive an explicit webhook denial during server-side dry-run, remain absent, and leave accounting unchanged; a transport failure or schema rejection is not accepted as admission proof. A new ordinary VM then receives the freed slot through a controller-created reservation and finalizer.
11. The `ha` group keeps a live VM through active-worker stop, follower replacement, normal one-to-two replica churn, and leader-interface bounce. The HA VM is placed on the surviving worker solely by its ordinary hostname `nodeSelector`; helper placement remains production-default. It retains identity, reservation, Service metrics, sampled network continuity, and a natural renewal while the other worker is stopped, then checks cold start again after total helper-pod loss.
12. The `lease` group shortens the lease, keeps the guest running, restores the normal lease, and proves the exact native renewal `REQUEST`/`ACK` and sampled continuity.
13. The `multipool` group rolls out a separate secondary helper/NAD/metrics pair, initializes an independent `10.78.0.x` pool and live guest, and creates a halted two-NIC VM whose single shared VMNetCfg retains both networks' reservations. A separate single-NIC primary guest boots through DHCP while the secondary guest remains live, then stays running through primary leader deletion and proves a natural renewal/ACK plus client and network continuity after recovery. Secondary packet and console recording continues throughout that primary operation: its native renewal and healthy samples must coexist with unchanged secondary Lease identity/transitions, metrics endpoint, and exact pool/VMNetCfg gauges. The shared VM remains halted because the unchanged guest observer requires one NIC; simultaneous data-plane proof comes from the two live single-NIC guests. Both guests and the shared VM are cleaned up, and both pools drain before removing secondary resources while the primary remains healthy; the suite never adds a second NAD to the primary helper.

These assertions are the complete contract of the emulated suite and every executed item receives a stable case ID in `cases.jsonl`, `report.json`, and `report.txt`. The scope is deliberately narrower than physical deployment qualification: kind uses KubeVirt software emulation on Linux `amd64`, so this suite does not claim KVM acceleration, external or VLAN-backed L2 reachability, control-plane network partitions, clock skew, non-`amd64` coverage, perfect packet-loss behavior, or throughput. It verifies the explicit DHCP options and guest observations above, not every optional DHCP field. Those boundaries are explicit exclusions, not silently skipped test cases.

Metric predicates resolve the requested Service's current nonterminating Ready leader on every call, verify ready EndpointSlice targets match that pod's UID and address, and fetch `/metrics` through that actual Service from the network-services Pod. `helper_service_metrics [service-name]` defaults to `KIH_METRICS_SERVICE` (`metrics-management`); secondary checks explicitly pass `metrics-storage`, rather than relying on cached failover globals. Every required series must occur exactly once and contain an integer value; missing, empty, or duplicate series fail. Separately, initialized IPPool API objects may omit zero-valued `used`, `available`, or `allocated` fields. Those omissions decode as `0`, `0`, and `{}` respectively; null or malformed values, missing initialization, out-of-range allocations, and inconsistent accounting still fail.

Every polling command has a deadline-clamped watchdog. A deadline cannot pass by taking a grace re-probe; an ownership-regression signature fails immediately rather than being converted into a later pass.

## Diagnostics

When cluster state, the kind binary, and a kubeconfig are available, `collect.sh` is invoked from the exit trap before deletion of a cluster owned by the run. Each command and the complete collection have hard deadlines, so best-effort diagnostic command failures are recorded without replacing an earlier test failure; failures to finalize required reports, evidence comparisons, or checksums do fail an otherwise successful run. The collection is written into the execution directory of the current run and contains:

- generated kubeconfig and rendered helper/webhook manifests
- cluster nodes, pods, API resources, and events
- Multus DaemonSet state and logs
- KubeVirt CR, workloads, and component logs
- expected primary and, during its scenario, secondary helper Deployments, pods, Leases, metrics Services, Endpoints/EndpointSlices, logs, and previous logs
- canonical standalone webhook Deployment/pods/logs, VWC rules and CA bundle, CSR, Service/Endpoints/EndpointSlices, and the TLS Secret's public certificate (never its private key or Secret annotations)
- IPPool, VMNetCfg, VM, VMI, workload events, the guest userdata Secret, and virt-launcher logs
- each network leader's identity, interfaces, routes, UDP sockets, and independently named network metrics Service scrapes
- the per-bootstrap-gate `bootstrap-cases.jsonl` journal
- kind-node CNI files, bridge state, runtime-network identities, and container-runtime information
- `console-*.log` and `10-guest-samples.txt`, the compact index of fresh native samples
- top-level `dhcp-*.pcap`, decoded `*.pcap.jsonl`, capture `*.pcap.stderr`, and `*.pcap.decode-errors` files for each passive observer capture

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

`kind delete cluster` does not remove the separately owned data networks. After deletion, inspect their ownership labels and remove only that retained cluster's now-unused `<cluster>-data` and `<cluster>-data2` networks with the same runtime. Do not force removal of attached networks.

The attribution lane uses the same commands with `dependency-era` in place of `current`.

## Run history

Each execution gets its own directory under the profile's artifact root, so repeated local runs are compared instead of overwritten. `E2E_ARTIFACTS_ROOT` is the directory CI uploads, `E2E_ARTIFACTS_DIR` is the current run inside it, and `latest` is the pointer between them:

```text
_artifacts/e2e/current-core/                     # E2E_ARTIFACTS_ROOT
├── latest                                       # pointer to the newest successful run
└── runs/
    └── 20260903T085931Z-18422/                  # E2E_ARTIFACTS_DIR
        ├── report.json
        ├── report.txt
        ├── bootstrap-cases.jsonl
        ├── artifact-manifest.sha256
        ├── 10-guest-samples.txt
        ├── dhcp-<label>-<node>.pcap
        ├── dhcp-<label>-<node>.pcap.jsonl
        ├── dhcp-<label>-<node>.pcap.stderr
        ├── dhcp-<label>-<node>.pcap.decode-errors
        ├── diagnostics/                         # collected text/log/YAML and kubeconfig
        └── evidence/
```

- The default run id is the UTC second plus the harness process id (`20260903T085931Z-18422`), so directory names sort in execution order while simultaneous starts remain isolated. An explicit `E2E_RUN_ID` is useful for external correlation, but it must be unique: selecting an existing nonempty run directory is rejected rather than overwriting history.
- `E2E_ARTIFACTS_DIR` is overridable only as `${E2E_ARTIFACTS_ROOT}/runs/${E2E_RUN_ID}`; any other value aborts at startup with a corrective message.
- `latest` is an atomically replaced one-line text pointer to the newest successful finalized run. An interrupted, unfinalized, failed, or incomplete-evidence run therefore never becomes the baseline, and the pointer survives `actions/upload-artifact` unchanged. Resolve it from the repository root:

```sh
root=_artifacts/e2e/current-core
pointer="$(readlink "${root}/latest" 2> /dev/null || cat "${root}/latest")"
run="${root}/${pointer}"
jq -r '.status, .exitCode' "${run}/report.json"
```

- Initialization reads the old `latest` value before writing any run data. If that directory still exists it becomes `E2E_PREVIOUS_RUN_DIR`; otherwise the variable stays empty. Only finalization publishes the new pointer, after reports, evidence comparison, and checksums are written. Earlier runs are never deleted, so the immutable baseline stays next to the new result.
- `diagnostics/` mirrors collected `.txt`, `.log`, and `.yaml` files plus the generated kubeconfig. Packet captures, their JSONL decodes, capture stderr, and decode errors deliberately remain top-level run artifacts; `artifact-manifest.sha256` covers all of them rather than treating a mirrored diagnostic copy as evidence.

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
The suite's `EXIT` and signal traps also tear down the guest console capture (`CONSOLE_PID`, `CONSOLE_FEEDER_PID`, and the FIFO), even when a boot assertion fails mid-way.

Because `report.txt` is echoed on the way out for both outcomes, the console and the CI job log already show the result without parsing anything. To read the stored files instead:

```sh
jq -r '.status, .exitCode, .durationMs' "${run}/report.json"
jq -r '.counts | to_entries[] | "\(.key)=\(.value)"' "${run}/report.json"
jq -r '.cases[] | [.id, .group, .status, (.durationMs | tostring), .detail] | @tsv' "${run}/report.json"
jq -r 'select(.kind == "case") | [.id, .status] | @tsv' "${run}/cases.jsonl"
cat "${run}/report.txt"
```

## Kubernetes evidence and checkpoint diffs

Each checkpoint records Kubernetes object state under `evidence/checkpoints/<NN-name>/`:

- `raw.json` preserves every captured Kubernetes document in a fixed, lexically sorted group order; only JSON object-key ordering is canonicalized by `jq -S`.
- `normalized.json` retains evidence schema version `1`: a deterministic projection of specs, status, finalizers, deletion state, labels, annotations, owners, generation, and UID while removing resource versions, managed fields, known server timestamps, and generated EndpointSlice timestamps. VWC rules, endpoint addresses/ports, and the projected public certificate remain available in raw evidence and live assertions, not dedicated semantic-history fields.
- `changes-from-previous.json` records the added, removed, and changed objects between consecutive checkpoints in the same run.
- `observations.txt` records guest `E2E_NET_SAMPLE` observations, every helper's network-labelled interface/route/listener observations, and a separately attributed scrape through every present metrics Service. The pcap, decoded JSONL, capture stderr, and decode errors remain top-level checksum-covered artifacts, not copies in checkpoint observations or `diagnostics/`.

The captured object groups include the full cluster CRD inventory, both helper CRDs and their instances, the KubeVirt CR, VMs/VMIs, NADs, namespaces/events, helper and webhook Deployments/pods/Leases/Services/Endpoints/EndpointSlices, canonical VWC and CSR, the projected TLS public certificate, workload pods, and workload namespace Secrets including guest userdata. Topology expectations cover the primary and expected secondary helper, not arbitrary discovered Deployments. The runner exports `E2E_SECOND_NETWORK_EXPECTED=1` during the secondary resource lifetime and clears it only after intentional removal; missing secondary resources then fail evidence, while their absence before creation, after cleanup, or in primary-only lanes is normal. `01-bootstrap` alone permits the not-yet-installed helper/webhook resources. Collector failures remain append-only in `evidence/capture-errors.txt`, and all artifacts are covered by the final checksum manifest. These records expose resource creation/deletion, network-specific leadership/routing, finalizer handshakes, owner references, labels, annotations, generations, UIDs, and pool accounting for post-run inspection.

## Verify and compare runs

`artifact-manifest.sha256` lists sorted relative paths and checksums for every file in the run directory—including the top-level pcap, JSONL, stderr, and decode-error evidence—and is written last so it covers the whole directory:

```sh
cd "${run}" && sha256sum -c artifact-manifest.sha256
```

That makes a downloaded CI artifact self-checking before anyone reads it. To inspect the newest run's recorded comparison with its previous baseline:

```sh
pointer="$(readlink "${root}/latest" 2> /dev/null || cat "${root}/latest")"
now="${root}/${pointer}"
jq '.previousRun | {caseCount, added, removed, statusChanged}' "${now}/report.json"
jq . "${now}/evidence/comparison-to-previous-run.json"
```

Prior-run diffs are informational evidence, not a regression verdict. Added, removed, or changed cases and objects may result from intentional suite-semantic changes, so compare only like-for-like runs when judging a regression.

## CI

`.github/workflows/e2e-kind-kubevirt.yaml` runs the same `test/e2e/run.sh` entry point for pull requests and manual dispatches. Its matrix runs `dependency-era/core` plus `current/core`, `current/pool`, `current/lease`, `current/ha`, and `current/multipool`, with `fail-fast: false`. Each job derives isolated cluster, cache, image, and artifact names from its stack and group, and runs in an isolated VM so its fixed shared-L2 CIDRs do not collide with another matrix lane.

Each job builds both checkout images locally, retags each with its full content ID, loads those exact references into every kind node, and performs no registry login or helper/webhook-image pull. The job has a 50-minute ceiling, while the E2E step has a 40-minute execution budget so collection, bounded cluster cleanup, and report finalization still have time to finish.

The upload step is guarded by `if: always()`, so reports, evidence, and diagnostics are retained for a green job and for a red one alike: the step runs after the suite regardless of its exit status, uploads the whole artifact root `_artifacts/e2e/<stack>-<group>` (pointer file and every `runs/<id>/` directory with reports, evidence, top-level packet artifacts, diagnostics, rendered inputs, and console logs), and keeps it for 14 days. Missing files only warn, so an artifact problem never overrides the suite verdict. Because `run.sh` itself prints `report.txt` on the way out, the job log already shows the case list for both outcomes; no follow-up step is needed to reproduce it, and no follow-up step can change the exit status the suite returned.

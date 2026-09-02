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
- `bash`, `kubectl`, GNU `timeout`, and either Docker or Podman.
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
E2E_ARTIFACTS_DIR=_artifacts/e2e/current
E2E_KEEP_CLUSTER=1
E2E_VM_BOOT_TIMEOUT=300
```

A cluster created by `run.sh` is deleted on success or failure. A compatible cluster that already existed is reused but never deleted by the harness. `E2E_KEEP_CLUSTER=1` retains a newly created cluster and preserves the generated kubeconfig under that profile's artifact directory.

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

The guest has a normal masqueraded pod-network NIC for KubeVirt management and a bridge-bound Multus NIC with explicit MAC `02:00:00:00:00:11`. Its userdata locates the helper-served NIC by MAC, acquires DHCP there, and writes `E2E_DHCP_OK=<address>` to the serial console.

## Assertions

A run fails on any missing invariant:

1. Both helper replicas are Ready, both contain `kihnet0`, and both report that interface in Multus `network-status`.
2. Exactly one pod has `kubevirtiphelper/leader=active`.
3. The `kubevirt-ip-helper-lock` Lease holder UUID is the UUID logged by that labelled pod.
4. The metrics Service has exactly one endpoint and it is the labelled leader pod.
5. The leader owns `10.77.0.2/24` on `kihnet0`, listens on UDP/67, and serves metrics.
6. Creating halted VM `e2e/e2e-vm` produces a VMNetCfg reservation and an IPPool allocation before any VMI exists.
7. The reservation is in range, belongs to the explicit MAC, has VMNetCfg status `OK`, and is reflected by the IPPool and VM metrics.
8. The first guest boot prints only `E2E_DHCP_OK=<reserved address>` markers.
9. A stop/start cycle keeps the reservation and reacquires the same address.
10. Changing `leasetime` from 300 to 600 drives the live DHCP-pool reload path without losing the allocation, DHCP listener, or metrics; a subsequent guest boot gets the same address.
11. Deleting the active leader transfers both label and Lease UUID. Election, state reconstruction, and guest reacquisition share a 540-second maximum, but their absolute deadline is capped by the 600-second lease window started before the reload boot; VM stop latency therefore reduces the failover budget instead of moving it past lease expiry.
12. Deleting the VM removes its VMNetCfg, releases the IPPool allocation, returns metrics to used `0` / available `11`, and removes the VM metric.
13. The `pool` group fills all eleven addresses, refuses a twelfth allocation without disrupting an existing DHCP lease, refuses a duplicate-MAC owner, reclaims a released address through an explicit VMNetCfg request, and rejects an out-of-range address without changing pool accounting.
14. The `ha` group replaces a follower, scales from two replicas to one and back, bounces the leader's `kihnet0`, deletes both replicas simultaneously, and requires leadership, listener, address, metrics, and pool state to converge after every transition.
15. The `lease` group shortens the lease, keeps the guest running, observes a subsequent DHCP client event on the serial console, and proves the reservation and accounting remain stable without assuming an exact BusyBox T1 timer.
16. The `multipool` group adds a second bridge attachment and helper interface, initializes an independent non-overlapping pool, allocates and releases from it, proves the primary pool remains unchanged, and restores the original one-interface topology.

## Diagnostics

`collect.sh` is invoked from the exit trap before deletion of a cluster owned by the run. Each command and the complete collection have hard deadlines, so diagnostics are best effort and never replace or indefinitely delay the original test exit status. The artifact directory contains:

- generated kubeconfig and rendered helper manifest
- cluster nodes, pods, API resources, and events
- Multus DaemonSet state and logs
- KubeVirt CR, workloads, and component logs
- helper Deployment, pods, Lease, Service, Endpoints, logs, and previous logs
- IPPool, VMNetCfg, VM, VMI, workload events, and virt-launcher logs
- leader interface, route, UDP socket, and metrics state
- kind-node CNI files, bridge state, and container-runtime information
- `console-*.log` for each guest boot and `10-guest-markers.txt` as a compact marker index

For a retained cluster in the default lane:

```sh
export KUBECONFIG=$PWD/_artifacts/e2e/current/kubeconfig
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

## CI

`.github/workflows/e2e-kind-kubevirt.yaml` runs the same `test/e2e/run.sh` entry point for pull requests and manual dispatches. Its matrix runs `dependency-era/core` plus `current/core`, `current/pool`, `current/lease`, `current/ha`, and `current/multipool`, with `fail-fast: false`. Each group derives isolated cluster, cache, image, and artifact names from its stack and group.

Each job builds the checkout image locally under the short commit SHA and loads it directly with `kind load docker-image`; it performs no registry login or pull. The job has a 45-minute ceiling, while the E2E step has a 40-minute execution budget so diagnostics still have time to upload under `if: always()`.

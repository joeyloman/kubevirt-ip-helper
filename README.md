# kubevirt-ip-helper

The kubevirt-ip-helper is a static DHCP solution for KubeVirt Virtual Machines which are attached to a bridged network using Multus. 
It stores it's IP reservations in Kubernetes/ETCD using it's own Custom Resource Definition (CRD) and serve them using it's 
internal DHCP service.

## Use case

This adds a static DHCP service to KubeVirt/Multus bridged networks and is integrated in the Kubernetes event mechanism.
The benefits in comparison to running a classic DHCP service in Kubernetes is that you don't have to use persistent volumes to store 
the lease database or run a seperate service outside your Kubernetes cluster.

Another use case, and this is the main reason why this project was started, is that when you have for example small IP ranges and/or 
limited available IP addresses in a range, you want to avoid that the pool gets exausted with unused IP leases from deleted Virtual 
Machines because they are not expired yet. When using a classic DHCP service you can solve this by putting the lease time very short 
so they will expire faster when they are not claimed anymore. However if a Virtual Machine is down for a certain amount of time and 
this exceeds the lease time the IP can be re-assigned to another Virtual Machine. This could be a problem when you run for example 
Kubernetes with ETCD in those Virtual Machines. ETCD members cannnot find each other anymore and the cluster won't come up.

The kubevirt-ip-helper application will solve this by controlling the following:

* IPs will be automatically assigned to KubeVirt Virtual Machines who are using configured Multus Network Attached Definition networks.
* IPs are always static and assigned to a specific Virtual Machine, also when the lease time is over they won't be released.
* IPs only are released when a Virtual Machine is deleted. This will be done immediately when a deletion is detected.

## How does the kubevirt-ip-helper work?

When KubeVirt Virtual Machines are created the kubevirt-ip-helper controllers picks them up and creates static DHCP reservations 
in the form of VirtualMachineNetworkConfiguration objects and then assign them to an IPPool so they will be picked up by 
the internal DHCP service. The following image gives an overview about the internals of the kubevirt-ip-helper:

![kubevirt-ip-helper](image/kubevirt-ip-helper.png)

## Prerequisites

The following components need to be installed/configured to use the kubevirt-ip-helper:

* Kubernetes
* KubeVirt
* Multus with bridge networking configured
* Auto MAC address registration such as kubemacpool or something simular

## Creating the Kubernetes Custom Resource Definitions (CRDs)

Execute the crd yaml files which are located in the crds directory of the Helm chart, for example:

```SH
kubectl create -f deployments/charts/kubevirt-ip-helper/crds/
```

## Building the container

There is a Dockerfile in the build directory which can be used to build the container, for example:

```SH
[docker|podman] build -f build/Dockerfile -t <DOCKER_REGISTRY_URI>/kubevirt-ip-helper:latest .
```

Then push it to the remote container registry target, for example:

```SH
[docker|podman] push  <DOCKER_REGISTRY_URI>/kubevirt-ip-helper:latest
```

Always use the repository root as the image build context, not `build/`.

Use helper and webhook images built from this revision with the multi-network
manifests. An older helper using the fixed shared Lease is not compatible with
the new topology.

## Deployment topology

Run one helper Deployment per served NetworkAttachmentDefinition (NAD), with
one replica by default and optional additional same-network HA replicas. All
helpers and NADs live in `kubevirt-ip-helper`; tenant VMs use qualified references
such as `kubevirt-ip-helper/management`. A network is the NAD namespace/name, not a
VLAN number. CNI supplies the interface and VLAN configuration: the helper does
not create VLAN devices, NADs or IPPools.

Configure the host bridge/trunk and existing NADs before starting helpers.
Each served NAD must represent a distinct DHCP broadcast domain; two NAD names
for the same segment must not become competing DHCP authorities. If Multus
namespace isolation is enabled, allow tenant VMs to use the shared infrastructure
namespace. One VM may have several networks but keeps one VMNetCfg in its own
namespace, with each helper managing only its network's rows.

Use **Helm OR plain YAML** to own the shared resources, never both. Adopting an
existing plain installation into Helm is a separate ownership migration; preserve
the shared webhook and durable reservation state. Never delete/recreate CRDs to
satisfy Helm ownership.

### Deploying with plain YAML

Edit the images and network-specific names/attachments in
`deployments/deployment.yaml` for your environment. The example provides:

| NAD | Helper Deployment | Metrics Service | Interface | Replicas |
| --- | --- | --- | --- | --- |
| `management` | `helper-management` | `metrics-management` | `net1` | 1 |
| `storage` | `helper-storage` | `metrics-storage` | `net1` | 1 |

Both pairs are in `kubevirt-ip-helper`, share the helper ServiceAccount/RBAC,
and retain container name `kubevirt-ip-helper`. Each pod has exactly one derived
attachment and `kubevirtiphelper/network` identity label. Keep that label,
Deployment selector, Service selector and attachment in agreement. Identity is
immutable for a running process: replace pods rather than editing their labels.
The plain manifests use `app` labels; do not substitute Helm's release selectors.

For a fresh installation after creating the CRDs and operator-managed NADs/pools:

```SH
kubectl apply -f deployments/deployment.yaml
kubectl apply -f deployments/webhook-deployment.yaml
# Optional, only if the monitoring.coreos.com ServiceMonitor CRD is installed:
kubectl apply -f deployments/servicemonitor.yaml
```

The base helper manifest does not require Prometheus. Its optional shared monitor
discovers both network metrics Services. Existing installations must instead
follow the stop-old/start-new cutover below.

### Deploying with Helm

Install one release in namespace `kubevirt-ip-helper`; the example names it
`kubevirt-ip-helper`. The chart enforces the namespace and canonical webhook
Service/ports because the singleton runtime pins its TLS/admission identity.
Do not install one release per network.

```SH
helm install kubevirt-ip-helper deployments/charts/kubevirt-ip-helper \
  --namespace kubevirt-ip-helper --create-namespace \
  --skip-crds -f my-values.yaml
```

The chart ships unchanged CRDs in `crds/`. Helm installs them only on a fresh
install and never upgrades or deletes them. The example uses `--skip-crds`
because they were installed using the separate CRD command above; omit that flag
only when Helm should perform the first CRD installation.

Example `my-values.yaml`:

```YAML
kubevirtiphelper:
  image:
    repository: <DOCKER_REGISTRY_URI>/kubevirt-ip-helper
    tag: "<IMAGE_TAG>"
  imagePullSecrets:
    - name: <REGISTRY_PULL_SECRET>
  networks:
    - name: management
      interface: net1
      deploymentName: helper-management
      metricsServiceName: metrics-management
      replicaCount: 1
    - name: storage
      interface: net1
      deploymentName: helper-storage
      metricsServiceName: metrics-storage
      replicaCount: 1
  serviceMonitor:
    enabled: true # Set false if the ServiceMonitor CRD is absent.
webhook:
  fullnameOverride: kubevirt-ip-helper-webhook
  image:
    repository: <DOCKER_REGISTRY_URI>/kubevirt-ip-helper-webhook
    tag: "<IMAGE_TAG>"
  imagePullSecrets:
    - name: <REGISTRY_PULL_SECRET>
  service:
    webhookServicePort: 8080
    webhookListenPort: 8443
```

Replace registry/tag/secret placeholders (or omit `imagePullSecrets` for a public
image). Each network requires its NAD `name`, `interface`, `deploymentName`
and `metricsServiceName`. Names are nonempty DNS labels, at most 63 characters;
NAD names must be unique, and Deployment/Service names must be unique within
their kind, including the shared webhook. Per-network names are rejected, never truncated
into collisions. Omitted per-network `replicaCount` means 1; explicit 0 remains 0
for staged cutover. The old global helper replica/autoscaling settings are gone.

Image, security, resources, scheduling, environment and annotations remain
shared helper settings. The one-NAD Multus annotation is derived from each
entry and the release namespace, never overridden by global `podAnnotations`.
The Helm chart keeps `app.kubernetes.io/name` and
`app.kubernetes.io/instance` selectors, adding the network label. One optional
ServiceMonitor selects all network metrics Services by the common release
labels, not a single network. A supplied ServiceAccount (`create: false`,
`name: existing-account`) still receives the chart's RoleBinding and
ClusterRoleBinding. External accounts must be named explicitly; this avoids
silently granting these bindings to the namespace's default account.

Both packaging paths use the same startup/liveness/readiness probes and graceful
shutdown. Keep the helper health/metrics listener on 8080 for these probes.
A healthy standby is ready without serving DHCP; each metrics Service selects
only its own `kubevirtiphelper/leader: active` pod. The per-network Lease is
`kubevirt-ip-helper-lock-<nad-name>` in `kubevirt-ip-helper`.

### Shared webhook ownership

The singleton webhook uses Service `kubevirt-ip-helper-webhook` in
`kubevirt-ip-helper`, Service port 8080 and listener 8443. Its TLS Secret is
`kubevirt-ip-helper-webhook-tls`; CSR and serving DNS identity derive from
`kubevirt-ip-helper-webhook.kubevirt-ip-helper.svc`.

The webhook creates `kubevirt-ip-helper-validator` at runtime; that
ValidatingWebhookConfiguration is not owned by Helm. Before a full teardown,
delete it while keeping the webhook available until this completes:

```SH
kubectl delete validatingwebhookconfiguration kubevirt-ip-helper-validator
```

The IPPool deletion entry has `failurePolicy: Fail`, so deleting pools while
admission is down would otherwise be rejected. Reinstallation recreates the
configuration and reuses the TLS Secret. A normal Helm upgrade preserves the
shared webhook and does not need this deletion.

### Migrating an existing installation

**The old `kubevirt-ip-helper-lock` and new per-network Leases do not exclude one
another. Never roll an old helper directly into the new topology or rely on
Helm upgrade ordering to prevent overlapping DHCP authorities.** Use new
Deployment names because an existing Deployment's selector is immutable.

1. Save current manifests, IPPool reservation status and VMNetCfg state.
   Preflight every pool against admission: the full IPv4 spec is revalidated
   even on metadata-only label updates. Repair invalid pools safely first or
   in the same validated update; adding labels cannot bypass this gate.
   Deploy the updated shared webhook before the new helpers. Stage every new
   helper at zero replicas, and verify its identity, one-NAD attachment,
   interface, VLAN configuration and network-to-pool mapping.
2. Scale the old helper to zero and wait until **all its pods have terminated**.
   Leave the shared webhook Deployment and Service running. Do not enable new
   leaders while any old helper can still serve.
3. Add both identity labels to pools and fully qualify their `spec.networkname`
   without discarding reservation status. Apply the new network Services.
   Preserve VMNetCfg state for startup replay.
4. Start each per-network Deployment (set its replicas to 1 or the desired HA
   count). Verify exactly one leader per network, the expected registered pool,
   and a real guest DHCP exchange on each network.
5. Check metrics endpoints select only their respective network leaders.
   After successful cutover remove the obsolete helper Deployment, metrics
   Service and old Lease; the obsolete Lease is not automatically deleted.

For the original plain deployment, the stop/wait boundary is:

```SH
kubectl -n kubevirt-ip-helper scale deployment/kubevirt-ip-helper --replicas=0
kubectl -n kubevirt-ip-helper wait --for=delete pod -l app=kubevirt-ip-helper --timeout=180s
# Only after that succeeds, and pool labels/Services are ready:
kubectl -n kubevirt-ip-helper scale deployment/helper-management deployment/helper-storage --replicas=1
```

For an old Helm installation, use its existing helper Deployment name and old
release selector instead; never include the webhook in the scale-down. Keep
new chart network replicas explicitly zero in the staging values and change
them only after the stop/wait boundary. This handover intentionally interrupts
DHCP rather than allowing unsafe overlap.

The normal migration assumes NADs already live in `kubevirt-ip-helper`.
Moving a legacy NAD from another namespace into `kubevirt-ip-helper` changes network
identity, not merely labels. Drain its allocations under the old helper,
recreate the NAD and pool centrally and update tenant references before
retiring the old helper. Do not relabel a live pool or rewrite live reservation
references across namespaces. Keep new helpers stopped until the boundary.

Rollback follows the same exclusion rule: stop **all** new helpers and wait for
their pods to terminate before restarting the old deployment with its original
attachments. Preserve the latest durable reservation state; never overwrite
new allocations with a stale backup.

## Usage

### Creating an IPPool object

First you need to create an IPPool object with the Network/DHCP configuration like in the example below. This will allocate a new IPAM subnet memory DB and starts a DHCP service on the bindinterface.

The following yaml/command example can be used to create a new IPPool object with a class b-subnet:

```SH
(
cat <<EOF
apiVersion: kubevirtiphelper.k8s.binbash.org/v1
kind: IPPool
metadata:
  name: <POOL_NAME>
  labels:
    kubevirtiphelper/network: management
    kubevirtiphelper/network-namespace: kubevirt-ip-helper
spec:
  ipv4config:
    serverip: 172.16.0.2
    subnet: 172.16.0.0/16
    pool:
      start: 172.16.0.10
      end: 172.16.255.250
      exclude:
        - 172.16.0.67
        - 172.16.100.154
        - 172.16.189.99
    router: 172.16.0.1
    dns:
      - 8.8.8.8
      - 8.8.4.4
    domainname: example.com
    domainsearch:
      - example.com
    ntp:
      - 0.pool.ntp.org
      - 1.pool.ntp.org
    leasetime: 300
  networkname: kubevirt-ip-helper/management
  bindinterface: net1
EOF
) | kubectl create -f -
```
> **_NOTE:_** Replace \<POOL_NAME> and adapt the IPv4 settings for your NAD.
> The cluster-scoped IPPool requires both identity labels, and `spec.networkname`
> must exactly equal that namespace/name. Its `bindinterface` must match the
> helper's attachment. Selected mismatches are rejected before serving DHCP.
> Do not move a live pool to another network by relabelling it; drain and recreate it.

Create a VM using the same shared network, for example this two-NIC excerpt:

```YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: example-vm
  namespace: tenant-a
spec:
  template:
    spec:
      domain:
        devices:
          interfaces:
            - name: nic-a
              macAddress: "02:00:00:00:01:01"
              bridge: {}
            - name: nic-b
              macAddress: "02:00:00:00:02:01"
              bridge: {}
      networks:
        - name: nic-a
          multus:
            networkName: kubevirt-ip-helper/management
        - name: nic-b
          multus:
            networkName: kubevirt-ip-helper/storage
```

VMs in `tenant-b` use the same qualified NAD references, not duplicated NADs.
An unqualified VM/VMNetCfg reference resolves in that object's namespace,
not the helper's namespace. Ownership is canonical network plus canonical MAC;
helpers preserve other networks' spec/status rows. During deletion each helper
drains its own rows; the shared cleanup finalizer is removed only after both
arrays are empty.

### Status information

Status information about the IP reservations are kept in the status fields in the ippool objects and in the vmnetcfg objects.

### Logging

By default only the startup, error and warning logs are enabled. More logging can be enabled by changing the LOGLEVEL environment setting in the kubevirt-ip-helper deployment. The supported loglevels are INFO, DEBUG and TRACE.

### Metrics

The following metrics are included in the application which can be used for monitoring:

```YAML
Name: kubevirtiphelper_ippool_used
Description: Amount of IP addresses which are in use in an IPPool.
```

```YAML
Dame: kubevirtiphelper_ippool_available
Description: Amount of IP addresses which are available in an IPPool.
```

```YAML
Name: kubevirtiphelper_vmnetcfg_status
Description: Information and status of the VirtualMachineNetworkConfig objects.
```

```YAML
Name: kubevirtiphelper_app_logs
Description: Amount of warnings or errors detected.
```

Metrics are exported on port 8080. The optional shared ServiceMonitor is in
`deployments/servicemonitor.yaml` or controlled by Helm's
`kubevirtiphelper.serviceMonitor.enabled`.

## The kubevirt-ip-helper-webhook

The kubevirt-ip-helper-webhook prevents deleting IPPools still in use and rejects
VirtualMachineNetworkConfig objects which duplicate a `(vmname, canonical MAC)`
claim from another object in the same namespace, regardless of network.

The IPPool deletion gate matches each allocation record against VMNetCfg spec
references using namespace, VM name, canonical network and canonical MAC,
including configs being deleted. A reference only on another network does not
keep an orphaned allocation blocking this pool. Records with no matching
reference are orphaned and do not block deletion. A failed cluster-wide lookup,
an unparseable owner reference or an ambiguous network identity remains blocking.

The ippool admission check rejects an IPPool spec whose ipv4 configuration cannot serve: a subnet which does not parse as an ipv4 prefix (the crd schema accepts spellings like 10.0.0.0/33), an allocation range outside the subnet, a pool end before its start, a pool end or exclude entry equal to the broadcast address of the subnet, a pool range larger than the helper's cap of 65536 addresses, or an exclude address outside the allocation range. The checks mirror the helper controller's own registration validation, so a projection the controller would register is never rejected - the controller accepts an off-subnet serverip, so the admission check deliberately does too. The helper controller rejects such a projection on its own sync as well, but only after the object is stored - on update the previously registered configuration keeps serving while the object carries the broken spec and the rejection is re-logged on every resync. Only fields which are present are validated, so an omitted optional field stays the controller's business.

The vmnetcfg admission check also rejects an explicit `ipaddress` which does not lie between the start and the end of the allocation range of the IPPool serving its `networkname`: the helper's controller refuses such an interface too, but only after the object is stored, leaving a permanent ERROR status whose rejection is re-logged on every retry. The range check only runs when an IPPool for the networkname exists - a vmnetcfg whose network has no pool yet is the intended ordering of a vm created before its pool, and the controller's ERROR-then-recover path is its observed contract.

The vmnetcfg admission check also rejects a `macaddress` which cannot serve as a source address (every multicast address and the broadcast address carry the individual/group bit). Unlike the other checks this one is deliberately stricter than the helper controller: it registers such a binding without a complaint, and the reservation then silently consumes the pool capacity because no guest interface can ever hold that macaddress. The check cannot reject a controller-created binding, since the macaddress of a vm interface is assigned through kubemacpool, which does not hand out multicast addresses.

Admission rejects a distinct VMNetCfg in the same namespace that claims the
same VM/MAC, even on another network; a different VM claiming an already-bound
MAC stays admissible and is refused by the owning helper with an ERROR status.
A VMNetCfg with an empty `metadata.name` or `spec.vmname` is admitted without
row validation. CREATE validates all claimed rows; main-resource UPDATE only added
or modified rows against OldObject, preserving unchanged foreign rows, and a
`spec.vmname` change revalidates every row. The entry uses
`failurePolicy: Ignore` and no namespace selector, so an admission outage never
blocks controller writes.

### Building the webhook container

The webhook lives in the same repository and is built from the Dockerfile.webhook file, for example:

```SH
[docker|podman] build -f build/Dockerfile.webhook -t <DOCKER_REGISTRY_URI>/kubevirt-ip-helper-webhook:latest .
```

Then push it to the remote container registry target, for example:

```SH
[docker|podman] push <DOCKER_REGISTRY_URI>/kubevirt-ip-helper-webhook:latest
```

### Deploying the webhook container

Use the webhook-deployment.yaml template which is located in the deployments directory, for example:

```SH
kubectl create -f deployments/webhook-deployment.yaml
```

### Webhook logging

By default only the startup, error and warning logs are enabled. More logging can be enabled by changing the LOGLEVEL environment setting in the kubevirt-ip-helper-webhook deployment. The supported loglevels are INFO, DEBUG and TRACE.

# License

Copyright (c) 2026 Joey Loman <joey@binbash.org>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

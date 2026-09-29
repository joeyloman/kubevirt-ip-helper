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

## Deploying the container

Use the deployment.yaml template which is located in the templates directory, for example:

```SH
kubectl create -f deployments/deployment.yaml
```

Before executing the above command, edit the deployment.yaml and:

Configure the Multus NetworkAttachmentDefinition name and namespace:
```YAML
spec:
  [..]
  template:
    metadata:
      annotations:
        k8s.v1.cni.cncf.io/networks: '[{ "interface":"eth1","name":"<NETWORKATTACHMENTDEFINITION_NAME>","namespace":"<NAMESPACE>" }]'
```

> **_NOTE:_** Make sure to replace the \<NETWORKATTACHMENTDEFINITION_NAME> and \<NAMESPACE> placeholders.

## Deploying with the Helm chart

The Helm chart in `deployments/charts/kubevirt-ip-helper` deploys both the kubevirt-ip-helper controller
and the kubevirt-ip-helper-webhook, including their RBAC, services and a ServiceMonitor.

The webhook binary hardcodes its service name and namespace (`kubevirt-ip-helper-webhook` in
`kubevirt-ip-helper`) when it registers its ValidatingWebhookConfiguration, so install the chart with
the release name `kubevirt-ip-helper` into a namespace named `kubevirt-ip-helper`:

```SH
kubectl create namespace kubevirt-ip-helper
helm install kubevirt-ip-helper deployments/charts/kubevirt-ip-helper \
  --namespace kubevirt-ip-helper \
  --skip-crds \
  -f my-values.yaml
```

The chart ships the CRDs in its `crds/` directory. Helm only installs them on a fresh install and
never upgrades or deletes them, so if the CRDs already exist in the cluster pass `--skip-crds`
(as above) or pre-apply them with `kubectl create -f deployments/charts/kubevirt-ip-helper/crds/`.

Create a values file to point the chart at your environment, for example:

```YAML
kubevirtiphelper:
  image:
    repository: <DOCKER_REGISTRY_URI>/kubevirt-ip-helper
    tag: "<IMAGE_TAG>"
  imagePullSecrets:
    - name: <REGISTRY_PULL_SECRET>
  podAnnotations:
    k8s.v1.cni.cncf.io/networks: '[{ "interface":"eth1","name":"<NETWORKATTACHMENTDEFINITION_NAME>","namespace":"<NAMESPACE>" }]'

webhook:
  image:
    repository: <DOCKER_REGISTRY_URI>/kubevirt-ip-helper-webhook
    tag: "<IMAGE_TAG>"
  imagePullSecrets:
    - name: <REGISTRY_PULL_SECRET>
```

> **_NOTE:_** Make sure to replace the \<DOCKER_REGISTRY_URI>, \<IMAGE_TAG>, \<REGISTRY_PULL_SECRET>,
> \<NETWORKATTACHMENTDEFINITION_NAME> and \<NAMESPACE> placeholders. The `podAnnotations` value must
> reference your existing Multus NetworkAttachmentDefinitions: the controller serves DHCP on the
> attached interfaces (`bindinterface` of the IPPools) and the pod needs one interface per IPPool,
> since the controller registers only one IPPool per bindinterface.

The ValidatingWebhookConfiguration `kubevirt-ip-helper-validator` is created at runtime by the
webhook itself and is not owned by the Helm release. Delete it before tearing the deployment down
(a normal `helm upgrade` keeps a serving webhook pod and does not need this):

```SH
kubectl delete validatingwebhookconfiguration kubevirt-ip-helper-validator
```

The IPPool deletion webhook entry uses failurePolicy Fail, so while the webhook pods are down the
deletion of any IPPool would otherwise be rejected. After the release is (re)installed the webhook
recreates the configuration and reuses its serving certificate from the
`kubevirt-ip-helper-webhook-tls` secret.

When the chart replaces an older deployment, the controller's leader lease (`kubevirt-ip-helper-lock`)
is still held by the identity of a removed pod: the new leader is only elected after the lease
expires (60 seconds by default), and the pools and DHCP services are (re)registered from that moment.
The pods run and report Running before that; the leader pod is recognizable by its
`kubevirtiphelper/leader: active` label, which the metrics service also selects on.

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
  networkname: <NAMESPACE>/<NETWORKATTACHMENTDEFINITION_NAME>
  bindinterface: eth1
EOF
) | kubectl create -f -
```
> **_NOTE:_** Make sure to replace the \<NAMESPACE>, \<NETWORKATTACHMENTDEFINITION_NAME> and \<POOL_NAME> placeholders.

Now create a Virtual Machine in the same network as the \<NETWORKATTACHMENTDEFINITION_NAME> to test if the DHCP service works.

The `serverip` and the `router` of a pool are the addresses of the helper's own interface and of the gateway: they must not lie inside the allocation range (`pool.start` to `pool.end`), because the allocator would hand an in-range address to a guest while the infrastructure still uses it. A pool whose `serverip` or `router` lies inside its allocation range is rejected - by the ippool admission check before the object is stored, and by the same check mirrored in the helper's own registration for objects which reach the controller anyway (for example when the webhook is not installed) - unless the address is also listed in `pool.exclude`, which reserves it explicitly. This is a compatibility change: a pool which previously served its own infrastructure address to a guest was accepted silently and is now rejected; fix such a pool by moving the address outside the allocation range or by adding it to the exclude list.

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

Metrics are exported on port 8080 by default. This can be changed by adding the METRICS_PORT environment variable in the deployment. The deployment example also contains a servicemonitor object which can be automatically picked up by the Prometheus monitoring solution.

## Operational limits and recovery

### Address release and reuse

IP addresses are released immediately when the deletion of a Virtual Machine is detected; a DHCPRELEASE or a DHCPDECLINE message from a guest does not free a controller reservation. The immediate release assumes that a removed guest stops using its address before it is reused: the controller cannot observe whether the guest of a deleted VirtualMachine has actually relinquished its interface, so the reuse of a released address must not race a lingering guest. Tear a guest's interface down together with its object.

### Orphaned cleanup after retry exhaustion

A force-deleted binding whose durable cleanup cannot complete - for example while the apiserver is unreachable - is retried with a bounded budget. When the budget is exhausted, the in-process state (the dhcp lease and the claim) is already released and exactly the persisted ledger record remains; no further retry is scheduled, so the address stays reserved until the next process era - a restart or a leader handoff - revalidates the ledger against the live objects during the pool registration and releases the record of a genuinely dead owner. Recovery procedure for a stuck reservation: restore apiserver availability, then restart the helper or trigger a leader handoff.

### Readiness semantics

The readiness probe reports process health, not serving authority: a standby which holds no leadership is Ready, and APP_RUNNING does not prove that every configured pool currently serves - a permanently rejected pool (for example an unregistrable projection) leaves the era running with that pool unserved. This is deliberate: readiness must not flap on apiserver outages or on a single bad pool. Inspect the per-pool status and the metrics instead when the serving state of every pool matters.

### Scale and cleanup budgets

No packet-load envelope has been measured: the dhcp server (a pinned library) starts one goroutine per received packet, known clients are logged per packet at the default log level, and the per-binding status metric deletion walks every stored series. Measure a representative concurrent-client workload with logging enabled before raising the scale, and bound the client count per helper accordingly.

On leadership loss the local NIC cleanup runs after an optional pod-unlabel apiserver call which is bounded at 30 seconds, so the host cleanup can be delayed by that budget on a slow apiserver. The watchdog which force-exits a wedged leader leaves the leadership lease to expire on its own (60 seconds) rather than releasing it, so a standby never acquires in parallel with a serving predecessor.

### Qualification status

The unit and race suites do not qualify real leader failover, CSR issuance, trusted admission TLS, certificate renewal or a Helm rollout: no live dual-instance failover or webhook installation test has been executed against a real cluster. Treat those paths as unqualified until a cluster-level qualification run is performed.

## The kubevirt-ip-helper-webhook

The kubevirt-ip-helper-webhook is a webhook service for the kubevirt-ip-helper which prevents deleting IPPools which are still in use and rejects VirtualMachineNetworkConfig objects which record a (vmname, macaddress) pair that another object of the same namespace already records.

The IPPool deletion gate blocks a deletion only while an allocation record is backed by a live VirtualMachineNetworkConfig: a record whose (namespace, vmname, macaddress) has no live object anymore - for example the record a deleted hand-created vmnetcfg without the cleanup finalizer leaves behind, which the helper itself only revalidates at its next service era - is orphaned and does not block the deletion. The lookup errs toward blocking: a failed cluster-wide list keeps every record blocking and an unparseable reference can never be proven orphaned.

The ippool admission check rejects an IPPool spec whose ipv4 configuration cannot serve: a subnet which does not parse as an ipv4 prefix (the crd schema accepts spellings like 10.0.0.0/33), an allocation range outside the subnet, a pool end before its start, a pool end or exclude entry equal to the broadcast address of the subnet, a pool range larger than the helper's cap of 65536 addresses, an exclude address outside the allocation range, or a `serverip` or `router` which lies inside the allocation range without also being excluded (the helper's interface address and the gateway are in use by the network infrastructure, and the allocator would otherwise hand them to a guest). The checks mirror the helper controller's own registration validation, so a projection the controller would register is never rejected - the controller accepts an off-subnet serverip, so the admission check deliberately does too. The helper controller rejects such a projection on its own sync as well, but only after the object is stored - on update the previously registered configuration keeps serving while the object carries the broken spec and the rejection is re-logged on every resync. Only fields which are present are validated, so an omitted optional field stays the controller's business.

The vmnetcfg admission check also rejects an explicit `ipaddress` which does not lie between the start and the end of the allocation range of the IPPool serving its `networkname`: the helper's controller refuses such an interface too, but only after the object is stored, leaving a permanent ERROR status whose rejection is re-logged on every retry. The range check only runs when an IPPool for the networkname exists - a vmnetcfg whose network has no pool yet is the intended ordering of a vm created before its pool, and the controller's ERROR-then-recover path is its observed contract.

The vmnetcfg admission check also rejects a `macaddress` which cannot serve as a source address (every multicast address and the broadcast address carry the individual/group bit). Unlike the other checks this one is deliberately stricter than the helper controller: it registers such a binding without a complaint, and the reservation then silently consumes the pool capacity because no guest interface can ever hold that macaddress. The check cannot reject a controller-created binding, since the macaddress of a vm interface is assigned through kubemacpool, which does not hand out multicast addresses.

The kubevirt-ip-helper controllers key the lease ownership on the `vmname` of the vmnetcfg spec and the DHCP allocator keys its lease map on the macaddress alone, so two objects carrying the same vm and macaddress are indistinguishable to them - on any network: contradictory specs of such objects oscillate the one lease between them on every resync while both report status OK. The vmnetcfg admission check rejects the second object at admission time. It deliberately only covers the same-vmname case: a different vmname claiming the macaddress of another vm stays admissible and is refused by the controller with an ERROR status. The vmnetcfg admission entry uses failurePolicy Ignore so an admission outage never blocks the controller's own vmnetcfg writes, and it carries no namespace selector so the objects of every namespace are guarded.

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

package service

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
	"github.com/joeyloman/kubevirt-ip-helper/pkg/util"
	log "github.com/sirupsen/logrus"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// vmNetCfgAPIPath is the apiserver path of the kubevirtiphelper v1 group.
// The vmnetcfg and ippool objects are read through the generic path of the
// core RESTClient: the webhook only serves read-only list calls, so it
// reuses the core clientset it already holds instead of constructing a
// second, generated typed clientset.
const vmNetCfgAPIPath = "/apis/kubevirtiphelper.k8s.binbash.org/v1"

type Handler struct {
	ctx         context.Context
	kubeConfig  string
	kubeContext string
	clientset   *kubernetes.Clientset
	httpServer  *http.Server
}

// maxPoolAddrs mirrors the pool size cap of the helper's ipam
// (ipam.MaxPoolAddrs): a range larger than the cap is rejected by the
// controller's registration validation and would only produce a
// permanently unregistrable object.
const maxPoolAddrs = 65536

// ipv4Broadcast computes the broadcast address of an ipv4 prefix.
func ipv4Broadcast(prefix netip.Prefix) netip.Addr {
	addr := prefix.Addr().As4()

	bits := prefix.Bits()
	var broadcast [4]byte
	for i := range 4 {
		maskByte := byte(0)
		if remaining := bits - i*8; remaining > 0 {
			if remaining >= 8 {
				maskByte = 0xFF
			} else {
				maskByte = byte(0xFF) << (8 - remaining)
			}
		}

		broadcast[i] = addr[i] | ^maskByte
	}

	return netip.AddrFrom4(broadcast)
}

// ipv4RangeLen returns the number of addresses of the inclusive ipv4
// range between start and end, mirroring the helper's v4RangeLen.
func ipv4RangeLen(start netip.Addr, end netip.Addr) uint64 {
	startOctets := start.As4()
	endOctets := end.As4()

	startUint := binary.BigEndian.Uint32(startOctets[:])
	endUint := binary.BigEndian.Uint32(endOctets[:])

	return uint64(endUint-startUint) + 1
}

func Register(ctx context.Context, kubeConfig string, kubeContext string) *Handler {
	return &Handler{
		ctx:         ctx,
		kubeConfig:  kubeConfig,
		kubeContext: kubeContext,
	}
}

func (h *Handler) Init() {
	config, err := util.GetKubeConfig(h.kubeConfig, h.kubeContext)
	if err != nil {
		log.Panicf("%s", err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panicf("%s", err.Error())
	}
	h.clientset = clientset
}

// listVirtualMachineNetworkConfigs lists the VirtualMachineNetworkConfig
// objects of one namespace.
func (h *Handler) listVirtualMachineNetworkConfigs(namespace string) (list *kihv1.VirtualMachineNetworkConfigList, err error) {
	raw, err := h.clientset.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("%s/namespaces/%s/virtualmachinenetworkconfigs", vmNetCfgAPIPath, namespace)).
		Do(context.TODO()).Raw()
	if err != nil {
		return
	}

	list = &kihv1.VirtualMachineNetworkConfigList{}
	if err = json.Unmarshal(raw, list); err != nil {
		return
	}

	return
}

// listIPPools lists the IPPool objects. the ippools are cluster-scoped and
// are read through the generic apiserver path like the vmnetcfg objects.
func (h *Handler) listIPPools() (list *kihv1.IPPoolList, err error) {
	raw, err := h.clientset.CoreV1().RESTClient().Get().
		AbsPath("/apis/kubevirtiphelper.k8s.binbash.org/v1/ippools").
		Do(context.TODO()).Raw()
	if err != nil {
		return
	}

	list = &kihv1.IPPoolList{}
	if err = json.Unmarshal(raw, list); err != nil {
		return
	}

	return
}

// listAllVirtualMachineNetworkConfigs lists the VirtualMachineNetworkConfig
// objects of every namespace.
func (h *Handler) listAllVirtualMachineNetworkConfigs() (list *kihv1.VirtualMachineNetworkConfigList, err error) {
	raw, err := h.clientset.CoreV1().RESTClient().Get().
		AbsPath(fmt.Sprintf("%s/virtualmachinenetworkconfigs", vmNetCfgAPIPath)).
		Do(context.TODO()).Raw()
	if err != nil {
		return
	}

	list = &kihv1.VirtualMachineNetworkConfigList{}
	if err = json.Unmarshal(raw, list); err != nil {
		return
	}

	return
}

// parseAllocationRef splits an allocation reference of the IPPool status
// ("namespace/vmname [macaddress]", the spelling the helper persists)
// into its components. the macaddress is canonicalized through net.ParseMAC
// so the dash and uppercase spellings of older revisions and hand-edited
// status match the live objects. references which do not parse report
// ok=false and must be treated as unprovably orphaned.
func parseAllocationRef(ref string) (namespace string, vmName string, hwAddr string, ok bool) {
	sep := strings.LastIndex(ref, " [")
	if sep < 0 {
		return "", "", "", false
	}

	hw, err := net.ParseMAC(strings.TrimSuffix(ref[sep+2:], "]"))
	if err != nil {
		return "", "", "", false
	}

	owner := ref[:sep]
	slash := strings.Index(owner, "/")
	if slash < 0 {
		return "", "", "", false
	}

	return owner[:slash], owner[slash+1:], hw.String(), true
}

// allocationOwnerKey identifies one network binding. An empty network in the
// index records an ambiguous live reference and blocks deletion conservatively
// for that owner and MAC on every network.
type allocationOwnerKey struct {
	namespace string
	vmName    string
	network   string
	hwAddr    string
}

type allocationOwnerIndex map[allocationOwnerKey]string

func buildAllocationOwnerIndex(list *kihv1.VirtualMachineNetworkConfigList) allocationOwnerIndex {
	index := allocationOwnerIndex{}

	for _, obj := range list.Items {
		if obj.Spec.VMName == "" {
			continue
		}

		for _, nc := range obj.Spec.NetworkConfig {
			if nc.MACAddress == "" {
				continue
			}

			hw, err := net.ParseMAC(nc.MACAddress)
			if err != nil {
				continue
			}

			key := allocationOwnerKey{
				namespace: obj.Namespace,
				vmName:    obj.Spec.VMName,
				network:   util.QualifyNetworkName(obj.Namespace, nc.NetworkName),
				hwAddr:    hw.String(),
			}
			if _, exists := index[key]; !exists {
				index[key] = obj.Name
			}
		}
	}

	return index
}

// evaluateIPPoolRecords splits the allocation records of an IPPool into the
// ones which block its deletion and the orphaned ones which do not. a
// record blocks when its owner/network/MAC tuple is backed by a live
// VirtualMachineNetworkConfig, when the index is unavailable, or when its
// owner or network is ambiguous. Only a provably orphaned record stops
// blocking. The returned slices are ordered by IP for deterministic denials.
func evaluateIPPoolRecords(allocated map[string]string, network string, index allocationOwnerIndex, indexAvailable bool) (blocking []string, orphaned []string) {
	ips := make([]string, 0, len(allocated))
	for ip := range allocated {
		ips = append(ips, ip)
	}
	sort.Strings(ips)
	network = util.QualifyNetworkName("", network)

	for _, ip := range ips {
		ref := allocated[ip]
		if ref == "EXCLUDED" {
			continue
		}

		if !indexAvailable {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %q", ip, ref))

			continue
		}

		namespace, vmName, hwAddr, ok := parseAllocationRef(ref)
		if !ok {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to the unparseable reference %q", ip, ref))

			continue
		}

		if network == "" {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %q (ambiguous pool network)", ip, ref))

			continue
		}

		key := allocationOwnerKey{namespace: namespace, vmName: vmName, network: network, hwAddr: hwAddr}

		if objName, live := index[key]; live {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %s (VirtualMachineNetworkConfig %s/%s)", ip, ref, namespace, objName))

			continue
		}

		key.network = ""
		if objName, ambiguous := index[key]; ambiguous {
			blocking = append(blocking, fmt.Sprintf("ip %s is allocated to %s (VirtualMachineNetworkConfig %s/%s has an ambiguous network)", ip, ref, namespace, objName))

			continue
		}

		orphaned = append(orphaned, fmt.Sprintf("ip %s (%s)", ip, ref))
	}

	return blocking, orphaned
}

// validateIPPool rejects the deletion of an IPPool whose allocation records
// are still backed by a live VirtualMachineNetworkConfig. an allocation
// record whose owner tuple no longer has any live object - for example the
// record a deleted hand-created vmnetcfg without the cleanup finalizer
// leaves behind, which the helper only revalidates at its next service era -
// is orphaned and does not block the deletion: without this the pool stays
// undeletable until a leader restart.
//
// the lookup errs toward blocking: a failed cluster-wide list keeps every
// record blocking (the gate is then exactly the old one), and an unparseable
// reference can never be proven orphaned either. only a record whose
// (namespace, vmname, canonical network, canonical macaddress) matches no
// live object stops blocking.
func (h *Handler) validateIPPool(ar *admissionv1.AdmissionReview, pool *kihv1.IPPool) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	index := allocationOwnerIndex{}
	indexAvailable := true

	if len(pool.Status.IPv4.Allocated) > 0 {
		list, err := h.listAllVirtualMachineNetworkConfigs()
		if err != nil {
			indexAvailable = false
			log.Errorf("(service.validateIPPool) cannot list the VirtualMachineNetworkConfigs, every allocation record of IPPool %s blocks the deletion: %s",
				pool.Name, err.Error())
		} else {
			index = buildAllocationOwnerIndex(list)
		}
	}

	blocking, orphaned := evaluateIPPoolRecords(pool.Status.IPv4.Allocated, pool.Spec.NetworkName, index, indexAvailable)

	if len(blocking) > 0 {
		log.Warnf("(service.validateIPPool) denying the deletion of IPPool %s: %s", pool.Name, strings.Join(blocking, "; "))

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: fmt.Sprintf("ippool is still in use: %s", strings.Join(blocking, "; ")),
			},
		}
	}

	if len(orphaned) > 0 {
		log.Warnf("(service.validateIPPool) IPPool %s carries allocation records without a live VirtualMachineNetworkConfig (%s); they do not block the deletion",
			pool.Name, strings.Join(orphaned, ", "))
	}

	return allow
}

// findRecordedTuple reports whether another object of the list records the
// (vmname, macaddress) pair of one of the network interfaces of the
// admitted object, and returns the denial message naming the conflicting
// object and both networks. The single-object ownership policy deliberately
// remains network-agnostic: a distinct object cannot claim the same VM/MAC
// even on another network. A shared object never conflicts with itself;
// a different vmname claiming the MAC of another VM stays admissible.
func findRecordedTuple(obj *kihv1.VirtualMachineNetworkConfig, list *kihv1.VirtualMachineNetworkConfigList) (denied *string) {
	for _, nc := range obj.Spec.NetworkConfig {
		if nc.MACAddress == "" {
			continue
		}

		for _, other := range list.Items {
			if other.Name == obj.Name || other.Spec.VMName != obj.Spec.VMName {
				continue
			}

			for _, onc := range other.Spec.NetworkConfig {
				if onc.MACAddress == nc.MACAddress {
					msg := fmt.Sprintf(
						"vmname %s is already recorded with macaddress %s by VirtualMachineNetworkConfig %s/%s (network %s): distinct objects cannot claim the same vm and macaddress, regardless of network",
						obj.Spec.VMName, nc.MACAddress, other.Namespace, other.Name, onc.NetworkName,
					)

					return &msg
				}
			}
		}
	}

	return nil
}

// validateVmNetCfgMACAddresses rejects a network interface whose
// macaddress cannot serve as a source address: a macaddress with the
// individual/group bit set (every multicast address, and the broadcast
// address) can never be held by a guest interface.
//
// this check is deliberately stricter than the helper controller, unlike
// the mirroring checks: the controller registers such a binding without a
// complaint (observed live: a multicast macaddress allocated an address
// with status OK), and the reservation then silently consumes the pool
// capacity because no guest can ever claim it - deleting the object leaks
// the record like any hand-created vmnetcfg. the check cannot reject a
// controller-created binding: the macaddress of a vm interface is assigned
// through kubemacpool, which does not hand out multicast addresses.
func (h *Handler) validateVmNetCfgMACAddresses(obj *kihv1.VirtualMachineNetworkConfig) (denied *string) {
	for _, nc := range obj.Spec.NetworkConfig {
		if msg := checkNICMACAddress(nc); msg != nil {
			return msg
		}
	}

	return nil
}

// checkNICMACAddress rejects the macaddress of one network interface when
// it cannot serve as a source address: a macaddress with the
// individual/group bit set (every multicast address, and the broadcast
// address) can never be held by a guest interface.
func checkNICMACAddress(nc kihv1.NetworkConfig) (denied *string) {
	if nc.MACAddress == "" {
		return nil
	}

	hw, err := net.ParseMAC(nc.MACAddress)
	if err != nil {
		msg := fmt.Sprintf("the macaddress %s of network %s does not parse", nc.MACAddress, nc.NetworkName)

		return &msg
	}

	if hw[0]&0x01 != 0 {
		msg := fmt.Sprintf("the macaddress %s of network %s has the multicast bit set: no interface can hold it as its source address, so the reservation could never serve a guest and would only consume the allocation", nc.MACAddress, nc.NetworkName)

		return &msg
	}

	return nil
}

// validateVmNetCfgIPAddresses rejects the explicit ipaddress of a
// VirtualMachineNetworkConfig which does not lie between the start and the
// end of the allocation range of the IPPool serving its networkname. the
// controller of the helper refuses such an interface too, but only after the
// object is stored: the nic is recorded with a permanent ERROR status and
// its rejection is re-logged on every rate-limited retry for the lifetime
// of the object (observed live with an out-of-range address re-attempted
// every few seconds). denying it at admission keeps the invalid object out
// of the cluster entirely.
//
// the check only runs when an IPPool for the networkname exists: a vmnetcfg
// whose network has no pool yet is the intended ordering of a vm created
// before its pool, and the controller's ERROR-then-recover path (the failed
// nic is re-attempted and converges to OK once the pool appears) is its
// observed contract - the admission check must not break it. internal
// failures fail open like the other vmnetcfg checks: the controller's own
// range validation stays the authoritative guard.
func (h *Handler) validateVmNetCfgIPAddresses(obj *kihv1.VirtualMachineNetworkConfig) (denied *string) {
	lookupNeeded := false
	for _, nc := range obj.Spec.NetworkConfig {
		if nc.IPAddress != "" && nc.NetworkName != "" {
			lookupNeeded = true

			break
		}
	}

	if !lookupNeeded {
		return nil
	}

	pools, err := h.listIPPools()
	if err != nil {
		log.Errorf("(service.validateVmNetCfgIPAddresses) cannot list the IPPools, allowing the request: %s", err.Error())

		return nil
	}

	poolByNetwork := map[string]*kihv1.IPPool{}
	for i := range pools.Items {
		network := util.QualifyNetworkName("", pools.Items[i].Spec.NetworkName)
		if network == "" {
			continue
		}

		if _, exists := poolByNetwork[network]; !exists {
			poolByNetwork[network] = &pools.Items[i]
		}
	}

	for _, nc := range obj.Spec.NetworkConfig {
		if msg := checkNICIPAddress(nc, poolByNetwork[util.QualifyNetworkName(obj.Namespace, nc.NetworkName)]); msg != nil {
			return msg
		}
	}

	return nil
}

// checkNICIPAddress rejects the explicit ipaddress of one network
// interface when it does not lie between the start and the end of the
// allocation range of the IPPool serving its networkname. a nil pool (no
// IPPool serves the network yet) and a pool whose range does not parse are
// both allowed: the first is the intended ordering of a vm created before
// its pool, the second is the ippool controller's own projection rejection
// to handle.
func checkNICIPAddress(nc kihv1.NetworkConfig, pool *kihv1.IPPool) (denied *string) {
	if nc.IPAddress == "" || nc.NetworkName == "" || pool == nil {
		return nil
	}

	ip, err := netip.ParseAddr(nc.IPAddress)
	if err != nil || !ip.Is4() {
		msg := fmt.Sprintf("ipaddress %s of network %s does not parse as an ipv4 address (IPPool %s)",
			nc.IPAddress, nc.NetworkName, pool.Name)

		return &msg
	}

	start, startErr := netip.ParseAddr(pool.Spec.IPv4Config.Pool.Start)
	end, endErr := netip.ParseAddr(pool.Spec.IPv4Config.Pool.End)
	if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() {
		return nil
	}

	if ip.Compare(start) < 0 || ip.Compare(end) > 0 {
		msg := fmt.Sprintf("ipaddress %s is not between the pool range %s..%s of network %s (IPPool %s): the controller would record the network interface with a permanent ERROR status",
			nc.IPAddress, pool.Spec.IPv4Config.Pool.Start, pool.Spec.IPv4Config.Pool.End, nc.NetworkName, pool.Name)

		return &msg
	}

	return nil
}

// changedNetworkConfigs subtracts complete stored rows as a multiset, rather
// than matching by position or canonical identity. Reorders and removals need
// no validation; each extra duplicate or modified row does. Changing the VM
// owner invalidates every remaining row's previous admission.
func changedNetworkConfigs(obj, old *kihv1.VirtualMachineNetworkConfig) []kihv1.NetworkConfig {
	if obj.Spec.VMName != old.Spec.VMName {
		return obj.Spec.NetworkConfig
	}

	remaining := make(map[kihv1.NetworkConfig]int, len(old.Spec.NetworkConfig))
	for _, nc := range old.Spec.NetworkConfig {
		remaining[nc]++
	}
	var changed []kihv1.NetworkConfig
	for _, nc := range obj.Spec.NetworkConfig {
		if remaining[nc] > 0 {
			remaining[nc]--
		} else {
			changed = append(changed, nc)
		}
	}
	return changed
}

// validateVmNetCfg validates all CREATE rows, but only added/modified UPDATE
// rows, leaving unchanged foreign rows untouched. Ownership changes to a
// nonempty vmname revalidate all rows. MAC/IP and distinct-object duplicate
// guards share that selection. An object whose metadata.name or spec.vmname is
// empty keeps the baseline exemption and is admitted without row validation.
// Internal lookup failures retain the existing fail-open policy; controller
// guards remain authoritative.
func (h *Handler) validateVmNetCfg(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	obj := &kihv1.VirtualMachineNetworkConfig{}
	if err := json.Unmarshal(ar.Request.Object.Raw, obj); err != nil {
		log.Errorf("cannot unmarshal json to vmnetcfg: %s", err)

		return allow
	}

	if obj.Name == "" || obj.Spec.VMName == "" {
		return allow
	}

	if ar.Request.Operation == admissionv1.Update {
		old := &kihv1.VirtualMachineNetworkConfig{}
		if err := json.Unmarshal(ar.Request.OldObject.Raw, old); err != nil {
			// Without a usable old object no row can be proven unchanged.
			log.Errorf("cannot unmarshal old vmnetcfg, validating every row: %s", err)
		} else {
			obj.Spec.NetworkConfig = changedNetworkConfigs(obj, old)
		}
	}
	if len(obj.Spec.NetworkConfig) == 0 {
		return allow
	}

	if msg := h.validateVmNetCfgMACAddresses(obj); msg != nil {
		log.Warnf("(service.validateVmNetCfg) denying VirtualMachineNetworkConfig %s/%s: %s",
			obj.Namespace, obj.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}

	if msg := h.validateVmNetCfgIPAddresses(obj); msg != nil {
		log.Warnf("(service.validateVmNetCfg) denying VirtualMachineNetworkConfig %s/%s: %s",
			obj.Namespace, obj.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}
	list, err := h.listVirtualMachineNetworkConfigs(obj.Namespace)
	if err != nil {
		log.Errorf("cannot list the VirtualMachineNetworkConfigs of namespace %s, allowing the request: %s",
			obj.Namespace, err.Error())

		return allow
	}

	if msg := findRecordedTuple(obj, list); msg != nil {
		log.Warnf("(service.validateVmNetCfg) denying VirtualMachineNetworkConfig %s/%s: %s",
			obj.Namespace, obj.Name, *msg)

		return &admissionv1.AdmissionResponse{
			UID:     ar.Request.UID,
			Allowed: false,
			Result: &metav1.Status{
				Message: *msg,
			},
		}
	}

	return allow
}

func (h *Handler) validateIPPoolAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	pool := &kihv1.IPPool{}
	if err := json.Unmarshal(ar.Request.OldObject.Raw, &pool); err != nil {
		log.Errorf("cannot unmarshal json to pool: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot unmarshal json to pool: %s", err)
	}

	ar.Response = h.validateIPPool(ar, pool)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

// evaluateIPPoolSpec returns the sorted problems of an ipv4 configuration
// which cannot serve: a subnet which does not parse as an ipv4 prefix, a
// pool start or pool end outside the subnet, a pool end before its start,
// a pool end or exclude entry equal to the broadcast address of the
// subnet, a pool range larger than the cap, and an exclude address outside
// the subnet or the allocation range. the checks mirror the controller's
// own registration validation so a projection the controller would
// register is never rejected: the serverip is parse-checked only, exactly
// like the controller's projection validation. only fields which are
// present are validated.
func evaluateIPPoolSpec(cfg kihv1.IPv4Config) (problems []string) {
	var prefix netip.Prefix
	subnetParses := false
	if cfg.Subnet != "" {
		p, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil || !p.Addr().Is4() {
			problems = append(problems, fmt.Sprintf("the subnet %q does not parse as an ipv4 prefix", cfg.Subnet))
		} else {
			prefix = p
			subnetParses = true
		}
	}

	// the serverip is parse-checked like the controller's own projection
	// validation, but its placement is deliberately not contained to the
	// subnet: the controller registers an off-subnet serverip without a
	// complaint (it only parses the address), so an admission containment
	// check here would reject a projection the controller serves
	if cfg.ServerIP != "" {
		if addr, err := netip.ParseAddr(cfg.ServerIP); err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the serverip %q does not parse as an ipv4 address", cfg.ServerIP))
		}
	}

	var rangeStart, rangeEnd netip.Addr
	rangeParses := true
	if cfg.Pool.Start != "" {
		addr, err := netip.ParseAddr(cfg.Pool.Start)
		if err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the pool start %q does not parse as an ipv4 address", cfg.Pool.Start))
			rangeParses = false
		} else {
			rangeStart = addr

			if subnetParses && !prefix.Contains(addr) {
				problems = append(problems, fmt.Sprintf("the pool start %s is not within the subnet %s", cfg.Pool.Start, cfg.Subnet))
			}
		}
	}

	if cfg.Pool.End != "" {
		addr, err := netip.ParseAddr(cfg.Pool.End)
		if err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the pool end %q does not parse as an ipv4 address", cfg.Pool.End))
			rangeParses = false
		} else {
			rangeEnd = addr

			if subnetParses && !prefix.Contains(addr) {
				problems = append(problems, fmt.Sprintf("the pool end %s is not within the subnet %s", cfg.Pool.End, cfg.Subnet))
			}
		}
	}

	completeRange := rangeParses && cfg.Pool.Start != "" && cfg.Pool.End != ""

	if completeRange && rangeEnd.Compare(rangeStart) < 0 {
		problems = append(problems, fmt.Sprintf("the pool end %s lies before the pool start %s", cfg.Pool.End, cfg.Pool.Start))
	}

	// the broadcast address of the subnet can neither serve as the pool end
	// nor as an exclude entry, and the range size is capped, exactly like
	// the controller's own registration validation (MaxPoolAddrs)
	var broadcast netip.Addr
	if subnetParses {
		broadcast = ipv4Broadcast(prefix)
	}

	if subnetParses && cfg.Pool.End != "" && rangeParses && rangeEnd == broadcast {
		problems = append(problems, fmt.Sprintf("the pool end %s equals the broadcast address %s of the subnet %s", cfg.Pool.End, broadcast, cfg.Subnet))
	}

	if completeRange && rangeEnd.Compare(rangeStart) >= 0 && ipv4RangeLen(rangeStart, rangeEnd) > maxPoolAddrs {
		problems = append(problems, fmt.Sprintf("the pool range %s - %s is larger than the maximum of %d addresses", cfg.Pool.Start, cfg.Pool.End, maxPoolAddrs))
	}

	for _, exclude := range cfg.Pool.Exclude {
		if exclude == "" {
			continue
		}

		addr, err := netip.ParseAddr(exclude)
		if err != nil || !addr.Is4() {
			problems = append(problems, fmt.Sprintf("the exclude address %q does not parse as an ipv4 address", exclude))

			continue
		}

		if subnetParses && !prefix.Contains(addr) {
			problems = append(problems, fmt.Sprintf("the exclude address %s is not within the subnet %s", exclude, cfg.Subnet))
		}

		if subnetParses && addr == broadcast {
			problems = append(problems, fmt.Sprintf("the exclude address %s equals the broadcast address %s of the subnet %s", exclude, broadcast, cfg.Subnet))
		}

		if cfg.Pool.Start != "" && cfg.Pool.End != "" && rangeParses && (addr.Compare(rangeStart) < 0 || addr.Compare(rangeEnd) > 0) {
			problems = append(problems, fmt.Sprintf("the exclude address %s is not within the pool range %s..%s", exclude, cfg.Pool.Start, cfg.Pool.End))
		}
	}

	sort.Strings(problems)

	return problems
}

// validateIPPoolSpec rejects an IPPool whose ipv4 configuration cannot
// serve. the crd schema accepts spellings the helper controller cannot
// register (for example a subnet length of two digits such as 10.0.0.0/33),
// and an invalid spec which is stored anyway is rejected by the controller
// on every resync - on update the previously registered configuration keeps
// serving while the object carries the broken spec (observed live with an
// exclude address outside the allocation range erroring on every sync until
// it was repaired). denying the write at admission keeps the invalid spec
// out of the cluster entirely.
//
// only fields which are present are validated: an omitted optional field is
// the controller's business, and the checks mirror the controller's own
// registration validation (the subnet, range, broadcast and exclude
// validations, the pool size cap and the parse check of the serverip) so a
// projection the controller would register is never rejected - the
// controller accepts an off-subnet serverip, so the admission check
// deliberately does too. internal failures fail open - the controller's own
// projection rejection stays the authoritative fence.
func (h *Handler) validateIPPoolSpec(ar *admissionv1.AdmissionReview) *admissionv1.AdmissionResponse {
	allow := &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: true,
	}

	pool := &kihv1.IPPool{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &pool); err != nil {
		log.Errorf("cannot unmarshal json to pool: %s", err)

		return allow
	}

	problems := evaluateIPPoolSpec(pool.Spec.IPv4Config)

	if len(problems) == 0 {
		return allow
	}

	log.Warnf("(service.validateIPPoolSpec) denying the %s of IPPool %s: %s",
		strings.ToLower(string(ar.Request.Operation)), pool.Name, strings.Join(problems, "; "))

	return &admissionv1.AdmissionResponse{
		UID:     ar.Request.UID,
		Allowed: false,
		Result: &metav1.Status{
			Message: fmt.Sprintf("the ipv4 configuration of the IPPool cannot serve: %s (the helper controller would reject this projection on its own sync)", strings.Join(problems, "; ")),
		},
	}
}

func (h *Handler) validateIPPoolSpecAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	if ar.Request == nil || len(ar.Request.Object.Raw) == 0 {
		log.Errorf("the AdmissionReview carries no object, allowing the request")

		w.Header().Set("Content-Type", "application/json")
		ar.Response = &admissionv1.AdmissionResponse{
			UID:     "",
			Allowed: true,
		}
		json.NewEncoder(w).Encode(&ar)

		return
	}

	ar.Response = h.validateIPPoolSpec(ar)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

func (h *Handler) validateVmNetCfgAdmission(w http.ResponseWriter, r *http.Request) {
	ar := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(&ar); err != nil {
		log.Errorf("cannot decode AdmissionReview to json: %s", err)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "cannot decode AdmissionReview to json: %s", err)

		return
	}

	if ar.Request == nil || len(ar.Request.Object.Raw) == 0 {
		log.Errorf("the AdmissionReview carries no object, allowing the request")

		w.Header().Set("Content-Type", "application/json")
		ar.Response = &admissionv1.AdmissionResponse{
			UID:     "",
			Allowed: true,
		}
		json.NewEncoder(w).Encode(&ar)

		return
	}

	ar.Response = h.validateVmNetCfg(ar)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&ar)
}

func (h *Handler) Run() {
	homedir := os.Getenv("HOME")
	keyPath := fmt.Sprintf("%s/tls.key", homedir)
	certPath := fmt.Sprintf("%s/tls.crt", homedir)

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/validate-ippool", h.validateIPPoolAdmission)
	mux.HandleFunc("/validate-ippool-spec", h.validateIPPoolSpecAdmission)
	mux.HandleFunc("/validate-vmnetcfg", h.validateVmNetCfgAdmission)

	h.httpServer = &http.Server{
		Addr:           ":8443",
		Handler:        mux,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1048576
	}

	log.Error(h.httpServer.ListenAndServeTLS(certPath, keyPath))
}

func (h *Handler) Stop() error {
	return h.httpServer.Shutdown(h.ctx)
}

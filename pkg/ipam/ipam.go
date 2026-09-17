package ipam

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	log "github.com/sirupsen/logrus"
)

var (
	// ErrSubnetNotFound reports addressing a subnet name which has no
	// registered allocation state.
	ErrSubnetNotFound = errors.New("network does not exists")

	// ErrIPAlreadyFree reports a release attempt for an address with no
	// live allocation, so nothing is left to release.
	ErrIPAlreadyFree = errors.New("ip was not allocated")

	// ErrIPForeignOwner reports a reclaim attempt for an address whose
	// recorded owner differs from the claiming identity, or a claim
	// landing on the exclude pseudo-owner. registration seeding and the
	// binding restore path rely on the same-owner case staying idempotent.
	ErrIPForeignOwner = errors.New("ip is allocated by another owner")

	// ErrIPNotInCidr reports a release attempt for an address outside the
	// subnet of the named network: ipam never allocated such an address,
	// so nothing is left to release and cleanup can converge.
	ErrIPNotInCidr = errors.New("ip is not inside the subnet")

	// ErrIPInvalid reports a release attempt for an address which does not
	// parse at all: no allocation can ever carry this identity, so cleanup
	// callers treat the release as converged instead of retrying forever.
	ErrIPInvalid = errors.New("invalid ip address")

	// ErrSubnetInvalid reports a subnet registration whose range can never
	// become valid (unparseable or out-of-range start/end, reversed range or
	// a broadcast end): the caller can classify the rejection as definitive
	// instead of retrying it forever or letting the startup gate wait.
	ErrSubnetInvalid = errors.New("invalid subnet configuration")
)

type IPSubnet struct {
	cidr      netip.Prefix
	start     net.IP
	end       net.IP
	broadcast net.IP
	ips       map[string]bool
	// owners records which allocation reference holds an allocated
	// address: an empty token is a plain allocation without reclaim
	// semantics, a reference is produced by util.AllocationRef and
	// survives as the persisted claim. reclaim semantics keep the
	// registration seeding and the binding restore path idempotent.
	owners map[string]string
	// attributed records the vm-level reference ("namespace/vmname") of
	// the object whose recorded claim an ownerless protection pin was
	// made for: the registration sweep pins the address of an
	// unusable-mac claim without a reclaim identity (no valid mac means
	// no owner reference), and the attribution lets the binding of that
	// same vm retake its own pin once its identity is corrected, while
	// every other binding, fresh allocation and unattributed pin stays
	// rejected. an empty token is an unattributed pin which nothing can
	attributed map[string]string
}

// ExcludedOwner is the pseudo-owner marking an address reserved by a
// pool's exclude specification; no vm binding can ever claim such an
// address.
const ExcludedOwner = "EXCLUDED"

// MaxPoolAddrs caps the size of a pool: every address of the range
// occupies one bitmap entry at registration, so an accidentally huge
// range (for example a /8 projection) would present as a registration-time
// memory blowup followed by an OOM restart loop. pools up to a full /16
// stay below the cap.
const MaxPoolAddrs = 65536

type IPAllocator struct {
	ipam  map[string]IPSubnet
	mutex sync.Mutex
}

func NewIPAllocator() *IPAllocator {
	ipam := make(map[string]IPSubnet)

	return &IPAllocator{
		ipam: ipam,
	}
}

// ValidateSubnetSpec reports whether a subnet/range projection can ever
// produce a live registration: the same classification NewSubnet applies,
// exposed so callers can reject an unusable pool configuration before they
// tear down the currently registered state. the errors are classified with
// ErrSubnetInvalid.
func ValidateSubnetSpec(subnet string, start string, end string) error {
	ipnet, err := netip.ParsePrefix(subnet)
	if err != nil {
		return fmt.Errorf("invalid subnet %s: %v: %w", subnet, err, ErrSubnetInvalid)
	}

	// this controller only serves ipv4: an ipv6 prefix would blow up the
	// broadcast computation below (net.CIDRMask(bits, 32) is nil there and
	// the 16-byte address would overrun the 4-byte broadcast buffer), so
	// it is rejected like any other unregistrable projection. the family
	// gate compares the address, not the prefix length: an ipv6 prefix
	// like 2001:db8::/32 carries 32 bits and would slip past a bits-only
	// check
	if !ipnet.Addr().Is4() {
		return fmt.Errorf("subnet %s is not an ipv4 subnet: %w", subnet, ErrSubnetInvalid)
	}

	startIP, err := netip.ParseAddr(start)
	if err != nil {
		return fmt.Errorf("invalid start address %s: %v: %w", start, err, ErrSubnetInvalid)
	}
	if !ipnet.Contains(startIP) {
		return fmt.Errorf("start address %s is not within subnet %s range: %w", start, subnet, ErrSubnetInvalid)
	}

	endIP, err := netip.ParseAddr(end)
	if err != nil {
		return fmt.Errorf("invalid end address %s: %v: %w", end, err, ErrSubnetInvalid)
	}
	if !ipnet.Contains(endIP) {
		return fmt.Errorf("end address %s is not within subnet %s range: %w", end, subnet, ErrSubnetInvalid)
	}

	if startIP.Compare(endIP) > 0 {
		return fmt.Errorf("end address %s is smaller then the start address %s: %w", end, start, ErrSubnetInvalid)
	}

	subnetStart := net.IP(ipnet.Addr().AsSlice())
	subnetMask := net.CIDRMask(ipnet.Bits(), 32)
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := range subnetStart {
		subnetBroadcast[i] = subnetStart[i] | ^subnetMask[i]
	}
	broadcastAddr, _ := netip.AddrFromSlice(subnetBroadcast)

	if endIP.Unmap() == broadcastAddr.Unmap() {
		return fmt.Errorf("end address %s equals the broadcast address %s: %w", end, subnetBroadcast.String(), ErrSubnetInvalid)
	}

	if v4RangeLen(startIP, endIP) > MaxPoolAddrs {
		return fmt.Errorf("pool range %s - %s is larger than the maximum of %d addresses: %w",
			start, end, MaxPoolAddrs, ErrSubnetInvalid)
	}

	return nil
}

// v4RangeLen returns the number of addresses of the inclusive v4 range
// start..end; the callers have already classified the range as v4, so the
// uint32 projection cannot overflow.
func v4RangeLen(start, end netip.Addr) uint64 {
	s := start.Unmap().As4()
	e := end.Unmap().As4()

	var startU, endU uint32
	for i := 0; i < 4; i++ {
		startU = startU<<8 | uint32(s[i])
		endU = endU<<8 | uint32(e[i])
	}

	return uint64(endU) - uint64(startU) + 1
}
func (a *IPAllocator) NewSubnet(name string, subnet string, start string, end string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; exists {
		// replacing an existing subnet would drop its allocation bitmap,
		// so live addresses would be reissued to other clients
		return fmt.Errorf("network %s already exists", name)
	}

	// the same classification as ValidateSubnetSpec; the checks below
	// stay in place as the construction path's own defense
	if err := ValidateSubnetSpec(subnet, start, end); err != nil {
		return err
	}
	s := IPSubnet{}
	s.start = net.ParseIP(start)
	s.end = net.ParseIP(end)

	ipnet, err := netip.ParsePrefix(subnet)
	if err != nil {
		return fmt.Errorf("invalid subnet %s: %v: %w", subnet, err, ErrSubnetInvalid)
	}
	s.cidr = ipnet

	startIP, err := netip.ParseAddr(start)
	if err != nil {
		return fmt.Errorf("invalid start address %s: %v: %w", start, err, ErrSubnetInvalid)
	}
	startIPCheck := ipnet.Contains(startIP)
	if !startIPCheck {
		return fmt.Errorf("start address %s is not within subnet %s range: %w", start, subnet, ErrSubnetInvalid)
	}

	endIP, err := netip.ParseAddr(end)
	if err != nil {
		return fmt.Errorf("invalid end address %s: %v: %w", end, err, ErrSubnetInvalid)
	}
	endIPCheck := ipnet.Contains(endIP)
	if !endIPCheck {
		return fmt.Errorf("end address %s is not within subnet %s range: %w", end, subnet, ErrSubnetInvalid)
	}

	startAddr, _ := netip.AddrFromSlice(s.start)
	endAddr, _ := netip.AddrFromSlice(s.end)
	if startAddr.Compare(endAddr) > 0 {
		return fmt.Errorf("end address %s is smaller then the start address %s: %w", end, start, ErrSubnetInvalid)
	}

	subnetStart := net.IP(ipnet.Addr().AsSlice())
	subnetMask := net.CIDRMask(ipnet.Bits(), 32)
	subnetBroadcast := net.IP(make([]byte, 4))
	for i := range subnetStart {
		subnetBroadcast[i] = subnetStart[i] | ^subnetMask[i]
	}
	s.broadcast = subnetBroadcast

	if s.end.Equal(s.broadcast) {
		return fmt.Errorf("end address %s equals the broadcast address %s: %w", s.end.String(), s.broadcast.String(), ErrSubnetInvalid)
	}

	// pre-allocate all ips between the start and end address
	allocatedIPs := make(map[string]bool)
	for ip := startAddr; endAddr.Compare(ip.Prev()) > 0; ip = ip.Next() {
		allocatedIPs[ip.Unmap().String()] = false
	}
	s.ips = allocatedIPs
	s.owners = make(map[string]string)
	s.attributed = make(map[string]string)

	a.ipam[name] = s

	return
}

func (a *IPAllocator) DeleteSubnet(name string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	delete(a.ipam, name)
}

func (a *IPAllocator) GetIP(name string, givenIP string) (string, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return "", fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	if givenIP != "" {
		gIP, err := netip.ParseAddr(givenIP)
		if err != nil {
			return "", err
		}
		gIPCheck := a.ipam[name].cidr.Contains(gIP)
		if !gIPCheck {
			return "", fmt.Errorf("given ip %s is not cidr %s", givenIP, a.ipam[name].cidr)
		}

		if a.ipam[name].broadcast.Equal(gIP.Unmap().AsSlice()) {
			return "", fmt.Errorf("given ip %s equals the broadcast address %s", givenIP, a.ipam[name].broadcast.String())
		}
	}

	// the bitmap keys are stored in the canonical unmapped spelling, so an
	// alternative spelling of the same address (a v4-in-v6 form such as
	// ::ffff:10.0.0.5) is compared on its canonical form
	givenIPCanonical := ""
	if givenIP != "" {
		if gIP, err := netip.ParseAddr(givenIP); err == nil {
			givenIPCanonical = gIP.Unmap().String()
		}
	}

	for ip, allocated := range a.ipam[name].ips {
		if givenIP != "" {
			if ip == givenIPCanonical {
				if allocated {
					return "", fmt.Errorf("given ip %s is already allocated", givenIP)
				} else {
					a.ipam[name].ips[ip] = true
					// a plain allocation carries no reclaim identity: it
					// blocks every later owner-specific reclaim
					delete(a.ipam[name].owners, ip)
					delete(a.ipam[name].attributed, ip)
					return ip, nil
				}
			}
		} else {
			if !allocated {
				a.ipam[name].ips[ip] = true
				delete(a.ipam[name].owners, ip)
				delete(a.ipam[name].attributed, ip)
				return ip, nil
			}
		}
	}

	return "", fmt.Errorf("no more ips left in network %s", name)
}

// AllocateIP hands out the next free address of the network as a named
// reservation of the given allocation reference: a binding's fresh
// allocation is durably owned from the moment it exists, so the delayed
// cleanup of a removed nic can release it through the owner-validated
// release without ever being able to touch an address which a successor
// took over in the meantime. the reservation carries the same identity
// the binding's restore path reclaims with, so a resynchronized binding
// stays idempotent.
func (a *IPAllocator) AllocateIP(name string, owner string) (string, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return "", fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	if owner == "" {
		return "", fmt.Errorf("empty owner for the allocation in network %s", name)
	}

	for ip, allocated := range a.ipam[name].ips {
		if !allocated {
			a.ipam[name].ips[ip] = true
			a.ipam[name].owners[ip] = owner
			delete(a.ipam[name].attributed, ip)

			return ip, nil
		}
	}

	return "", fmt.Errorf("no more ips left in network %s", name)
}

// ProtectIP pins the exact address without a reclaim identity: no fresh
// allocation and no binding can take it while the object whose recorded
// claim it protects may still need it. claimant carries the vm-level
// reference ("namespace/vmname") of the object the pin was made for, so
// the binding of that same vm can retake its own pin once its identity is
// corrected (ReclaimIPClaimant); an empty claimant is an unattributed pin
// which nothing can ever reclaim, which keeps unknown historical
// references conservative.
func (a *IPAllocator) ProtectIP(name string, givenIP string, claimant string) error {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	gIP, err := netip.ParseAddr(givenIP)
	if err != nil {
		return err
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return fmt.Errorf("given ip %s is not cidr %s", givenIP, a.ipam[name].cidr)
	}

	if a.ipam[name].broadcast.Equal(gIP.Unmap().AsSlice()) {
		return fmt.Errorf("given ip %s equals the broadcast address %s", givenIP, a.ipam[name].broadcast.String())
	}

	ip := gIP.Unmap().String()

	allocated, withinRange := a.ipam[name].ips[ip]
	if !withinRange {
		return fmt.Errorf("given ip %s is not between the pool range of network %s", ip, name)
	}
	if allocated {
		return fmt.Errorf("given ip %s is already allocated", ip)
	}

	a.ipam[name].ips[ip] = true
	// the pin deliberately carries no owner identity; the attribution
	// records which object's claim it protects
	if claimant != "" {
		a.ipam[name].attributed[ip] = claimant
	}

	return nil
}

// ReclaimIP allocates the exact address for the given allocation reference
// and makes a re-claim from the same owner idempotent: the registration
// seeding pins the persisted claims of a pool before the bindings can be
// restored, and the resynchronized binding reclaims its own recorded
// address without fighting the pin. an address held by another owner - or
// one whose plain allocation carries no reclaim identity - is rejected, so
// a fresh allocation can never take a still-owned address silently.
func (a *IPAllocator) ReclaimIP(name string, givenIP string, owner string) (string, error) {
	return a.ReclaimIPClaimant(name, givenIP, owner, "")
}

// ReclaimIPClaimant reclaims like ReclaimIP, and additionally accepts an
// ownerless protection pin which the registration sweep attributed to the
// claiming vm-level reference: the sweep pins the recorded address of a
// claim with an unusable macaddress without an owner identity (no valid
// mac means no owner reference), so the binding of that same vm retakes
// its own pin once the identity is corrected, while a pin attributed to
// another vm, an unattributed pin and a foreign named owner all stay
// rejected. the decision runs under one lock acquisition, so a successor
// which takes the address over in the meantime can never be displaced.
func (a *IPAllocator) ReclaimIPClaimant(name string, givenIP string, owner string, claimant string) (string, error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return "", fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	if owner == "" {
		return "", fmt.Errorf("empty owner for the reclaim of ip %s in network %s", givenIP, name)
	}

	gIP, err := netip.ParseAddr(givenIP)
	if err != nil {
		return "", err
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return "", fmt.Errorf("given ip %s is not cidr %s", givenIP, a.ipam[name].cidr)
	}

	if a.ipam[name].broadcast.Equal(gIP.Unmap().AsSlice()) {
		return "", fmt.Errorf("given ip %s equals the broadcast address %s", givenIP, a.ipam[name].broadcast.String())
	}

	givenIP = gIP.Unmap().String()

	// only the pool range is part of the allocation bitmap: an address
	// between the subnet and pool boundaries exists but cannot be handed
	// out by ipam
	if allocated, withinRange := a.ipam[name].ips[givenIP]; !withinRange {
		return "", fmt.Errorf("given ip %s is not between the pool range of network %s", givenIP, name)
	} else if allocated {
		current := a.ipam[name].owners[givenIP]
		if current == owner {
			return givenIP, nil
		}

		if current == "" && claimant != "" && a.ipam[name].attributed[givenIP] == claimant {
			// the own protection pin of a claim which could not form its
			// owner identity at registration time: promote it now that
			// the corrected identity claims it
			a.ipam[name].owners[givenIP] = owner
			delete(a.ipam[name].attributed, givenIP)

			return givenIP, nil
		}

		return "", fmt.Errorf("given ip %s is already allocated by %s: %w", givenIP, current, ErrIPForeignOwner)
	}

	a.ipam[name].ips[givenIP] = true
	a.ipam[name].owners[givenIP] = owner
	delete(a.ipam[name].attributed, givenIP)

	return givenIP, nil
}

// AdoptIP retags an existing allocation under a verified owner: the lease
// idempotent path proves the binding owns the address, so an allocation
// this process previously made without a reclaim identity (the anonymous
// auto-allocation of an earlier sync whose durable write failed) is
// promoted to the named owner while a allocation named by another owner
// is rejected. a free address becomes owned by the caller, which covers a
// binding whose lease survived the restart but whose allocator claim was
// lost with the previous process.
func (a *IPAllocator) AdoptIP(name string, givenIP string, owner string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	if owner == "" {
		return fmt.Errorf("empty owner for the adopt of ip %s in network %s", givenIP, name)
	}

	gIP, err := netip.ParseAddr(givenIP)
	if err != nil {
		return err
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return fmt.Errorf("given ip %s is not cidr %s: %w", givenIP, a.ipam[name].cidr, ErrIPNotInCidr)
	}

	if a.ipam[name].broadcast.Equal(gIP.Unmap().AsSlice()) {
		return fmt.Errorf("given ip %s equals the broadcast address %s", givenIP, a.ipam[name].broadcast.String())
	}

	ip := gIP.Unmap().String()

	if allocated, withinRange := a.ipam[name].ips[ip]; !withinRange {
		return fmt.Errorf("given ip %s is not between the pool range of network %s", ip, name)
	} else if allocated {
		current := a.ipam[name].owners[ip]
		if current == owner {
			return nil
		}

		if current != "" {
			return fmt.Errorf("given ip %s is already allocated by %s: %w", ip, current, ErrIPForeignOwner)
		}

		// promote the anonymous allocation to the verified owner
		a.ipam[name].owners[ip] = owner
		delete(a.ipam[name].attributed, ip)

		return nil
	}

	a.ipam[name].ips[ip] = true
	a.ipam[name].owners[ip] = owner
	delete(a.ipam[name].attributed, ip)

	return nil
}

// ReleaseIP releases the given address of the named network. the lookup
// runs on the canonical spelling (the v4 form of any v4-in-v6 input,
// exactly like every other mutator of the allocation state), so a
// non-canonical spelling finds its own reservation instead of reporting
// an already-free address while the reservation survives.
func (a *IPAllocator) ReleaseIP(name string, givenIP string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	if givenIP == "" {
		return fmt.Errorf("given ip is empty")
	}

	gIP, err := netip.ParseAddr(givenIP)
	if err != nil {
		return fmt.Errorf("given ip %s: %w", givenIP, ErrIPInvalid)
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return fmt.Errorf("given ip %s is not cidr %s: %w", givenIP, a.ipam[name].cidr, ErrIPNotInCidr)
	}
	ip := gIP.Unmap().String()

	allocated, withinRange := a.ipam[name].ips[ip]
	if !withinRange {
		return fmt.Errorf("given ip %s not found in network %s: %w", ip, name, ErrIPAlreadyFree)
	}
	if !allocated {
		return fmt.Errorf("given ip %s: %w", ip, ErrIPAlreadyFree)
	}

	a.ipam[name].ips[ip] = false
	// a released address forgets its owner: a later reclaim starts over
	// instead of matching a stale identity
	delete(a.ipam[name].owners, ip)
	delete(a.ipam[name].attributed, ip)

	return
}

// ReleaseIPOwnedBy releases the exact address only while its reservation
// still carries the given owner reference: a compensating release after a
// raced cleanup must never free an allocation which a successor took over
// in the meantime (a fresh anonymous allocation or another owner's named
// reclaim), so the owner check and the release run under one lock
// acquisition. the caller treats the foreign-owner and already-free
// outcomes as converged: there is nothing left of this owner to release.
func (a *IPAllocator) ReleaseIPOwnedBy(name string, givenIP string, owner string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		return fmt.Errorf("%s: %w", name, ErrSubnetNotFound)
	}

	if givenIP == "" {
		return fmt.Errorf("given ip is empty")
	}

	if owner == "" {
		return fmt.Errorf("empty owner for the release of ip %s in network %s", givenIP, name)
	}

	gIP, err := netip.ParseAddr(givenIP)
	if err != nil {
		return fmt.Errorf("given ip %s: %w", givenIP, ErrIPInvalid)
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return fmt.Errorf("given ip %s is not cidr %s: %w", givenIP, a.ipam[name].cidr, ErrIPNotInCidr)
	}

	ip := gIP.Unmap().String()

	allocated, withinRange := a.ipam[name].ips[ip]
	if !withinRange {
		return fmt.Errorf("given ip %s not found in network %s: %w", ip, name, ErrIPAlreadyFree)
	}
	if !allocated {
		return fmt.Errorf("given ip %s: %w", ip, ErrIPAlreadyFree)
	}

	if current := a.ipam[name].owners[ip]; current != owner {
		return fmt.Errorf("given ip %s is allocated by %s: %w", ip, current, ErrIPForeignOwner)
	}
	a.ipam[name].ips[ip] = false
	// a released address forgets its owner: a later reclaim starts over
	// instead of matching a stale identity
	delete(a.ipam[name].owners, ip)
	delete(a.ipam[name].attributed, ip)

	return
}

// IPsOwnedBy snapshots the allocated addresses held by this owner in one
// network. Anonymous protection pins are never treated as owned claims.
// Callers must recheck ownership when releasing a returned address.
func (a *IPAllocator) IPsOwnedBy(name string, owner string) (ips []string) {
	if owner == "" {
		return nil
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	subnet := a.ipam[name]
	for ip, currentOwner := range subnet.owners {
		if currentOwner == owner && subnet.ips[ip] {
			ips = append(ips, ip)
		}
	}
	return ips
}

// UsageCounts returns one consistent accounting snapshot and whether the
// network is registered. A missing subnet is not an empty, available pool.
func (a *IPAllocator) UsageCounts(name string) (used int, available int, exists bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	subnet, exists := a.ipam[name]
	if !exists {
		return
	}
	for _, allocated := range subnet.ips {
		if allocated {
			used++
		} else {
			available++
		}
	}
	return
}

func (a *IPAllocator) Used(name string) (i int) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		log.Warnf("(ipam.Used) network %s does not exists", name)

		return
	}

	for _, allocated := range a.ipam[name].ips {
		if allocated {
			i++
		}
	}

	return i
}

func (a *IPAllocator) Available(name string) (i int) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		log.Warnf("(ipam.Available) network %s does not exists", name)

		return
	}

	for _, allocated := range a.ipam[name].ips {
		if allocated {
			i++
		}
	}

	return len(a.ipam[name].ips) - i
}

func (a *IPAllocator) Usage(name string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; !exists {
		log.Warnf("(ipam.Usage) network %s does not exists", name)

		return
	}

	log.Infof("(ipam.Usage) %s: cidr=%s, start=%s, end=%s, broadcast=%s",
		name,
		a.ipam[name].cidr.String(),
		a.ipam[name].start.String(),
		a.ipam[name].end.String(),
		a.ipam[name].broadcast.String(),
	)

	var i int = 0
	log.Infof("(ipam.Usage) allocated ips:")
	for ip, allocated := range a.ipam[name].ips {
		if allocated {
			log.Infof("- %s", ip)
			i++
		}
	}

	log.Infof("(ipam.Usage) ipsinpool=%d, usedips=%d",
		len(a.ipam[name].ips),
		i,
	)
}

func New() *IPAllocator {
	return NewIPAllocator()
}

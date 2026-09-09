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
}

// ExcludedOwner is the pseudo-owner marking an address reserved by a
// pool's exclude specification; no vm binding can ever claim such an
// address.
const ExcludedOwner = "EXCLUDED"

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

func (a *IPAllocator) NewSubnet(name string, subnet string, start string, end string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.ipam[name]; exists {
		// replacing an existing subnet would drop its allocation bitmap,
		// so live addresses would be reissued to other clients
		return fmt.Errorf("network %s already exists", name)
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

	for ip, allocated := range a.ipam[name].ips {
		if givenIP != "" {
			if ip == givenIP {
				if allocated {
					return "", fmt.Errorf("given ip %s is already allocated", givenIP)
				} else {
					a.ipam[name].ips[ip] = true
					// a plain allocation carries no reclaim identity: it
					// blocks every later owner-specific reclaim
					delete(a.ipam[name].owners, ip)
					return ip, nil
				}
			}
		} else {
			if !allocated {
				a.ipam[name].ips[ip] = true
				delete(a.ipam[name].owners, ip)
				return ip, nil
			}
		}
	}

	return "", fmt.Errorf("no more ips left in network %s", name)
}

// ReclaimIP allocates the exact address for the given allocation reference
// and makes a re-claim from the same owner idempotent: the registration
// seeding pins the persisted claims of a pool before the bindings can be
// restored, and the resynchronized binding reclaims its own recorded
// address without fighting the pin. an address held by another owner - or
// one whose plain allocation carries no reclaim identity - is rejected, so
// a fresh allocation can never take a still-owned address silently.
func (a *IPAllocator) ReclaimIP(name string, givenIP string, owner string) (string, error) {
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
		if current := a.ipam[name].owners[givenIP]; current != owner {
			return "", fmt.Errorf("given ip %s is already allocated by %s: %w", givenIP, current, ErrIPForeignOwner)
		}

		return givenIP, nil
	}

	a.ipam[name].ips[givenIP] = true
	a.ipam[name].owners[givenIP] = owner

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

		return nil
	}

	a.ipam[name].ips[ip] = true
	a.ipam[name].owners[ip] = owner

	return nil
}

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
		return err
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return fmt.Errorf("given ip %s is not cidr %s: %w", givenIP, a.ipam[name].cidr, ErrIPNotInCidr)
	}

	for ip, allocated := range a.ipam[name].ips {
		if ip == givenIP {
			if allocated {
				a.ipam[name].ips[ip] = false
				// a released address forgets its owner: a later reclaim
				// starts over instead of matching a stale identity
				delete(a.ipam[name].owners, ip)

				return
			} else {
				return fmt.Errorf("given ip %s: %w", givenIP, ErrIPAlreadyFree)
			}
		}
	}

	return fmt.Errorf("given ip %s not found in network %s: %w", givenIP, name, ErrIPAlreadyFree)
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

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
}

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
					return ip, nil
				}
			}
		} else {
			if !allocated {
				a.ipam[name].ips[ip] = true
				return ip, nil
			}
		}
	}

	return "", fmt.Errorf("no more ips left in network %s", name)
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

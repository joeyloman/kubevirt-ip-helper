package dhcp

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/insomniacslk/dhcp/rfc1035label"
)

var (
	// ErrLeaseNotFound reports lease operations on a hardware address
	// which currently has no lease registered.
	ErrLeaseNotFound = errors.New("lease does not exists")

	// ErrLeaseForeignOwner reports a lease operation which would affect a
	// lease registered for a different owner reference.
	ErrLeaseForeignOwner = errors.New("lease belongs to another owner")
)

type DHCPPool struct {
	ServerIP     net.IP
	SubnetMask   net.IPMask
	Router       net.IP
	DNS          []net.IP
	DomainName   string
	DomainSearch []string
	NTP          []net.IP
	LeaseTime    int
	Nic          string
}

type DHCPLease struct {
	PoolName  string
	ClientIP  net.IP
	Reference string
}

type DHCPAllocator struct {
	pools   map[string]DHCPPool
	leases  map[string]DHCPLease
	servers map[string]*server4.Server
	mutex   sync.Mutex
}

func NewDHCPAllocator() *DHCPAllocator {
	pools := make(map[string]DHCPPool)
	leases := make(map[string]DHCPLease)
	servers := make(map[string]*server4.Server)

	return &DHCPAllocator{
		pools:   pools,
		leases:  leases,
		servers: servers,
	}
}

func (a *DHCPAllocator) AddPool(
	name string,
	serverIP string,
	subnetMask string,
	routerIP string,
	DNSServers []string,
	domainName string,
	domainSearch []string,
	NTPServers []string,
	leaseTime int,
	nic string,
) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	pool := DHCPPool{}
	pool.ServerIP = net.ParseIP(serverIP)
	pool.SubnetMask = net.IPMask(net.ParseIP(subnetMask).To4())
	pool.Router = net.ParseIP(routerIP)
	for i := 0; i < len(DNSServers); i++ {
		pool.DNS = append(pool.DNS, net.ParseIP(DNSServers[i]))
	}
	pool.DomainName = domainName
	pool.DomainSearch = domainSearch
	for i := 0; i < len(NTPServers); i++ {
		hostip := net.ParseIP(NTPServers[i])
		if hostip.To4() != nil {
			pool.NTP = append(pool.NTP, net.ParseIP(NTPServers[i]))
		} else {
			hostips, err := net.LookupIP(NTPServers[i])
			if err != nil {
				log.Errorf("(dhcp.AddPool) cannot get any ip addresses from ntp domainname entry %s: %s", NTPServers[i], err)
			}
			for _, ip := range hostips {
				if ip.To4() != nil {
					pool.NTP = append(pool.NTP, ip)
				}
			}
		}
	}
	pool.LeaseTime = leaseTime
	pool.Nic = nic

	a.pools[name] = pool

	log.Debugf("(dhcp.AddPool) pool %s added", name)

	return
}

func (a *DHCPAllocator) CheckPool(name string) bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	_, exists := a.pools[name]

	return exists
}

func (a *DHCPAllocator) GetPool(name string) (pool DHCPPool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	return a.pools[name]
}

func (a *DHCPAllocator) DeletePool(name string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if _, exists := a.pools[name]; !exists {
		return fmt.Errorf("pool %s does not exists", name)
	}

	delete(a.pools, name)

	log.Debugf("(dhcp.DeletePool) pool %s deleted", name)

	return
}

func (a *DHCPAllocator) AddLease(
	hwAddr string,
	poolName string,
	clientIP string,
	ref string,
) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if hwAddr == "" {
		return fmt.Errorf("hwaddr is empty")
	}

	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		return fmt.Errorf("hwaddr %s is not valid", hwAddr)
	}

	// leases are stored under the canonical colon form of the mac address,
	// so hyphen and uppercase spellings of the same hardware address map to
	// the same lease key
	key := hw.String()

	if _, exists := a.leases[key]; exists {
		return fmt.Errorf("lease for hwaddr %s already exists", key)
	}

	lease := DHCPLease{}
	lease.PoolName = poolName
	lease.ClientIP = net.ParseIP(clientIP)
	lease.Reference = ref

	a.leases[key] = lease

	log.Debugf("(dhcp.AddLease) lease added for hardware address: %s", key)

	return
}

func (a *DHCPAllocator) CheckLease(hwAddr string) bool {
	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		return false
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	_, exists := a.leases[hw.String()]

	return exists
}

func (a *DHCPAllocator) GetLease(hwAddr string) (lease DHCPLease) {
	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	return a.leases[hw.String()]
}

// GetLeaseByIP returns the lease which currently holds the given client ip.
// It is used to check whether an ip address was already reassigned to
// another owner before releasing it.
func (a *DHCPAllocator) GetLeaseByIP(clientIP string) (hwAddr string, lease DHCPLease, found bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	for hw, l := range a.leases {
		if l.ClientIP != nil && l.ClientIP.String() == clientIP {
			hwAddr = hw
			lease = l
			found = true

			return
		}
	}

	return
}

func (a *DHCPAllocator) DeleteLease(hwAddr string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		return fmt.Errorf("hwaddr %s is not valid", hwAddr)
	}

	key := hw.String()

	if _, exists := a.leases[key]; !exists {
		return fmt.Errorf("lease for hwaddr %s does not exists", key)
	}

	delete(a.leases, key)

	log.Debugf("(dhcp.DeleteLease) lease deleted for hardware address: %s", key)

	return
}

// DeleteLeaseOwnedBy removes the lease for the hardware address only while
// the registered lease still references the given owner reference, so a
// delayed cleanup cannot delete an allocation which a concurrent writer
// reassigned to another vm in the meantime. the owner check and the
// deletion run under one lock acquisition.
func (a *DHCPAllocator) DeleteLeaseOwnedBy(hwAddr string, ref string) (err error) {
	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		return fmt.Errorf("hwaddr %s is not valid", hwAddr)
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	key := hw.String()

	lease, exists := a.leases[key]
	if !exists {
		return fmt.Errorf("%w: hwaddr %s", ErrLeaseNotFound, key)
	}

	if lease.Reference != ref {
		return fmt.Errorf("%w: hwaddr %s is registered for %s", ErrLeaseForeignOwner, key, lease.Reference)
	}

	delete(a.leases, key)

	log.Debugf("(dhcp.DeleteLeaseOwnedBy) lease deleted for hardware address: %s (%s)", key, ref)

	return
}

func (a *DHCPAllocator) Usage() {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	for hwaddr, lease := range a.leases {
		pool := a.pools[lease.PoolName]
		log.Infof("(dhcp.Usage) lease: hwaddr=%s, pool=%s, clientip=%s, netmask=%s, router=%s, dns=%+v, domain=%s, domainsearch=%+v, ntp=%+v, leasetime=%d, ref=%s, nic=%s",
			hwaddr,
			lease.PoolName,
			lease.ClientIP.String(),
			pool.SubnetMask.String(),
			pool.Router.String(),
			pool.DNS,
			pool.DomainName,
			pool.DomainSearch,
			pool.NTP,
			pool.LeaseTime,
			lease.Reference,
			pool.Nic,
		)
	}
}

func New() *DHCPAllocator {
	return NewDHCPAllocator()
}

func (a *DHCPAllocator) dhcpHandler(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	if m == nil {
		log.Errorf("(dhcp.dhcpHandler) packet is nil!")

		return
	}

	log.Tracef("(dhcp.dhcpHandler) INCOMING PACKET=%s", m.Summary())

	if m.OpCode != dhcpv4.OpcodeBootRequest {
		log.Errorf("(dhcp.dhcpHandler) not a BootRequest!")

		return
	}

	// lease lookups use the canonical colon form of the mac address
	lease := a.GetLease(m.ClientHWAddr.String())

	if lease.ClientIP == nil {
		log.Warnf("(dhcp.dhcpHandler) NO LEASE FOUND: hwaddr=%s", m.ClientHWAddr.String())

		return
	}

	if !a.CheckPool(lease.PoolName) {
		log.Warnf("(dhcp.dhcpHandler) NO MATCHED POOL FOUND FOR LEASE: hwaddr=%s", m.ClientHWAddr.String())

		return
	}
	pool := a.GetPool(lease.PoolName)

	log.Debugf("(dhcp.dhcpHandler) LEASE FOUND: hwaddr=%s, serverip=%s, clientip=%s, mask=%s, router=%s, dns=%+v, domainname=%s, domainsearch=%+v, ntp=%+v, leasetime=%d, reference=%s, nic=%s",
		m.ClientHWAddr.String(),
		pool.ServerIP.String(),
		lease.ClientIP.String(),
		pool.SubnetMask.String(),
		pool.Router.String(),
		pool.DNS,
		pool.DomainName,
		pool.DomainSearch,
		pool.NTP,
		pool.LeaseTime,
		lease.Reference,
		pool.Nic,
	)

	var replyType dhcpv4.MessageType
	var sendReply bool

	switch mt := m.MessageType(); mt {
	case dhcpv4.MessageTypeDiscover:
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPDISCOVER from %s via %s", m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic)

		replyType = dhcpv4.MessageTypeOffer
		sendReply = true
	case dhcpv4.MessageTypeRequest:
		// a request must reference the offered address, either through the
		// requested-ip/server-identifier options (address selection) or
		// through the client address field (renewal)
		if serverID := m.ServerIdentifier(); len(serverID) > 0 && !serverID.Equal(pool.ServerIP) {
			log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPREQUEST from %s via %s ignored: server identifier %s does not match this server",
				m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic, serverID.String())

			return
		}

		claimIP := m.RequestedIPAddress()
		if len(claimIP) == 0 || claimIP.Equal(net.IPv4zero) {
			claimIP = m.ClientIPAddr
		}

		if len(claimIP) == 0 || !claimIP.Equal(lease.ClientIP) {
			log.Warnf("(dhcp.dhcpHandler) [txid=%s] DHCPREQUEST from %s via %s claims ip %s, but the lease holds %s, sending DHCPNAK",
				m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic, claimIP, lease.ClientIP)

			a.sendNak(conn, peer, m, pool.ServerIP)

			return
		}

		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPREQUEST for %s from %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)

		replyType = dhcpv4.MessageTypeAck
		sendReply = true
	case dhcpv4.MessageTypeRelease:
		// rfc 2131 4.3.4: a release is a one-way notification without a reply
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPRELEASE for %s from %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)

		return
	default:
		log.Warnf("(dhcp.dhcpHandler) [txid=%s] Unhandled message type for %s via %s: %v", m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic, mt)

		return
	}

	if !sendReply {
		return
	}

	reply, err := dhcpv4.NewReplyFromRequest(m)
	if err != nil {
		log.Errorf("(dhcp.dhcpHandler) NewReplyFromRequest failed: %v", err)

		return
	}

	// rfc 2131 figure 3: an offer always carries a zero ciaddr and the
	// offered address in yiaddr; an ack copies the client address of its
	// request, which is set during renewal and zero during address selection
	reply.ServerIPAddr = pool.ServerIP
	reply.YourIPAddr = lease.ClientIP
	reply.TransactionID = m.TransactionID
	reply.ClientHWAddr = m.ClientHWAddr
	reply.Flags = m.Flags
	reply.GatewayIPAddr = m.GatewayIPAddr
	if replyType == dhcpv4.MessageTypeAck {
		reply.ClientIPAddr = m.ClientIPAddr
	}

	reply.UpdateOption(dhcpv4.OptMessageType(replyType))
	reply.UpdateOption(dhcpv4.OptServerIdentifier(pool.ServerIP))
	reply.UpdateOption(dhcpv4.OptSubnetMask(pool.SubnetMask))
	reply.UpdateOption(dhcpv4.OptRouter(pool.Router))

	if len(pool.DNS) > 0 {
		reply.UpdateOption(dhcpv4.OptDNS(pool.DNS...))
	}

	if pool.DomainName != "" {
		reply.UpdateOption(dhcpv4.OptDomainName(pool.DomainName))
	}

	if len(pool.DomainSearch) > 0 {
		dsl := rfc1035label.NewLabels()
		dsl.Labels = append(dsl.Labels, pool.DomainSearch...)

		reply.UpdateOption(dhcpv4.OptDomainSearch(dsl))
	}

	if len(pool.NTP) > 0 {
		reply.UpdateOption(dhcpv4.OptNTPServers(pool.NTP...))
	}

	if pool.LeaseTime > 0 {
		reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(time.Duration(pool.LeaseTime) * time.Second))
	} else {
		// default lease time: 1 year
		reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(31536000 * time.Second))
	}
	if replyType == dhcpv4.MessageTypeOffer {
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPOFFER on %s to %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
	} else {
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPACK on %s to %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
	}

	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		log.Errorf("(dhcp.dhcpHandler) Cannot reply to client: %v", err)
	}
}

// sendNak tells the client that its request does not match the lease state,
// so it restarts the discovery. A nak contains no lease options.
func (a *DHCPAllocator) sendNak(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4, serverIP net.IP) {
	reply, err := dhcpv4.NewReplyFromRequest(m, dhcpv4.WithMessageType(dhcpv4.MessageTypeNak))
	if err != nil {
		log.Errorf("(dhcp.dhcpHandler) building DHCPNAK failed: %v", err)

		return
	}

	reply.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))

	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		log.Errorf("(dhcp.dhcpHandler) Cannot reply to client: %v", err)
	}
}

func (a *DHCPAllocator) Run(nic string, serverip string) (err error) {
	log.Infof("(dhcp.Run) starting DHCP service on nic %s", nic)

	// we need to listen on 0.0.0.0 otherwise client discovers will not be answered
	laddr := net.UDPAddr{
		IP:   net.ParseIP("0.0.0.0"),
		Port: 67,
	}

	server, err := server4.NewServer(nic, &laddr, a.dhcpHandler)
	if err != nil {
		return
	}

	go server.Serve()

	a.mutex.Lock()
	a.servers[nic] = server
	a.mutex.Unlock()

	return
}

func (a *DHCPAllocator) Stop(nic string) (err error) {
	log.Infof("(dhcp.Stop) stopping DHCP service on nic %s", nic)

	a.mutex.Lock()
	server, exists := a.servers[nic]
	delete(a.servers, nic)
	a.mutex.Unlock()

	if !exists || server == nil {
		log.Debugf("(dhcp.Stop) no running dhcp service on nic %s, nothing to stop", nic)

		return
	}

	return server.Close()
}

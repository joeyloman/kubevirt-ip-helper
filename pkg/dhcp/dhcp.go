package dhcp

import (
	"context"
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

	// ErrLeaseInvalidHwAddr reports a lease operation for a hardware address
	// which does not parse at all: no lease can ever carry this identity, so
	// cleanup callers treat the deletion as converged instead of retrying
	// forever.
	ErrLeaseInvalidHwAddr = errors.New("invalid hardware address")

	// ErrServerAlreadyRunning reports a Run for a network the allocator
	// already serves: the caller classifies it as the converged outcome of
	// a racing listener repair - the pool is serving, which is exactly what
	// the repair wanted.
	ErrServerAlreadyRunning = errors.New("dhcp service already running")
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
	pools  map[string]DHCPPool
	leases map[string]DHCPLease
	// servers holds the running dhcp servers keyed by pool identity
	// (spec.NetworkName), not by nic: several pools may legitimately share
	// one interface and each server must stay individually stoppable
	servers map[string]*server4.Server
	// serverNics records the interface each running server is bound to, so
	// a second Run on the same interface can surface the kernel-dependent
	// delivery duplication instead of hiding it
	serverNics map[string]string
	// warnMutex guards the throttled warning state of the packet handler:
	// a broadcast flood of unknown hardware addresses must not produce one
	// log line (and one string formatting pass) per packet
	warnMutex          sync.Mutex
	lastUnknownHWAddr  time.Time
	unknownHWAddrCount uint64
	mutex              sync.Mutex

	// resolver resolves ntp hostname entries during pool registrations;
	// a nil resolver uses net.DefaultResolver. the field lets tests
	// observe and shortcut resolution without swapping the global.
	resolver *net.Resolver
}

func NewDHCPAllocator() *DHCPAllocator {
	pools := make(map[string]DHCPPool)
	leases := make(map[string]DHCPLease)
	servers := make(map[string]*server4.Server)
	serverNics := make(map[string]string)

	return &DHCPAllocator{
		pools:      pools,
		leases:     leases,
		servers:    servers,
		serverNics: serverNics,
	}
}

// resolveNTPServers normalizes the ntp server entries into ip addresses,
// resolving hostname entries through the resolver of the allocator.
// AddPool runs it before taking the allocator lock: the dhcp packet
// handler reads the pools and leases under the same mutex, so a slow or
// broken resolver during one pool registration must not stall packet
// processing on every pool. entries which cannot be resolved are logged
// and skipped, so a pool with a broken ntp hostname still registers.
func (a *DHCPAllocator) resolveNTPServers(NTPServers []string) (ntp []net.IP) {
	resolver := a.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	for _, entry := range NTPServers {
		hostip := net.ParseIP(entry)
		if hostip.To4() != nil {
			ntp = append(ntp, hostip)

			continue
		}

		hostips, err := resolver.LookupIP(context.Background(), "ip", entry)
		if err != nil {
			log.Errorf("(dhcp.AddPool) cannot get any ip addresses from ntp domainname entry %s: %s", entry, err)
		}
		for _, ip := range hostips {
			if ip.To4() != nil {
				ntp = append(ntp, ip)
			}
		}
	}

	return
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
	// resolve the ntp hostnames before taking the lock, the packet
	// handler must not wait behind a slow resolver
	ntp := a.resolveNTPServers(NTPServers)

	a.mutex.Lock()
	defer a.mutex.Unlock()

	pool := DHCPPool{}
	pool.ServerIP = net.ParseIP(serverIP)
	pool.SubnetMask = net.IPMask(net.ParseIP(subnetMask).To4())
	pool.Router = net.ParseIP(routerIP)
	for _, dnsServer := range DNSServers {
		pool.DNS = append(pool.DNS, net.ParseIP(dnsServer))
	}
	pool.DomainName = domainName
	pool.DomainSearch = domainSearch
	pool.NTP = ntp
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

// GetLeaseByIPAndNetwork returns the lease which currently holds the
// given client ip within one network. ipam allocations are scoped by the
// networkname, so an ownership check before releasing an address must
// not collide with same numeric addresses served by another network.
func (a *DHCPAllocator) GetLeaseByIPAndNetwork(networkName string, clientIP string) (hwAddr string, lease DHCPLease, found bool) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	for hw, l := range a.leases {
		if l.PoolName == networkName && l.ClientIP != nil && l.ClientIP.String() == clientIP {
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
		return fmt.Errorf("%w: hwaddr %q", ErrLeaseInvalidHwAddr, hwAddr)
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

// WithOwnedLease runs fn with the client ip of the lease only while the
// lease still exists and references the given owner reference: the
// validation and the callback run under one lock acquisition, so a
// concurrent cleanup cannot remove the lease between the check and the
// claim mutation fn performs (the adoption of the leased address into the
// ipam allocator). the snapshot-based lease checks of a reconciliation
// cannot provide this guarantee on their own - there is always a window
// between an unsynchronized check and the allocator mutation.
//
// fn must stay fast and in-memory: it runs while the dhcp allocator lock
// is held, so it must not make network or kubernetes api calls, and it
// must only acquire locks which are always taken after the dhcp lock. the
// ipam allocator lock qualifies: nothing in this codebase takes the dhcp
// lock while holding the ipam lock.
func (a *DHCPAllocator) WithOwnedLease(hwAddr string, ref string, fn func(clientIP string) error) (err error) {
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

	if lease.ClientIP == nil {
		return fmt.Errorf("lease of hwaddr %s carries no client ip", key)
	}

	clientIP := lease.ClientIP.String()

	if err := fn(clientIP); err != nil {
		return fmt.Errorf("the guarded lease update of hwaddr %s failed: %w", key, err)
	}

	log.Debugf("(dhcp.WithOwnedLease) guarded lease update for hardware address: %s (%s)", key, ref)

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

// packetLeaseAndPool resolves the lease and its pool of one packet under a
// single lock acquisition. the dhcp packet handler previously looked the
// lease, the pool existence and the pool up with three separate lock
// acquisitions per packet; a flood on the shared interface then serialized
// its ten layers of contention across the whole registration and lease
// adoption machinery. an hwaddr without a lease reports leaseFound=false
// (a stored lease always carries a client ip), and a lease whose pool is
// gone reports poolFound=false so the caller can fail its client fast.
func (a *DHCPAllocator) packetLeaseAndPool(hwAddr string) (lease DHCPLease, pool DHCPPool, leaseFound bool, poolFound bool) {
	hw, err := net.ParseMAC(hwAddr)
	if err != nil {
		return lease, pool, false, false
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	key := hw.String()
	lease, leaseFound = a.leases[key]
	if !leaseFound || lease.ClientIP == nil {
		return lease, pool, false, false
	}

	pool, poolFound = a.pools[lease.PoolName]

	return lease, pool, true, poolFound
}

// logUnknownHWAddr reports packets whose hardware address has no lease,
// throttled to one aggregated line per window: an unrelated or abusive
// broadcast flood on the served segment must not produce one log line per
// packet. the count resets with every window so a persistent flood keeps a
// periodic heartbeat visible.
func (a *DHCPAllocator) logUnknownHWAddr(m *dhcpv4.DHCPv4) {
	warnNow := false

	a.warnMutex.Lock()
	a.unknownHWAddrCount++
	if a.lastUnknownHWAddr.IsZero() || time.Since(a.lastUnknownHWAddr) >= 5*time.Second {
		warnNow = true
		a.lastUnknownHWAddr = time.Now()
	}
	count := a.unknownHWAddrCount
	if warnNow {
		a.unknownHWAddrCount = 0
	}
	a.warnMutex.Unlock()

	if warnNow {
		log.Warnf("(dhcp.dhcpHandler) NO LEASE FOUND: hwaddr=%s (txid=%s, type=%s) - %d unknown-hwaddr packet(s) in the last 5s", m.ClientHWAddr.String(), m.TransactionID.String(), m.MessageType(), count)
	}
}

// IsRunning reports whether the DHCP service of the pool identified by
// networkName currently serves on its interface. the ippool controller
// uses it to detect a listener which died after its registration (its
// socket error is deregistered by the serve wrapper) and re-serve the
// pool on the next event or resync.
func (a *DHCPAllocator) IsRunning(networkName string) bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	_, running := a.servers[networkName]

	return running
}

// RemoveLeasesForNetwork drops every lease of the named network from the
// lease registry: deleting a pool must not leave its leases behind, which
// would blackhole the renewals of still-running vms with no pool to serve
// them (and no NAK to restart them). it is called by the pool deletion
// path only - a dhcp pool reload keeps the leases of the live vms.
func (a *DHCPAllocator) RemoveLeasesForNetwork(networkName string) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	for hw, lease := range a.leases {
		if lease.PoolName == networkName {
			delete(a.leases, hw)
		}
	}
}

func (a *DHCPAllocator) dhcpHandler(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	if m == nil {
		log.Errorf("(dhcp.dhcpHandler) packet is nil!")

		return
	}

	// the summary is an expensive string build: only format it when the
	// trace level is actually enabled, or every single packet on the
	// served segment pays for it even at the default info level
	if log.IsLevelEnabled(log.TraceLevel) {
		log.Tracef("(dhcp.dhcpHandler) INCOMING PACKET=%s", m.Summary())
	}

	if m.OpCode != dhcpv4.OpcodeBootRequest {
		log.Errorf("(dhcp.dhcpHandler) not a BootRequest!")

		return
	}

	// lease and pool resolution under one lock acquisition (see
	// packetLeaseAndPool): a flood on the shared interface must not stall
	// the allocator behind three serialized lookups per packet
	lease, pool, leaseFound, poolFound := a.packetLeaseAndPool(m.ClientHWAddr.String())

	if !leaseFound {
		a.logUnknownHWAddr(m)

		return
	}

	if !poolFound {
		// the lease's pool is gone (deleted while the vm still runs, or the
		// brief reload window): a request which asks for an address this
		// server can no longer serve gets a nak so the client restarts the
		// discovery instead of retransmitting indefinitely against a silent
		// drop; other message types are dropped, there is nothing to offer
		log.Warnf("(dhcp.dhcpHandler) NO MATCHED POOL FOUND FOR LEASE: hwaddr=%s", m.ClientHWAddr.String())

		if m.MessageType() == dhcpv4.MessageTypeRequest {
			serverIP := m.ServerIdentifier()
			if len(serverIP) == 0 {
				serverIP = net.IPv4zero
			}

			a.sendNak(conn, m, serverIP)
		}

		return
	}

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
	// informReply marks a DHCPINFORM ack: rfc 2131 4.3.5 says it carries
	// the configuration options only - no yiaddr and no lease time
	informReply := false

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

			a.sendNak(conn, m, pool.ServerIP)

			return
		}

		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPREQUEST for %s from %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)

		replyType = dhcpv4.MessageTypeAck
		sendReply = true
	case dhcpv4.MessageTypeInform:
		// rfc 2131 4.3.5: a client which already has an address asks only
		// for its configuration parameters; the ack carries the options
		// without yiaddr and without a lease time
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPINFORM from %s via %s", m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic)

		replyType = dhcpv4.MessageTypeAck
		sendReply = true
		informReply = true
	case dhcpv4.MessageTypeDecline:
		// rfc 2131 4.3.3: the client reports an on-segment conflict for
		// the offered address. the pre-allocated model keeps the lease:
		// the address belongs to this binding by the controller's ledger,
		// and abandoning it under the dhcp lock is not possible without
		// the ipam allocator (whose lock is only ever taken after this
		// one). the conflict stays visible here; the binding's resync
		// re-serves the same address by design
		log.Errorf("(dhcp.dhcpHandler) [txid=%s] DHCPDECLINE for %s from %s: client reports an address conflict for a pre-allocated lease, keeping the reservation (see the ipam/ippool status for the binding)",
			m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String())

		return
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
	// request, which is set during renewal and zero during address selection;
	// an inform ack carries no yiaddr at all (rfc 2131 4.3.5)
	reply.ServerIPAddr = pool.ServerIP
	reply.TransactionID = m.TransactionID
	reply.ClientHWAddr = m.ClientHWAddr
	reply.Flags = m.Flags
	reply.GatewayIPAddr = m.GatewayIPAddr
	if !informReply {
		reply.YourIPAddr = lease.ClientIP
	}
	if replyType == dhcpv4.MessageTypeAck && !informReply {
		reply.ClientIPAddr = m.ClientIPAddr
	}

	reply.UpdateOption(dhcpv4.OptMessageType(replyType))
	reply.UpdateOption(dhcpv4.OptServerIdentifier(pool.ServerIP))
	reply.UpdateOption(dhcpv4.OptSubnetMask(pool.SubnetMask))

	// an unset router must not produce a zero-length option (code 3 with
	// no payload), which strict client parsers drop
	if len(pool.Router) > 0 {
		reply.UpdateOption(dhcpv4.OptRouter(pool.Router))
	}

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

	if !informReply {
		if pool.LeaseTime > 0 {
			reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(time.Duration(pool.LeaseTime) * time.Second))
		} else {
			// default lease time: 1 year
			reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(31536000 * time.Second))
		}
	}
	if replyType == dhcpv4.MessageTypeOffer {
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPOFFER on %s to %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
	} else if informReply {
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPACK (inform) to %s via %s", m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic)
	} else {
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPACK on %s to %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
	}

	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		log.Errorf("(dhcp.dhcpHandler) Cannot reply to client: %v", err)
	}
}

// sendNak tells the client that its request does not match the lease state,
// so it restarts the discovery. A nak contains no lease options. rfc 2131
// section 3.2: a request which arrived without a relay address gets the nak
// broadcast to the 0xffffffff address, because the client may not hold a
// valid network address or subnet mask and may not answer arp requests;
// unicasting to the rejected claim address would leave such a client
// without the nak. a request which came through a bootp relay agent gets
// the nak sent to the relay address, which forwards it to the client's
// hardware address.
func (a *DHCPAllocator) sendNak(conn net.PacketConn, m *dhcpv4.DHCPv4, serverIP net.IP) {
	reply, err := dhcpv4.NewReplyFromRequest(m, dhcpv4.WithMessageType(dhcpv4.MessageTypeNak))
	if err != nil {
		log.Errorf("(dhcp.dhcpHandler) building DHCPNAK failed: %v", err)

		return
	}

	reply.UpdateOption(dhcpv4.OptServerIdentifier(serverIP))

	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ClientPort}
	if !m.GatewayIPAddr.Equal(net.IPv4zero) {
		// rfc 2131 section 4.3.2: a relayed client may not hold a valid
		// network address or subnet mask and may not answer arp requests:
		// the server MUST set the broadcast bit in the nak so the relay
		// agent broadcasts it to the client (the request's own flags are
		// copied by the reply builder, but the bit is set regardless)
		reply.SetBroadcast()

		// the relay agent forwards the nak towards the client's hardware
		// address
		dst = &net.UDPAddr{IP: m.GatewayIPAddr, Port: dhcpv4.ServerPort}
	}

	if _, err := conn.WriteTo(reply.ToBytes(), dst); err != nil {
		log.Errorf("(dhcp.dhcpHandler) Cannot send DHCPNAK to %s: %v", dst, err)
	}
}

// Run starts the DHCP service for the pool identified by networkName,
// serving on nic. the server is registered under the pool identity before
// serving starts, so a concurrent Stop always finds the entry. a second Run
// for the same network is rejected instead of overwriting the registry
// entry and orphaning the live server (socket and serve goroutine) which
// Stop could then never reach.
func (a *DHCPAllocator) Run(networkName string, nic string) (err error) {
	log.Infof("(dhcp.Run) starting DHCP service for network %s on nic %s", networkName, nic)

	// we need to listen on 0.0.0.0 otherwise client discovers will not be answered
	laddr := net.UDPAddr{
		IP:   net.ParseIP("0.0.0.0"),
		Port: 67,
	}

	a.mutex.Lock()
	if _, exists := a.servers[networkName]; exists {
		a.mutex.Unlock()

		return fmt.Errorf("%w: network %s", ErrServerAlreadyRunning, networkName)
	}

	// several pools on one interface share the 0.0.0.0:67 socket group;
	// whether the kernel duplicates broadcast packets to every reuseport
	// socket of the group is kernel-version dependent, so the setup is
	// surfaced instead of silently relying on it
	for registeredNetwork, registeredNic := range a.serverNics {
		if registeredNic == nic {
			log.Warnf("(dhcp.Run) network %s serves on the same interface %s as network %s: broadcast dhcp packets are delivered to every socket of the shared interface (reuseport group), the duplication semantics depend on the deployment kernel",
				networkName, nic, registeredNetwork)
			break
		}
	}

	server, err := server4.NewServer(nic, &laddr, a.dhcpHandler)
	if err != nil {
		a.mutex.Unlock()

		return
	}

	a.servers[networkName] = server
	a.serverNics[networkName] = nic
	a.mutex.Unlock()

	// the serve loop never returns while servicing; it returns only on a
	// socket error or a Stop-initiated close. the wrapper deregisters the
	// entry on an unexpected exit, so CheckPool/IsRunning stop reporting
	// the pool as live, and the ippool controller's listener repair
	// re-serves the pool on the next event or resync - instead of the pool
	// silently never answering dhcp again while every lookup still
	// believes it runs
	go func() {
		serveErr := server.Serve()

		a.mutex.Lock()
		_, stillRegistered := a.servers[networkName]
		if stillRegistered {
			delete(a.servers, networkName)
			delete(a.serverNics, networkName)
		}
		a.mutex.Unlock()

		if serveErr != nil && stillRegistered {
			log.Errorf("(dhcp.Run) the DHCP service of network %s terminated unexpectedly: %v; the pool is deregistered and the next pool sync re-serves it", networkName, serveErr)
		}
	}()

	return
}

// Stop stops and removes the DHCP service of the pool identified by
// networkName. stopping a network which is not running is a converged
// no-op, so cleanup paths can call it unconditionally.
func (a *DHCPAllocator) Stop(networkName string) (err error) {
	log.Infof("(dhcp.Stop) stopping DHCP service for network %s", networkName)

	a.mutex.Lock()
	server, exists := a.servers[networkName]
	delete(a.servers, networkName)
	delete(a.serverNics, networkName)
	a.mutex.Unlock()

	if !exists || server == nil {
		log.Debugf("(dhcp.Stop) no running dhcp service for network %s, nothing to stop", networkName)

		return
	}

	return server.Close()
}

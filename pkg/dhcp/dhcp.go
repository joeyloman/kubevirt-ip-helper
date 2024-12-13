package dhcp

import (
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/insomniacslk/dhcp/rfc1035label"
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
	_, exists := a.pools[name]
	return exists
}

func (a *DHCPAllocator) GetPool(name string) (pool DHCPPool) {
	return a.pools[name]
}

func (a *DHCPAllocator) DeletePool(name string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if !a.CheckPool(name) {
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

	if _, err := net.ParseMAC(hwAddr); err != nil {
		return fmt.Errorf("hwaddr %s is not valid", hwAddr)
	}

	if a.CheckLease(hwAddr) {
		return fmt.Errorf("lease for hwaddr %s already exists", hwAddr)
	}

	lease := DHCPLease{}
	lease.PoolName = poolName
	lease.ClientIP = net.ParseIP(clientIP)
	lease.Reference = ref

	a.leases[hwAddr] = lease

	log.Debugf("(dhcp.AddLease) lease added for hardware address: %s", hwAddr)

	return
}

func (a *DHCPAllocator) CheckLease(hwAddr string) bool {
	_, exists := a.leases[hwAddr]
	return exists
}

func (a *DHCPAllocator) GetLease(hwAddr string) (lease DHCPLease) {
	return a.leases[hwAddr]
}

func (a *DHCPAllocator) DeleteLease(hwAddr string) (err error) {
	a.mutex.Lock()
	defer a.mutex.Unlock()

	if !a.CheckLease(hwAddr) {
		return fmt.Errorf("lease for hwaddr %s does not exists", hwAddr)
	}

	delete(a.leases, hwAddr)

	log.Debugf("(dhcp.DeleteLease) lease deleted for hardware address: %s", hwAddr)

	return
}

func (a *DHCPAllocator) Usage() {
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

	reply, err := dhcpv4.NewReplyFromRequest(m)
	if err != nil {
		log.Errorf("(dhcp.dhcpHandler) NewReplyFromRequest failed: %v", err)
		return
	}

	lease := a.leases[m.ClientHWAddr.String()]

	if lease.ClientIP == nil {
		log.Warnf("(dhcp.dhcpHandler) NO LEASE FOUND: hwaddr=%s", m.ClientHWAddr.String())

		return
	}

	if !a.CheckPool(lease.PoolName) {
		log.Warnf("(dhcp.dhcpHandler) NO MATCHED POOL FOUND FOR LEASE: hwaddr=%s", m.ClientHWAddr.String())

		return
	}
	pool := a.pools[lease.PoolName]

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

	reply.ClientIPAddr = lease.ClientIP
	reply.ServerIPAddr = pool.ServerIP
	reply.YourIPAddr = lease.ClientIP
	reply.TransactionID = m.TransactionID
	reply.ClientHWAddr = m.ClientHWAddr
	reply.Flags = m.Flags
	reply.GatewayIPAddr = m.GatewayIPAddr

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

	switch mt := m.MessageType(); mt {
	case dhcpv4.MessageTypeDiscover:
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPDISCOVER from %s via %s", m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic)
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPOFFER on %s to %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
	case dhcpv4.MessageTypeRequest:
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPREQUEST for %s from %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
		log.Infof("(dhcp.dhcpHandler) [txid=%s] DHCPACK on %s to %s via %s", m.TransactionID.String(), lease.ClientIP, m.ClientHWAddr.String(), pool.Nic)
	default:
		log.Warnf("(dhcp.dhcpHandler) [txid=%s] Unhandled message type for %s via %s: %v", m.TransactionID.String(), m.ClientHWAddr.String(), pool.Nic, mt)
		return
	}

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

	a.servers[nic] = server

	return
}

func (a *DHCPAllocator) Stop(nic string) (err error) {
	log.Infof("(dhcp.Stop) stopping DHCP service on nic %s", nic)

	return a.servers[nic].Close()
}

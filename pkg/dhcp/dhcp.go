package dhcp

import (
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

type DHCPLease struct {
	ServerIP   net.IP
	ClientIP   net.IP
	SubnetMask net.IPMask
	Router     net.IP
	DNS        []net.IP
}

type DHCPAllocator struct {
	leases map[string]DHCPLease
}

func NewDHCPAllocator() *DHCPAllocator {
	//log.Infof("(dhcp.NewDHCPAllocator) NewDHCPAllocator")

	leases := make(map[string]DHCPLease)

	return &DHCPAllocator{
		leases: leases,
	}
}

func (a *DHCPAllocator) AddLease(hwAddr string, serverIP string, clientIP string, subnetMask string, routerIP string, DNSServers []string) (err error) {
	log.Infof("(dhcp.AddLease) adding lease for hardware address: %s", hwAddr)

	lease := DHCPLease{}
	lease.ServerIP = net.ParseIP(serverIP)
	lease.ClientIP = net.ParseIP(clientIP)
	lease.SubnetMask = net.IPMask(net.ParseIP(subnetMask))
	lease.Router = net.ParseIP(routerIP)
	for i := 0; i < len(DNSServers); i++ {
		lease.DNS = append(lease.DNS, net.ParseIP(DNSServers[i]))
	}

	a.leases[hwAddr] = lease

	return
}

func (a *DHCPAllocator) CheckLease(hwAddr string) bool {
	log.Infof("(dhcp.CheckLease) checking lease for hardware address: %s", hwAddr)

	_, exists := a.leases[hwAddr]
	return exists
}

func (a *DHCPAllocator) DeleteLease(hwaddr string) {
	log.Infof("(dhcp.DeleteLease) deleting lease for hardware address: %s", hwaddr)
	delete(a.leases, hwaddr)
}

func (a *DHCPAllocator) Usage() {
	for hwaddr, lease := range a.leases {
		log.Infof("lease: hwaddr=%s, clientip=%s, netmask=%s, router=%s, dns=%+v",
			hwaddr,
			lease.ClientIP.String(),
			lease.SubnetMask.String(),
			lease.Router.String(),
			lease.DNS,
		)
	}
}

func New() *DHCPAllocator {
	log.Infof("(dhcp.New) allocating new leases")

	return NewDHCPAllocator()
}

func (a *DHCPAllocator) dhcpHandler(conn net.PacketConn, peer net.Addr, m *dhcpv4.DHCPv4) {
	log.Infof("(dhcp.dhcpHandler) start")

	if m == nil {
		log.Infof("Packet is nil!")
		return
	}

	//log.Infof("INCOMING PACKET=%s", m.Summary())

	if m.OpCode != dhcpv4.OpcodeBootRequest {
		log.Infof("Not a BootRequest!")
		return
	}

	reply, err := dhcpv4.NewReplyFromRequest(m)
	if err != nil {
		log.Infof("NewReplyFromRequest failed: %v", err)
		return
	}

	lease := a.leases[m.ClientHWAddr.String()]

	log.Infof("LEASE FOUND: serverip=%s, clientip=%s, mask=%s, router=%s, dns=%+v",
		lease.ServerIP.String(), lease.ClientIP.String(), lease.SubnetMask.String(), lease.Router.String(), lease.DNS)

	/*
		DHCP RFC2131: https://datatracker.ietf.org/doc/html/rfc2131#page-13
	*/

	/*
	   2023/05/15 05:46:49 PACKET=DHCPv4 Message
	     opcode: BootRequest
	     hwtype: Ethernet
	     hopcount: 0
	     transaction ID: 0x702f5ef8
	     num seconds: 14
	     flags: Unicast (0x00)
	     client IP: 0.0.0.0
	     your IP: 0.0.0.0
	     server IP: 0.0.0.0
	     gateway IP: 0.0.0.0
	     client MAC: 52:54:00:84:c4:5b
	     server hostname:
	     bootfile name:
	     options:
	       DHCP Message Type: DISCOVER
	       Parameter Request List: Subnet Mask, Router, Domain Name Server, Host Name, Domain Name, Interface MTU, Static Routing Table, NTP Servers, DNS Domain Search List, SIP Servers, Classless Static Route
	       Maximum DHCP Message Size: 576
	       Client identifier: [255 93 226 108 21 0 2 0 0 171 17 50 197 33 155 212 98 114 132]
	       FQDN: [5 0 0 15 103 111 45 100 104 99 112 45 99 108 105 101 110 116 49 3 108 97 98 7 97 116 109 111 115 98 118 2 110 108 0]
	   2023/05/15 05:46:49 DISCOVER: hwaddr=52:54:00:84:c4:5b
	*/

	/*
		ciaddr [4]
		Client IP address; only filled in if client is in
		BOUND, RENEW or REBINDING state and can respond
		to ARP requests.
	*/
	reply.ClientIPAddr = lease.ClientIP

	/*
		siaddr [4]
		IP address of next server to use in bootstrap;
		returned in DHCPOFFER, DHCPACK by server.
	*/
	reply.ServerIPAddr = lease.ServerIP

	/*
		yiaddr [4]
		'your' (client) IP address.
	*/
	reply.YourIPAddr = lease.ClientIP

	/*
		xid [4]
		Transaction ID, a random number chosen by the
		client, used by the client and server to associate
		messages and responses between a client and a
		server.
	*/
	reply.TransactionID = m.TransactionID

	/*
		chaddr [16]
		Client hardware address.
	*/
	reply.ClientHWAddr = m.ClientHWAddr

	/*
		flags [2]
		Client flags
	*/
	reply.Flags = m.Flags

	/*
		giaddr [4]
		Relay agent IP address, used in booting via a
		relay agent.
	*/
	reply.GatewayIPAddr = m.GatewayIPAddr

	/*
		options
		Returned DHCP options.
	*/
	reply.UpdateOption(dhcpv4.OptServerIdentifier(lease.ServerIP))
	reply.UpdateOption(dhcpv4.OptSubnetMask(lease.SubnetMask))
	reply.UpdateOption(dhcpv4.OptRouter(lease.Router))
	reply.UpdateOption(dhcpv4.OptDNS(lease.DNS...))
	reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(3 * time.Minute))
	//reply.UpdateOption(dhcpv4.OptBroadcastAddress(net.IP{192, 168, 10, 255}))
	//reply.UpdateOption(dhcpv4.OptClassIdentifier("k8s"))
	//reply.UpdateOption(dhcpv4.OptDomainName("example.com"))

	switch mt := m.MessageType(); mt {
	case dhcpv4.MessageTypeDiscover:
		log.Infof("DHCPDISCOVER: %+v", m)
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeOffer))
		log.Infof("DHCPOFFER: %+v", reply)
	case dhcpv4.MessageTypeRequest:
		log.Infof("DHCPREQUEST: %+v", m)
		reply.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeAck))
		log.Infof("DHCPACK: %+v", reply)
	default:
		log.Infof("Unhandled message type: %v", mt)
		return
	}

	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		log.Infof("Cannot reply to client: %v", err)
	}
}

func (a *DHCPAllocator) Run() {
	log.Infof("(dhcp.Run) starting DHCP service")
	// laddr := net.UDPAddr{
	// 	IP:   net.ParseIP("0.0.0.0"),
	// 	Port: 67,
	// }

	// var nics []string
	// nics = append(nics, "enp2s0")
	// nics = append(nics, "enp3s0")

	// for _, nic := range nics {
	// 	log.Infof("Serving on nic: %v", nic)

	// 	server, err := server4.NewServer(nic, &laddr, a.dhcpHandler)
	// 	if err != nil {
	// 		log.Fatal(err)
	// 	}

	// 	go server.Serve()
	// }

	// for {
	// 	time.Sleep(1138800 * time.Hour)
	// }
}

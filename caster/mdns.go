package caster

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

// DiscoverAirPlayIP sends a single multicast DNS (mDNS) PTR query for
// "_airplay._tcp.local" to 224.0.0.251:5353 and returns the IPv4 address of
// the responder whose DNS packet advertises deviceName (case-insensitive, e.g.
// "Office Tv").
//
// Why do we want this?
// Without router admin access for a static DHCP reservation, the AIMIBO might
// get a new LAN IP after a power outage or router reboot. If the configured
// DeviceHost stops answering on port 7000, the supervisor calls this function
// to rediscover the dongle automatically.
//
// Note: mDNS multicast only works when the container has direct L2 access to
// the LAN (i.e. `network_mode: host` in Docker Compose).
func DiscoverAirPlayIP(deviceName string, timeout time.Duration) (string, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return "", fmt.Errorf("mdns listen: %w", err)
	}
	defer conn.Close()

	query := buildMDNSPTRQuery("_airplay._tcp.local")
	mdnsAddr := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	if _, err := conn.WriteToUDP(query, mdnsAddr); err != nil {
		return "", fmt.Errorf("mdns send: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	want := strings.ToLower(strings.TrimSpace(deviceName))
	buf := make([]byte, 4096)

	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", fmt.Errorf("mdns: %q not found within %s", deviceName, timeout)
		}
		if packetMentionsName(buf[:n], want) {
			return src.IP.String(), nil
		}
	}
}

// buildMDNSPTRQuery constructs a minimal RFC 1035 / RFC 6762 DNS query packet
// asking for PTR (type 12), class IN (1) records for qname.
func buildMDNSPTRQuery(qname string) []byte {
	// 12-byte DNS header: ID=0, Flags=0 (standard query), QDCOUNT=1, rest=0.
	pkt := make([]byte, 12)
	binary.BigEndian.PutUint16(pkt[4:6], 1)

	// Encode each dot-separated label as [length][bytes...], terminated by 0x00.
	for _, label := range strings.Split(strings.TrimSuffix(qname, "."), ".") {
		pkt = append(pkt, byte(len(label)))
		pkt = append(pkt, label...)
	}
	pkt = append(pkt, 0x00)

	// QTYPE = 12 (PTR), QCLASS = 1 (IN)
	pkt = append(pkt, 0x00, 0x0c, 0x00, 0x01)
	return pkt
}

// packetMentionsName checks whether any printable substring in a raw mDNS
// response contains wantLower (for example, "office tv"). Every AirPlay mDNS
// responder includes its human-readable instance name in its PTR/SRV labels.
func packetMentionsName(pkt []byte, wantLower string) bool {
	if wantLower == "" {
		return false
	}
	// Convert non-printable bytes to spaces so label length prefixes don't glue words together.
	var sb strings.Builder
	sb.Grow(len(pkt))
	for _, b := range pkt {
		if b >= 32 && b < 127 {
			sb.WriteByte(b)
		} else {
			sb.WriteByte(' ')
		}
	}
	return strings.Contains(strings.ToLower(sb.String()), wantLower)
}

// detectOutboundIP asks the kernel routing table which local IPv4 address would
// be used to reach targetHost (no packets are actually sent over UDP dial).
// This lets StreamHost stay optional in Config when running on a single-NIC box.
func detectOutboundIP(targetHost string) (string, error) {
	conn, err := net.Dial("udp4", net.JoinHostPort(targetHost, "7000"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected local addr %v", conn.LocalAddr())
	}
	return localAddr.IP.String(), nil
}

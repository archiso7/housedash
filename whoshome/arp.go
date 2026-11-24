// Package whoshome scans the local LAN with ARP requests (RFC 826) so you can
// detect which devices are currently on the network without router access.
//
// It uses github.com/mdlayher/arp, which talks to Linux AF_PACKET sockets in
// pure Go — no CGO, no libpcap-dev, and works with CGO_ENABLED=0.
// On Linux/Docker, the process needs CAP_NET_RAW (e.g. `docker run --cap-add NET_RAW`).
package whoshome

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"time"

	"github.com/mdlayher/arp"
)

// Scan opens an ARP socket on ifaceName (e.g. "eth0"), broadcasts ARP requests
// across its IPv4 subnet every interval, and calls onReply for every ARP reply
// received from another host on the LAN.
//
// Scan blocks until ctx is cancelled or a fatal socket error occurs.
func Scan(ctx context.Context, ifaceName string, interval time.Duration, onReply func(ip netip.Addr, mac net.HardwareAddr)) error {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return fmt.Errorf("lookup interface %q: %w", ifaceName, err)
	}

	prefix, err := ipv4Prefix(iface)
	if err != nil {
		return fmt.Errorf("interface %q: %w", ifaceName, err)
	}
	log.Printf("[whoshome] scanning %s on %s (local MAC %s)", prefix, iface.Name, iface.HardwareAddr)

	// arp.Dial opens a raw L2 ARP socket bound to iface (requires CAP_NET_RAW on Linux).
	client, err := arp.Dial(iface)
	if err != nil {
		return fmt.Errorf("arp dial %q (did you pass --cap-add NET_RAW?): %w", iface.Name, err)
	}

	// Closing client when ctx is cancelled unblocks client.Read() in the reader goroutine.
	go func() {
		<-ctx.Done()
		_ = client.Close()
	}()
	defer client.Close()

	// Listen for incoming ARP replies in the background while we sweep.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		readReplies(ctx, client, iface.HardwareAddr, onReply)
	}()

	<-readDone
	return ctx.Err()
}

func readReplies(ctx context.Context, client *arp.Client, selfMAC net.HardwareAddr, onReply func(netip.Addr, net.HardwareAddr)) {
	for {
		pkt, _, err := client.Read()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[whoshome] arp read error: %v", err)
			return
		}

		// Ignore ARP requests and any packets originating from our own NIC.
		if bytes.Equal(selfMAC, pkt.SenderHardwareAddr) || pkt.SenderIP.IsUnspecified() {
			continue
		}

		if onReply != nil {
			// Copy the MAC slice so callers can safely store it across reads.
			mac := make(net.HardwareAddr, len(pkt.SenderHardwareAddr))
			copy(mac, pkt.SenderHardwareAddr)
			onReply(pkt.SenderIP, mac)
		}
	}
}

// ipv4Prefix returns the first usable non-loopback IPv4 CIDR prefix on iface
// (for example, 10.52.0.4/24) and refuses masks wider than /16 so we never
// accidentally sweep millions of addresses.
func ipv4Prefix(iface *net.Interface) (netip.Prefix, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Prefix{}, err
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil || ip4[0] == 127 {
			continue
		}
		ones, bits := ipnet.Mask.Size()
		if bits != 32 || ones < 16 {
			return netip.Prefix{}, fmt.Errorf("subnet mask /%d is too large to ARP sweep", ones)
		}
		addr, ok := netip.AddrFromSlice(ip4)
		if !ok {
			continue
		}
		return netip.PrefixFrom(addr, ones), nil
	}
	return netip.Prefix{}, errors.New("no usable IPv4 address found")
}

// subnetHostIPs returns every usable host IPv4 address in prefix (skipping the
// network address at the start and the broadcast address at the end).
func subnetHostIPs(prefix netip.Prefix) []netip.Addr {
	masked := prefix.Masked()
	start := masked.Addr().As4()
	bits := masked.Bits()
	if bits >= 31 {
		return nil
	}

	netU32 := binary.BigEndian.Uint32(start[:])
	hostMask := uint32((1 << (32 - bits)) - 1)
	bcastU32 := netU32 | hostMask

	out := make([]netip.Addr, 0, bcastU32-netU32-1)
	for cur := netU32 + 1; cur < bcastU32; cur++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], cur)
		out = append(out, netip.AddrFrom4(b))
	}
	return out
}

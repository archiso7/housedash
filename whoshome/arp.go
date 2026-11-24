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
	"sync"
	"time"

	"github.com/mdlayher/arp"
)

type Device struct {
	MAC      net.HardwareAddr
	IP       netip.Addr
	LastSeen time.Time
	IsHome   bool
	Name     string
}

type Tracker struct {
	ifaceName string
	iface     *net.Interface
	prefix    netip.Prefix
	client    *arp.Client

	probeAfter time.Duration
	probeEvery time.Duration
	awayAfter  time.Duration

	mu      sync.Mutex
	devices map[string]*Device
}

func NewTracker(ifaceName string, devices []Device) *Tracker {
	devMap := make(map[string]*Device, len(devices))
	for _, d := range devices {
		dev := d // copy so each map entry has its own struct
		devMap[dev.MAC.String()] = &dev
	}

	return &Tracker{
		ifaceName:  ifaceName,
		probeAfter: 2 * time.Minute,
		probeEvery: 1 * time.Minute,
		awayAfter:  15 * time.Minute,
		devices:    devMap,
	}
}

func (t *Tracker) Run(ctx context.Context) error {
	iface, err := net.InterfaceByName(t.ifaceName)
	if err != nil {
		return fmt.Errorf("lookup interface %q: %w", t.ifaceName, err)
	}

	prefix, err := ipv4Prefix(iface)
	if err != nil {
		return fmt.Errorf("interface %q: %w", t.ifaceName, err)
	}
	log.Printf("[whoshome] scanning %s on %s (local MAC %s)", prefix, iface.Name, iface.HardwareAddr)

	client, err := arp.Dial(iface)
	if err != nil {
		return fmt.Errorf("arp dial %q (did you pass --cap-add NET_RAW?): %w", iface.Name, err)
	}

	t.iface = iface
	t.prefix = prefix
	t.client = client

	go func() {
		<-ctx.Done()
		_ = client.Close()
	}()
	defer client.Close()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		t.listen(ctx)
	}()

	t.probeLoop(ctx)
	<-readDone
	return ctx.Err()
}

func (t *Tracker) listen(ctx context.Context) {
	for {
		pkt, _, err := t.client.Read()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("[whoshome] arp read error: %v", err)
			return
		}

		if bytes.Equal(t.iface.HardwareAddr, pkt.SenderHardwareAddr) || pkt.SenderIP.IsUnspecified() {
			continue
		}

		t.markSeen(pkt.SenderHardwareAddr, pkt.SenderIP)
	}
}

func (t *Tracker) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(t.probeEvery)
	defer ticker.Stop()

	for {
		t.probeOnce(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *Tracker) probeOnce(ctx context.Context) {
	toProbe, needDiscovery := t.checkDevices()

	for _, d := range toProbe {
		if ctx.Err() != nil {
			return
		}
		if err := t.sendUnicastARP(d.MAC, d.IP); err != nil && ctx.Err() == nil {
			log.Printf("[whoshome] unicast ARP to %s (%s) failed: %v", d.Name, d.IP, err)
		}
	}

	if needDiscovery {
		for _, ip := range subnetHostIPs(t.prefix) {
			if ctx.Err() != nil {
				return
			}
			if err := t.client.Request(ip); err != nil && ctx.Err() == nil {
				log.Printf("[whoshome] discovery ARP for %s failed: %v", ip, err)
				return
			}
		}
	}
}

func (t *Tracker) checkDevices() (toProbe []Device, needDiscovery bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, d := range t.devices {
		silentFor := time.Since(d.LastSeen)

		if d.IsHome && silentFor > t.awayAfter {
			d.IsHome = false
			d.IP = netip.Addr{}
			log.Printf("[whoshome] %s left (silent for %s)", d.Name, silentFor.Round(time.Second))
			continue
		}

		if d.LastSeen.IsZero() || silentFor > t.probeAfter {
			if d.IP.IsValid() {
				toProbe = append(toProbe, *d)
			} else {
				needDiscovery = true
			}
		}
	}
	return toProbe, needDiscovery
}

// sendUnicastARP sends a single Layer 2 unicast ARP request straight to dstMAC
// asking for dstIP. Unlike broadcast ARP, unicast wakes sleeping Wi-Fi clients
// via the AP's TIM without flooding the LAN.
func (t *Tracker) sendUnicastARP(dstMAC net.HardwareAddr, dstIP netip.Addr) error {
	pkt, err := arp.NewPacket(
		arp.OperationRequest,
		t.iface.HardwareAddr,
		t.prefix.Addr(),
		dstMAC,
		dstIP,
	)
	if err != nil {
		return err
	}
	return t.client.WriteTo(pkt, dstMAC)
}

func (t *Tracker) markSeen(mac net.HardwareAddr, ip netip.Addr) {
	t.mu.Lock()
	defer t.mu.Unlock()

	d, ok := t.devices[mac.String()]
	if !ok {
		return // early return still unlocks because of defer
	}
	d.LastSeen = time.Now()
	d.IP = ip
	if !d.IsHome {
		d.IsHome = true
		log.Printf("[whoshome] %s came home (%s)", d.Name, ip)
	}
}

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

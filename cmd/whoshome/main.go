// Command whoshome is a small CLI runner for testing the ARP scanner in
// package whoshome on your server's LAN.
//
// Example on 10.52.0.4 (needs --network host and --cap-add NET_RAW):
//
//	docker run --rm --network host --cap-add NET_RAW \
//	  -v /opt/housedash:/src -v go-mod-cache:/go/pkg/mod -w /src \
//	  golang:1.25-bookworm go run ./cmd/whoshome
//
// Optionally filter to a single MAC via WATCH_MAC=aa:bb:cc:dd:ee:ff.
package main

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/archiso7/housedash/whoshome"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	iface := envOr("IFACE", "eth0")

	var watchMAC net.HardwareAddr
	if raw := os.Getenv("WATCH_MAC"); raw != "" {
		parsed, err := net.ParseMAC(raw)
		if err != nil {
			log.Fatalf("invalid WATCH_MAC %q: %v", raw, err)
		}
		watchMAC = parsed
		log.Printf("watching for MAC %s", watchMAC)
	}

	err := whoshome.Scan(ctx, iface, 10*time.Second, func(ip netip.Addr, mac net.HardwareAddr) {
		if watchMAC != nil && !bytes.Equal(watchMAC, mac) {
			return
		}
		log.Printf("IP %-15s is at %s", ip, mac)
	})
	if err != nil && err != context.Canceled {
		log.Fatalf("scan failed: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

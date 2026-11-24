// Command whoshome is a small CLI runner for testing the presence tracker in
// package whoshome on your server's LAN.
//
// Example on 10.52.0.4:
//
//	WATCH_MAC=aa:bb:cc:dd:ee:ff ./dev.sh run ./cmd/whoshome
//
// Optional env vars:
//   - IFACE (default: eth0)
//   - WATCH_NAME (default: Liam)
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/archiso7/housedash/whoshome"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	iface := envOr("IFACE", "eth0")
	name := envOr("WATCH_NAME", "Liam")

	rawMAC := os.Getenv("WATCH_MAC")
	if rawMAC == "" {
		log.Fatalf("WATCH_MAC is required (e.g. WATCH_MAC=aa:bb:cc:dd:ee:ff ./dev.sh run ./cmd/whoshome)")
	}
	watchMAC, err := net.ParseMAC(rawMAC)
	if err != nil {
		log.Fatalf("invalid WATCH_MAC %q: %v", rawMAC, err)
	}

	devices := []whoshome.Device{
		{Name: name, MAC: watchMAC},
	}

	log.Printf("tracking %s (%s) on %s", name, watchMAC, iface)
	tracker := whoshome.NewTracker(iface, devices)
	if err := tracker.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("tracker failed: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

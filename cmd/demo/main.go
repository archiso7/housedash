// Command demo is a minimal runnable example showing how to use the
// github.com/archiso7/housedash/caster package.
//
// By default it serves the embedded demo.html page on http://127.0.0.1:8090/
// and casts it to the AIMIBO dongle. If you set TARGET_URL, it casts your own
// dashboard URL instead!
package main

import (
	"context"
	_ "embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/archiso7/housedash/caster"
)

//go:embed demo.html
var demoHTML []byte

func main() {
	// Cancel cleanly on Ctrl-C or `docker stop`.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	targetURL := envOr("TARGET_URL", "")
	if targetURL == "" {
		// No external dashboard URL provided — start the built-in demo page.
		demoAddr := envOr("DEMO_ADDR", "127.0.0.1:8090")
		startDemoServer(ctx, demoAddr)
		targetURL = "http://" + demoAddr + "/"
		log.Printf("[demo] serving built-in demo page at %s", targetURL)
	} else {
		log.Printf("[demo] casting custom TARGET_URL=%s", targetURL)
	}

	c := caster.New(caster.Config{
		TargetURL:   targetURL,
		DeviceHost:  envOr("AIMIBO_HOST", "10.52.0.18"),
		DevicePort:  envInt("AIMIBO_PORT", 7000),
		DeviceName:  envOr("AIMIBO_NAME", "Office Tv"),
		StreamHost:  envOr("STREAM_HOST", ""),
		HTTPAddr:    envOr("HLS_HTTP_ADDR", ":8899"),
		HLSDir:      envOr("HLS_DIR", "/tmp/hls"),
		Width:       envInt("SCREEN_WIDTH", 1920),
		Height:      envInt("SCREEN_HEIGHT", 1080),
		FPS:         envInt("STREAM_FPS", 2),
		SegmentSec:  envInt("HLS_SEGMENT_SEC", 4),
		ListSize:    envInt("HLS_LIST_SIZE", 5),
		ChromiumBin: envOr("CHROMIUM_BIN", "chromium"),
	})

	if err := c.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("[demo] caster exited with error: %v", err)
	}
	log.Printf("[demo] shut down cleanly")
}

func startDemoServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(demoHTML)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[demo] HTTP server error: %v", err)
		}
	}()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

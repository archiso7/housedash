// Command demo is a runnable example showing how to use the
// github.com/archiso7/housedash/caster package.
//
// By default it starts a tiny demo web server on :8090 with:
//   - GET /          : 1920x1080 plasma-safe TV view (what Chromium casts)
//   - GET /controls  : mobile/laptop control page to send messages, play a TV
//                      chime, and toggle night blackout live
//   - GET/POST /api/state : simple JSON state shared between /controls and /
//
// Once you build your own dashboard, set TARGET_URL=http://127.0.0.1:<port>/
// (or import github.com/archiso7/housedash/caster directly in your Go binary).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/archiso7/housedash/caster"
)

//go:embed demo.html
var demoHTML []byte

//go:embed controls.html
var controlsHTML []byte

// demoState is the tiny in-memory state manipulated by /controls and polled by
// the TV view on /. Replace this with SQLite in your real dashboard!
type demoState struct {
	mu            sync.Mutex
	Message       string `json:"message"`
	NightBlackout bool   `json:"night_blackout"`
	ChimeSeq      int    `json:"chime_seq"`
}

func main() {
	// Cancel cleanly on Ctrl-C or `docker stop`.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	targetURL := envOr("TARGET_URL", "")
	if targetURL == "" {
		// Bind on 0.0.0.0:8090 so you can open http://10.52.0.4:8090/controls
		// from your laptop or phone while Chromium loads http://127.0.0.1:8090/.
		demoAddr := envOr("DEMO_ADDR", ":8090")
		startDemoServer(ctx, demoAddr)

		_, port, err := net.SplitHostPort(demoAddr)
		if err != nil || port == "" {
			port = "8090"
		}
		targetURL = "http://127.0.0.1:" + port + "/"
		if chime := envOr("DEMO_CHIME", ""); chime != "" {
			targetURL += "?chime=" + chime
		}
		log.Printf("[demo] TV view at %s | interactive controls at http://<server-ip>:%s/controls", targetURL, port)
	} else {
		log.Printf("[demo] casting custom TARGET_URL=%s", targetURL)
	}

	c := caster.New(caster.Config{
		TargetURL:    targetURL,
		DeviceHost:   envOr("AIMIBO_HOST", "10.52.0.18"),
		DevicePort:   envInt("AIMIBO_PORT", 7000),
		DeviceName:   envOr("AIMIBO_NAME", "Office Tv"),
		StreamHost:   envOr("STREAM_HOST", ""),
		HTTPAddr:     envOr("HLS_HTTP_ADDR", ":8899"),
		HLSDir:       envOr("HLS_DIR", "/tmp/hls"),
		Width:        envInt("SCREEN_WIDTH", 1920),
		Height:       envInt("SCREEN_HEIGHT", 1080),
		FPS:          envInt("STREAM_FPS", 2),
		SegmentSec:   envInt("HLS_SEGMENT_SEC", 4),
		ListSize:     envInt("HLS_LIST_SIZE", 5),
		ChromiumBin:  envOr("CHROMIUM_BIN", "chromium"),
		EnableAudio:  envBool("ENABLE_AUDIO", true),
		AudioBitrate: envOr("AUDIO_BITRATE", "128k"),
	})

	if err := c.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("[demo] caster exited with error: %v", err)
	}
	log.Printf("[demo] shut down cleanly")
}

func startDemoServer(ctx context.Context, addr string) {
	state := &demoState{
		Message: "Open http://10.52.0.4:8090/controls to send a message or play a chime!",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(demoHTML)
	})
	mux.HandleFunc("GET /controls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(controlsHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	})
	mux.HandleFunc("POST /api/state", func(w http.ResponseWriter, r *http.Request) {
		var patch struct {
			Message       *string `json:"message"`
			NightBlackout *bool   `json:"night_blackout"`
			PlayChime     bool    `json:"play_chime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		state.mu.Lock()
		if patch.Message != nil {
			state.Message = *patch.Message
		}
		if patch.NightBlackout != nil {
			state.NightBlackout = *patch.NightBlackout
		}
		if patch.PlayChime {
			state.ChimeSeq++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
		state.mu.Unlock()
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

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

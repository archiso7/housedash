package caster

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// hlsServer serves the live.m3u8 playlist and .ts video segments produced by
// ffmpeg, and records the timestamp of the last segment request.
//
// Why track the last segment fetch?
// AirPlay's GET /playback-info sometimes keeps returning 200 OK even after the
// dongle stalls or the TV is power-cycled. Watching whether the AIMIBO is
// actually pulling a new .ts segment every ~4 seconds is the most reliable
// end-to-end health signal we have.
type hlsServer struct {
	dir        string
	addr       string
	srv        *http.Server
	lastSegNS  atomic.Int64 // unix nanos of last successful .ts segment fetch
	totalSegs  atomic.Int64
	logger     *log.Logger
}

func newHLSServer(dir, addr string, logger *log.Logger) *hlsServer {
	s := &hlsServer{
		dir:    dir,
		addr:   addr,
		logger: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleFile)

	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *hlsServer) handleFile(w http.ResponseWriter, r *http.Request) {
	// Clean the path so nobody can traverse outside HLSDir.
	clean := filepath.Base(r.URL.Path)
	if clean == "." || clean == "/" || clean == ".." {
		http.NotFound(w, r)
		return
	}

	// Only expose HLS playlists and MPEG-TS segments.
	var contentType string
	switch {
	case strings.HasSuffix(clean, ".m3u8"):
		contentType = "application/vnd.apple.mpegurl"
		// Playlists change every SegmentSec seconds and must never be cached.
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	case strings.HasSuffix(clean, ".ts"):
		contentType = "video/mp2t"
		s.lastSegNS.Store(time.Now().UnixNano())
		s.totalSegs.Add(1)
	default:
		http.NotFound(w, r)
		return
	}

	fullPath := filepath.Join(s.dir, clean)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// start launches the HTTP server in a background goroutine and shuts it down
// cleanly when ctx is cancelled.
func (s *hlsServer) start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
	}()

	go func() {
		s.logger.Printf("[hls] serving %s on http://%s/live.m3u8", s.dir, s.addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("[hls] server error: %v", err)
		}
	}()
	return nil
}

// resetWatchdog resets the segment-fetch timer right before we start a new
// AirPlay session so a fresh connect gets a full StallTimeout window.
func (s *hlsServer) resetWatchdog() {
	s.lastSegNS.Store(time.Now().UnixNano())
}

// timeSinceLastSegment reports how long it has been since the AIMIBO fetched
// a .ts segment.
func (s *hlsServer) timeSinceLastSegment() time.Duration {
	ts := s.lastSegNS.Load()
	if ts == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ts))
}

// hasReadyPlaylist checks whether live.m3u8 exists and already lists at least
// minSegments .ts files so the AIMIBO gets a clean initial playback buffer.
func (s *hlsServer) hasReadyPlaylist(minSegments int) bool {
	data, err := os.ReadFile(filepath.Join(s.dir, "live.m3u8"))
	if err != nil {
		return false
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), ".ts") {
			count++
		}
	}
	return count >= minSegments
}

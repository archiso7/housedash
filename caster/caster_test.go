package caster

import (
	"bufio"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAirPlayPlayAndPlaybackInfo verifies the exact raw HTTP/1.1 bytes our
// AirPlay 1 client puts on the wire (exact header casing, text/parameters,
// float Start-Position: 0.000000, persistent connection for /playback-info).
func TestAirPlayPlayAndPlaybackInfo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var gotPlayReq string
	var gotInfoReq string
	done := make(chan struct{})

	// Minimal fake AIMIBO server that speaks AirPlay 1 on one TCP connection.
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)

		// 1. Read POST /play headers + body
		req1, body1 := readRawHTTP(t, r)
		gotPlayReq = req1 + "\r\n" + body1
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")

		// 2. Read GET /playback-info on the same keep-alive socket
		req2, _ := readRawHTTP(t, r)
		gotInfoReq = req2
		plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
<key>duration</key>
<real>120.000000</real>
<key>position</key>
<real>12.500000</real>
<key>rate</key>
<real>1.000000</real>
<key>readyToPlay</key>
<true/>
</dict>
</plist>`
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/x-apple-plist+xml\r\nContent-Length: "+itoa(len(plist))+"\r\n\r\n"+plist)

		// 3. Read POST /stop
		_, _ = readRawHTTP(t, r)
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	for _, ch := range portStr {
		port = port*10 + int(ch-'0')
	}

	sess, err := DialAirPlay(host, port, 2*time.Second)
	if err != nil {
		t.Fatalf("DialAirPlay: %v", err)
	}

	if err := sess.Play("http://10.52.0.4:8899/live.m3u8"); err != nil {
		t.Fatalf("Play: %v", err)
	}

	info, err := sess.PlaybackInfo()
	if err != nil {
		t.Fatalf("PlaybackInfo: %v", err)
	}
	if !info.ReadyToPlay || info.Duration != 120 || info.Position != 12.5 || info.Rate != 1 {
		t.Fatalf("unexpected PlaybackInfo: %+v", info)
	}

	if err := sess.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-done

	// Verify exact wire quirks required by the AIMIBO firmware.
	for _, mustContain := range []string{
		"POST /play HTTP/1.1",
		"Content-Type: text/parameters",
		"X-Apple-Session-ID: ",
		"Content-Location: http://10.52.0.4:8899/live.m3u8",
		"Start-Position: 0.000000",
	} {
		if !strings.Contains(gotPlayReq, mustContain) {
			t.Errorf("POST /play missing %q in:\n%s", mustContain, gotPlayReq)
		}
	}
	if !strings.Contains(gotInfoReq, "GET /playback-info HTTP/1.1") {
		t.Errorf("expected GET /playback-info, got:\n%s", gotInfoReq)
	}
}

func TestMDNSQueryAndMatch(t *testing.T) {
	pkt := buildMDNSPTRQuery("_airplay._tcp.local")
	if len(pkt) < 16 {
		t.Fatalf("query too short: %d bytes", len(pkt))
	}
	// Fake responder payload containing "Office Tv" surrounded by binary DNS bytes.
	fakeResp := append([]byte{0x00, 0x09}, []byte("Office Tv")...)
	fakeResp = append(fakeResp, 0x00, 0x01)
	if !packetMentionsName(fakeResp, "office tv") {
		t.Fatalf("expected packetMentionsName to match Office Tv")
	}
	if packetMentionsName(fakeResp, "living room") {
		t.Fatalf("expected packetMentionsName not to match unrelated name")
	}
}

func TestHLSServerAndWatchdog(t *testing.T) {
	dir := t.TempDir()
	playlist := "#EXTM3U\n#EXTINF:4.0,\nseg_000001.ts\n#EXTINF:4.0,\nseg_000002.ts\n#EXTINF:4.0,\nseg_000003.ts\n"
	if err := os.WriteFile(filepath.Join(dir, "live.m3u8"), []byte(playlist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seg_000001.ts"), []byte("fake-ts-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newHLSServer(dir, ":0", log.New(io.Discard, "", 0))
	if !srv.hasReadyPlaylist(3) {
		t.Fatalf("expected playlist with 3 segments to be ready")
	}
	if srv.hasReadyPlaylist(4) {
		t.Fatalf("expected playlist with 3 segments not to satisfy min=4")
	}

	// Fetch playlist -> should set no-cache and NOT bump segment counter.
	rec := httptest.NewRecorder()
	srv.handleFile(rec, httptest.NewRequest(http.MethodGet, "/live.m3u8", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /live.m3u8 = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-cache") {
		t.Fatalf("expected no-cache header on playlist, got %q", rec.Header().Get("Cache-Control"))
	}
	if srv.totalSegs.Load() != 0 {
		t.Fatalf("playlist fetch should not count as segment fetch")
	}

	// Fetch .ts segment -> should bump counter & watchdog timestamp.
	rec2 := httptest.NewRecorder()
	srv.handleFile(rec2, httptest.NewRequest(http.MethodGet, "/seg_000001.ts", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /seg_000001.ts = %d, want 200", rec2.Code)
	}
	if srv.totalSegs.Load() != 1 || srv.lastSegNS.Load() == 0 {
		t.Fatalf("expected segment fetch to update watchdog metrics")
	}

	// Non-HLS file must be rejected.
	rec3 := httptest.NewRecorder()
	srv.handleFile(rec3, httptest.NewRequest(http.MethodGet, "/etc/passwd", nil))
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-HLS path, got %d", rec3.Code)
	}
}

func readRawHTTP(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	var hdr strings.Builder
	contentLen := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("readRawHTTP: %v", err)
		}
		hdr.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if k, v, ok := strings.Cut(trimmed, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
			for _, ch := range strings.TrimSpace(v) {
				contentLen = contentLen*10 + int(ch-'0')
			}
		}
	}
	if contentLen == 0 {
		return hdr.String(), ""
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("readRawHTTP body: %v", err)
	}
	return hdr.String(), string(buf)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

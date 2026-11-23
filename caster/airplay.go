package caster

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// AirPlaySession holds a persistent TCP connection to an AirPlay 1 receiver
// (such as the AIMIBO dongle on port 7000).
//
// Why raw TCP instead of Go's net/http.Client?
// Two real-world quirks we hit on the AIMIBO (EZAir1,1 / AppleTV3,2 firmware):
//  1. Go's net/http canonicalizes header names ("X-Apple-Session-ID" becomes
//     "X-Apple-Session-Id"). Older embedded AirPlay C parsers do case-sensitive
//     header matching, so writing raw HTTP/1.1 lines preserves exact casing.
//  2. AirPlay 1 ties playback lifetime to the same persistent TCP connection
//     and X-Apple-Session-ID header; holding a single net.Conn makes keep-alive
//     polling via GET /playback-info completely predictable.
type AirPlaySession struct {
	addr      string
	sessionID string
	conn      net.Conn
	reader    *bufio.Reader
}

// PlaybackInfo is the small subset of fields we care about from the XML plist
// returned by GET /playback-info.
type PlaybackInfo struct {
	ReadyToPlay bool
	Rate        float64
	Position    float64
	Duration    float64
	RawXML      string
}

// DialAirPlay opens a TCP connection to host:port and generates a fresh
// X-Apple-Session-ID UUID for this casting session.
func DialAirPlay(host string, port int, timeout time.Duration) (*AirPlaySession, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial airplay %s: %w", addr, err)
	}
	return &AirPlaySession{
		addr:      addr,
		sessionID: newUUIDv4(),
		conn:      conn,
		reader:    bufio.NewReader(conn),
	}, nil
}

// Play tells the AirPlay 1 receiver to start playing mediaURL (for us,
// http://<server-ip>:8899/live.m3u8).
//
// Protocol quirks verified against the AIMIBO (VER.20321145 / EZAir1,1):
//   - Content-Type MUST be "text/parameters" (XML/binary plists return 400 or wedge).
//   - Start-Position MUST be formatted as a float ("0.000000"); integer "0" returns 400.
//   - User-Agent mimics iTunes/10.6 (standard AirPlay 1 video sender).
//   - No pairing or FairPlay encryption is required for /play (unlike port 7100 mirroring).
func (s *AirPlaySession) Play(mediaURL string) error {
	body := fmt.Sprintf("Content-Location: %s\nStart-Position: 0.000000\n", mediaURL)
	req := fmt.Sprintf(
		"POST /play HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"User-Agent: iTunes/10.6 (Macintosh; Intel Mac OS X 10.7.3) AppleWebKit/535.18.5\r\n"+
			"Content-Type: text/parameters\r\n"+
			"X-Apple-Session-ID: %s\r\n"+
			"Content-Length: %d\r\n"+
			"\r\n%s",
		s.addr, s.sessionID, len(body), body,
	)

	// Give the dongle up to 15s to fetch the initial .m3u8 + first segments
	// before it replies "200 OK" to POST /play.
	status, _, err := s.roundTrip(req, 15*time.Second)
	if err != nil {
		return fmt.Errorf("airplay POST /play: %w", err)
	}
	if status != 200 {
		return fmt.Errorf("airplay POST /play returned HTTP %d", status)
	}
	return nil
}

// PlaybackInfo queries GET /playback-info on the open AirPlay connection.
// This doubles as a session heartbeat and lets us check if the receiver is
// still alive and playing.
func (s *AirPlaySession) PlaybackInfo() (PlaybackInfo, error) {
	req := fmt.Sprintf(
		"GET /playback-info HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"User-Agent: MediaControl/1.0\r\n"+
			"X-Apple-Session-ID: %s\r\n"+
			"\r\n",
		s.addr, s.sessionID,
	)

	status, body, err := s.roundTrip(req, 5*time.Second)
	if err != nil {
		return PlaybackInfo{}, fmt.Errorf("airplay GET /playback-info: %w", err)
	}
	if status != 200 {
		return PlaybackInfo{}, fmt.Errorf("airplay GET /playback-info returned HTTP %d", status)
	}

	return parsePlaybackInfoPlist(body), nil
}

// Stop sends POST /stop (best-effort) and closes the underlying TCP socket.
func (s *AirPlaySession) Stop() error {
	if s.conn == nil {
		return nil
	}
	req := fmt.Sprintf(
		"POST /stop HTTP/1.1\r\n"+
			"Host: %s\r\n"+
			"User-Agent: MediaControl/1.0\r\n"+
			"X-Apple-Session-ID: %s\r\n"+
			"Content-Length: 0\r\n"+
			"\r\n",
		s.addr, s.sessionID,
	)
	_, _, _ = s.roundTrip(req, 2*time.Second)
	err := s.conn.Close()
	s.conn = nil
	return err
}

// Close closes the TCP socket without sending POST /stop.
func (s *AirPlaySession) Close() error {
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// roundTrip writes a raw HTTP/1.1 request and reads back the status code and
// full body (based on Content-Length).
func (s *AirPlaySession) roundTrip(rawReq string, timeout time.Duration) (int, string, error) {
	if err := s.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return 0, "", err
	}
	if _, err := io.WriteString(s.conn, rawReq); err != nil {
		return 0, "", err
	}

	// Read status line: e.g. "HTTP/1.1 200 OK\r\n"
	statusLine, err := s.reader.ReadString('\n')
	if err != nil {
		return 0, "", fmt.Errorf("read status line: %w", err)
	}
	parts := strings.Fields(statusLine)
	if len(parts) < 2 {
		return 0, "", fmt.Errorf("malformed status line: %q", statusLine)
	}
	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", fmt.Errorf("parse status code %q: %w", parts[1], err)
	}

	// Read response headers until the blank "\r\n" line.
	contentLength := 0
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			return 0, "", fmt.Errorf("read header: %w", err)
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		if k, v, ok := strings.Cut(trimmed, ":"); ok {
			if strings.EqualFold(strings.TrimSpace(k), "Content-Length") {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					contentLength = n
				}
			}
		}
	}

	// Read the exact body bytes if Content-Length was provided.
	var body string
	if contentLength > 0 {
		buf := make([]byte, contentLength)
		if _, err := io.ReadFull(s.reader, buf); err != nil {
			return statusCode, "", fmt.Errorf("read body (%d bytes): %w", contentLength, err)
		}
		body = string(buf)
	}
	return statusCode, body, nil
}

// parsePlaybackInfoPlist extracts key fields from Apple's simple flat XML plist
// format without pulling in an external plist library.
func parsePlaybackInfoPlist(xmlBody string) PlaybackInfo {
	info := PlaybackInfo{RawXML: xmlBody}
	lines := strings.Split(xmlBody, "\n")
	for i := 0; i < len(lines)-1; i++ {
		line := strings.TrimSpace(lines[i])
		next := strings.TrimSpace(lines[i+1])
		switch line {
		case "<key>readyToPlay</key>":
			info.ReadyToPlay = strings.Contains(next, "<true")
		case "<key>rate</key>":
			info.Rate = parseXMLFloat(next)
		case "<key>position</key>":
			info.Position = parseXMLFloat(next)
		case "<key>duration</key>":
			info.Duration = parseXMLFloat(next)
		}
	}
	return info
}

func parseXMLFloat(tag string) float64 {
	// Strip outer "<real>...</real>" or "<integer>...</integer>" tags.
	start := strings.IndexByte(tag, '>')
	end := strings.LastIndexByte(tag, '<')
	if start == -1 || end <= start {
		return 0
	}
	v, _ := strconv.ParseFloat(strings.TrimSpace(tag[start+1:end]), 64)
	return v
}

// newUUIDv4 generates a random RFC 4122 v4 UUID string using crypto/rand.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Package caster renders a webpage headlessly (without a window manager)
// and streams it to an AirPlay 1 receiver (like an AIMIBO wireless HDMI dongle)
// over a live sliding-window HLS stream.
//
// High-level data flow:
//
//	TargetURL -> Chromium (--kiosk inside Xvfb :99)
//	          -> ffmpeg (x11grab -> H.264 Baseline HLS segments in tmpfs)
//	          -> local HTTP server (:8899/live.m3u8)
//	          -> AirPlay 1 POST /play (device:7000) -> TV over HDMI
//
// Nothing in this package depends on third-party Go modules; it uses only
// the Go standard library plus three system binaries (Xvfb, chromium, ffmpeg).
package caster

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Config controls every part of the casting pipeline. Any zero-valued field
// falls back to a sensible default in withDefaults().
type Config struct {
	// TargetURL is the webpage Chromium will open fullscreen (e.g. "http://127.0.0.1:8090/").
	TargetURL string

	// DeviceHost is the AIMIBO dongle's LAN IP (e.g. "10.52.0.18").
	// If empty or unreachable on port 7000, the caster falls back to mDNS
	// discovery using DeviceName.
	DeviceHost string

	// DevicePort is the AirPlay 1 HTTP port on the dongle (almost always 7000).
	DevicePort int

	// DeviceName is the mDNS AirPlay service name (e.g. "Office Tv") used
	// to rediscover the dongle if its DHCP IP ever changes after a reboot.
	DeviceName string

	// StreamHost is the LAN IP of THIS server that the AIMIBO will connect
	// back to in order to fetch live.m3u8 and .ts segments (e.g. "10.52.0.4").
	// If empty, we auto-detect the outbound LAN IP used to reach DeviceHost.
	StreamHost string

	// HTTPAddr is the bind address for the local HLS HTTP server (default ":8899").
	HTTPAddr string

	// HLSDir is the directory where ffmpeg writes live.m3u8 and segment .ts files.
	// Point this at a tmpfs mount (like /tmp/hls) so 4-second segment writes
	// stay in RAM instead of wearing out an SSD.
	HLSDir string

	// Virtual display geometry and encoding parameters.
	Display    string // X11 display number for Xvfb, e.g. ":99"
	Width      int    // Framebuffer width in pixels (default 1920)
	Height     int    // Framebuffer height in pixels (default 1080)
	FPS        int    // Capture/encode framerate (default 2; plenty for a dashboard)
	SegmentSec int    // Seconds per HLS .ts segment (default 4)
	ListSize   int    // Number of segments kept in the sliding playlist (default 5)

	// Binary overrides in case chromium/ffmpeg/Xvfb/pulseaudio live under
	// different names (e.g. "chromium-browser" vs "chromium").
	XvfbBin     string
	ChromiumBin string
	FFmpegBin   string
	PulseBin    string

	// Audio capture (routes browser <audio> / Web Audio / TTS out through the TV speakers).
	// When EnableAudio is true, the pipeline runs a headless PulseAudio null sink
	// (no physical sound card needed) and muxes its monitor into the HLS stream as AAC.
	EnableAudio  bool
	PulseSocket  string // UNIX socket path for our private PulseAudio daemon (default "/tmp/pulse/native")
	AudioBitrate string // AAC bitrate for ffmpeg (default "128k")

	// Health / reconnect tuning.
	PollInterval time.Duration // How often to poll GET /playback-info (default 10s)
	StallTimeout time.Duration // Reconnect if AIMIBO stops fetching .ts segments for this long (default 30s)
}

// withDefaults returns a copy of cfg with sensible defaults filled in.
func (c Config) withDefaults() Config {
	if c.DevicePort == 0 {
		c.DevicePort = 7000
	}
	if c.DeviceName == "" {
		c.DeviceName = "Office Tv"
	}
	if c.HTTPAddr == "" {
		c.HTTPAddr = ":8899"
	}
	if c.HLSDir == "" {
		c.HLSDir = "/tmp/hls"
	}
	if c.Display == "" {
		c.Display = ":99"
	}
	if c.Width == 0 {
		c.Width = 1920
	}
	if c.Height == 0 {
		c.Height = 1080
	}
	if c.FPS == 0 {
		c.FPS = 2
	}
	if c.SegmentSec == 0 {
		c.SegmentSec = 4
	}
	if c.ListSize == 0 {
		c.ListSize = 5
	}
	if c.XvfbBin == "" {
		c.XvfbBin = "Xvfb"
	}
	if c.ChromiumBin == "" {
		c.ChromiumBin = "chromium"
	}
	if c.FFmpegBin == "" {
		c.FFmpegBin = "ffmpeg"
	}
	if c.PulseBin == "" {
		c.PulseBin = "pulseaudio"
	}
	if c.PulseSocket == "" {
		c.PulseSocket = "/tmp/pulse/native"
	}
	if c.AudioBitrate == "" {
		c.AudioBitrate = "128k"
	}
	if c.PollInterval == 0 {
		c.PollInterval = 10 * time.Second
	}
	if c.StallTimeout == 0 {
		c.StallTimeout = 30 * time.Second
	}
	return c
}

// validate checks that required fields are sane before launching anything.
func (c Config) validate() error {
	if strings.TrimSpace(c.TargetURL) == "" {
		return fmt.Errorf("caster: TargetURL is required")
	}
	if c.DeviceHost == "" && c.DeviceName == "" {
		return fmt.Errorf("caster: either DeviceHost or DeviceName must be set")
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return fmt.Errorf("caster: invalid HTTPAddr %q: %w", c.HTTPAddr, err)
	}
	return nil
}

// httpPort extracts just the numeric port string from HTTPAddr (e.g. ":8899" -> "8899").
func (c Config) httpPort() string {
	_, port, err := net.SplitHostPort(c.HTTPAddr)
	if err != nil {
		return "8899"
	}
	return port
}

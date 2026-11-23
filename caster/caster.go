package caster

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"
)

// Caster is the top-level entry point that ties together:
//   - the local HTTP HLS server (hlsServer)
//   - the headless browser + ffmpeg encoder (pipeline)
//   - the AirPlay 1 keeper loop with automatic discovery and reconnect
//
// Typical usage from your own Go dashboard:
//
//	c := caster.New(caster.Config{
//	    TargetURL:  "http://127.0.0.1:8090/",
//	    DeviceHost: "10.52.0.18",
//	    DeviceName: "Office Tv",
//	    StreamHost: "10.52.0.4",
//	})
//	log.Fatal(c.Run(ctx))
type Caster struct {
	cfg    Config
	logger *log.Logger
}

// New constructs a Caster with defaults applied.
func New(cfg Config) *Caster {
	return &Caster{
		cfg:    cfg.withDefaults(),
		logger: log.New(os.Stdout, "", log.LstdFlags),
	}
}

// SetLogger overrides the default stdout logger.
func (c *Caster) SetLogger(l *log.Logger) {
	if l != nil {
		c.logger = l
	}
}

// Run starts the HLS server, headless render pipeline, and AirPlay reconnect
// loop. It blocks until ctx is cancelled (e.g. SIGINT/SIGTERM).
func (c *Caster) Run(ctx context.Context) error {
	if err := c.cfg.validate(); err != nil {
		return err
	}

	// 1. Start serving /tmp/hls on HTTPAddr (:8899).
	hls := newHLSServer(c.cfg.HLSDir, c.cfg.HTTPAddr, c.logger)
	if err := hls.start(ctx); err != nil {
		return err
	}

	// 2. Start Xvfb + Chromium --kiosk + ffmpeg x11grab -> HLS.
	pipe := newPipeline(c.cfg, c.logger)
	if err := pipe.start(ctx); err != nil {
		return err
	}

	// 3. Wait until ffmpeg has written at least 3 segments so the AIMIBO gets
	//    an immediate healthy buffer when it fetches live.m3u8.
	c.logger.Printf("[caster] waiting for ffmpeg to produce initial HLS segments in %s...", c.cfg.HLSDir)
	for !hls.hasReadyPlaylist(3) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	c.logger.Printf("[caster] HLS playlist ready; entering AirPlay keeper loop")

	// 4. Keep the AIMIBO playing forever, reconnecting automatically if the TV
	//    is powered off/on, Wi-Fi blips, or the dongle's DHCP IP changes.
	c.keepAliveLoop(ctx, hls)
	return ctx.Err()
}

func (c *Caster) keepAliveLoop(ctx context.Context, hls *hlsServer) {
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		deviceIP, err := c.resolveDeviceIP()
		if err != nil {
			c.logger.Printf("[airplay] device not reachable (%v); retrying in %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		streamHost := c.cfg.StreamHost
		if streamHost == "" {
			if autoIP, err := detectOutboundIP(deviceIP); err == nil {
				streamHost = autoIP
			} else {
				c.logger.Printf("[airplay] could not detect StreamHost (%v); set STREAM_HOST env var", err)
				if !sleepCtx(ctx, backoff) {
					return
				}
				continue
			}
		}

		playlistURL := fmt.Sprintf("http://%s:%s/live.m3u8", streamHost, c.cfg.httpPort())
		c.logger.Printf("[airplay] connecting to %s:%d -> %s", deviceIP, c.cfg.DevicePort, playlistURL)

		session, err := DialAirPlay(deviceIP, c.cfg.DevicePort, 5*time.Second)
		if err != nil {
			c.logger.Printf("[airplay] dial failed: %v; retrying in %s", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		hls.resetWatchdog()
		if err := session.Play(playlistURL); err != nil {
			c.logger.Printf("[airplay] POST /play failed: %v; retrying in %s", err, backoff)
			_ = session.Close()
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}

		// Connected and playing! Reset backoff and monitor until disconnect/stall.
		backoff = 2 * time.Second
		c.logger.Printf("[airplay] streaming live to %s", deviceIP)
		c.monitorSession(ctx, session, hls)

		// If the context is shutting down cleanly, send POST /stop; otherwise just close.
		if ctx.Err() != nil {
			_ = session.Stop()
			return
		}
		_ = session.Close()
		c.logger.Printf("[airplay] session ended; reconnecting in 3s...")
		if !sleepCtx(ctx, 3*time.Second) {
			return
		}
	}
}

// monitorSession polls GET /playback-info and checks the HLS segment-fetch
// watchdog. It returns as soon as either signal indicates the TV stopped playing.
func (c *Caster) monitorSession(ctx context.Context, session *AirPlaySession, hls *hlsServer) {
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check 1: is the AirPlay control socket still responsive?
			if _, err := session.PlaybackInfo(); err != nil {
				c.logger.Printf("[airplay] /playback-info failed: %v", err)
				return
			}

			// Check 2: has the AIMIBO fetched a .ts segment recently?
			// If the TV was turned off (cutting USB power to the dongle) or
			// playback froze, this catches it within StallTimeout.
			since := hls.timeSinceLastSegment()
			if since > c.cfg.StallTimeout {
				c.logger.Printf("[airplay] no HLS segment fetched for %s (limit %s); forcing reconnect",
					since.Round(time.Second), c.cfg.StallTimeout)
				return
			}
		}
	}
}

// resolveDeviceIP tries the configured DeviceHost first (fast TCP probe on
// DevicePort). If it's unset or unreachable, it falls back to mDNS discovery
// for DeviceName so DHCP IP changes heal automatically.
func (c *Caster) resolveDeviceIP() (string, error) {
	if c.cfg.DeviceHost != "" {
		addr := net.JoinHostPort(c.cfg.DeviceHost, strconv.Itoa(c.cfg.DevicePort))
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return c.cfg.DeviceHost, nil
		}
		c.logger.Printf("[airplay] configured host %s unreachable (%v); trying mDNS for %q",
			addr, err, c.cfg.DeviceName)
	}

	if c.cfg.DeviceName != "" {
		ip, err := DiscoverAirPlayIP(c.cfg.DeviceName, 3*time.Second)
		if err == nil {
			c.logger.Printf("[airplay] mDNS discovered %q at %s", c.cfg.DeviceName, ip)
			return ip, nil
		}
		return "", err
	}
	return "", fmt.Errorf("device %s unreachable", c.cfg.DeviceHost)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

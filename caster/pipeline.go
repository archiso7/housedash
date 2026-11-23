package caster

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// pipeline supervises the three OS processes that turn a webpage into live HLS
// segments with zero window manager:
//
//  1. Xvfb     — creates an in-memory 1920x1080 virtual X11 display (:99)
//  2. Chromium — renders TargetURL fullscreen (--kiosk) onto :99
//  3. ffmpeg   — captures :99 via x11grab at low FPS and writes rolling 4s
//                H.264 Baseline .ts segments + live.m3u8 into HLSDir (tmpfs)
//
// Each child is supervised in its own loop so if Chromium or ffmpeg ever exits
// unexpectedly, it restarts automatically without killing the container.
type pipeline struct {
	cfg    Config
	logger *log.Logger
}

func newPipeline(cfg Config, logger *log.Logger) *pipeline {
	return &pipeline{cfg: cfg, logger: logger}
}

// start prepares HLSDir and starts Xvfb -> Chromium -> ffmpeg in order.
// All three processes stop automatically when ctx is cancelled.
func (p *pipeline) start(ctx context.Context) error {
	if err := os.MkdirAll(p.cfg.HLSDir, 0o755); err != nil {
		return fmt.Errorf("create HLS dir %s: %w", p.cfg.HLSDir, err)
	}
	// Clear any stale segments from a previous container run so the AIMIBO
	// never tries to play old timestamps.
	p.cleanHLSDir()

	// 1. Start Xvfb first and wait briefly for its UNIX socket (/tmp/.X11-unix/X99).
	go p.supervise(ctx, "xvfb", p.xvfbCmd)
	if err := p.waitForX11(ctx, 5*time.Second); err != nil {
		return err
	}

	// 2. Launch headless Chromium in kiosk mode pointed at TargetURL.
	go p.supervise(ctx, "chromium", p.chromiumCmd)

	// Give Chromium a moment to paint its first frame before ffmpeg starts
	// grabbing :99 so the very first HLS segment isn't a blank white window.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}

	// 3. Launch ffmpeg x11grab -> HLS sliding window.
	go p.supervise(ctx, "ffmpeg", p.ffmpegCmd)
	return nil
}

func (p *pipeline) xvfbCmd(ctx context.Context) *exec.Cmd {
	screen := fmt.Sprintf("%dx%dx24", p.cfg.Width, p.cfg.Height)
	// -nolisten tcp keeps the virtual X server purely local (UNIX socket only).
	return exec.CommandContext(ctx, p.cfg.XvfbBin,
		p.cfg.Display,
		"-screen", "0", screen,
		"-nolisten", "tcp",
		"-ac",
	)
}

func (p *pipeline) chromiumCmd(ctx context.Context) *exec.Cmd {
	// These flags run Chromium cleanly inside a headless container with no WM:
	//   --kiosk / --window-size / --window-position=0,0 : fill the entire 1920x1080 screen
	//   --force-device-scale-factor=1                   : exact 1:1 CSS pixels
	//   --disable-dev-shm-usage                         : avoid Docker's tiny 64MB /dev/shm crashes
	//   --disable-background-timer-throttling           : keep JS clocks/polls running smoothly in Xvfb
	//   --noerrdialogs / --disable-infobars             : never pop up "Chrome didn't shut down" banners
	// Note: do NOT pass --disable-software-rasterizer alongside --disable-gpu,
	// since headless Xvfb relies on Chromium's software compositor to paint frames.
	args := []string{
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--kiosk",
		"--hide-scrollbars",
		fmt.Sprintf("--window-size=%d,%d", p.cfg.Width, p.cfg.Height),
		"--window-position=0,0",
		"--force-device-scale-factor=1",
		"--no-first-run",
		"--no-default-browser-check",
		"--noerrdialogs",
		"--disable-infobars",
		"--disable-session-crashed-bubble",
		"--disable-background-timer-throttling",
		"--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows",
		"--autoplay-policy=no-user-gesture-required",
		"--user-data-dir=/tmp/chromium-profile",
		p.cfg.TargetURL,
	}
	cmd := exec.CommandContext(ctx, p.cfg.ChromiumBin, args...)
	cmd.Env = append(os.Environ(), "DISPLAY="+p.cfg.Display)
	return cmd
}

func (p *pipeline) ffmpegCmd(ctx context.Context) *exec.Cmd {
	// Key encoding choices verified against the AIMIBO hardware decoder:
	//   -framerate 2                : dashboards barely move; 2fps uses near-zero CPU
	//   -profile:v baseline         : universally decoded by cheap HDMI dongles (no B-frames)
	//   -pix_fmt yuv420p            : required by hardware H.264 decoders
	//   -g (FPS * SegmentSec)       : force a keyframe cleanly at every HLS segment boundary
	//   -hls_flags delete_segments  : keep RAM usage flat in tmpfs
	//   +temp_file                  : atomic rename of live.m3u8 so AIMIBO never reads a half-written file
	gop := p.cfg.FPS * p.cfg.SegmentSec
	playlistPath := filepath.Join(p.cfg.HLSDir, "live.m3u8")
	segmentPattern := filepath.Join(p.cfg.HLSDir, "seg_%06d.ts")

	args := []string{
		"-loglevel", "warning",
		"-f", "x11grab",
		"-draw_mouse", "0",
		"-framerate", strconv.Itoa(p.cfg.FPS),
		"-video_size", fmt.Sprintf("%dx%d", p.cfg.Width, p.cfg.Height),
		"-i", p.cfg.Display,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "stillimage",
		"-profile:v", "baseline",
		"-level", "4.0",
		"-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gop),
		"-keyint_min", strconv.Itoa(gop),
		"-sc_threshold", "0",
		"-f", "hls",
		"-hls_time", strconv.Itoa(p.cfg.SegmentSec),
		"-hls_list_size", strconv.Itoa(p.cfg.ListSize),
		"-hls_flags", "delete_segments+independent_segments+temp_file",
		"-hls_segment_filename", segmentPattern,
		playlistPath,
	}
	return exec.CommandContext(ctx, p.cfg.FFmpegBin, args...)
}

// supervise runs a child command and restarts it with a short pause if it exits
// while ctx is still active.
func (p *pipeline) supervise(ctx context.Context, name string, build func(context.Context) *exec.Cmd) {
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := build(ctx)
		// Forward stderr so container logs show real ffmpeg/chromium diagnostics.
		cmd.Stderr = os.Stderr
		p.logger.Printf("[%s] starting: %s", name, cmd.Path)

		err := cmd.Run()
		if ctx.Err() != nil {
			return
		}
		p.logger.Printf("[%s] exited (%v); restarting in 3s...", name, err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (p *pipeline) waitForX11(ctx context.Context, timeout time.Duration) error {
	dispNum := strings.TrimPrefix(p.cfg.Display, ":")
	sockPath := filepath.Join("/tmp/.X11-unix", "X"+dispNum)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			p.logger.Printf("[xvfb] display %s is ready (%s)", p.cfg.Display, sockPath)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("xvfb: timed out waiting for %s", sockPath)
}

func (p *pipeline) cleanHLSDir() {
	entries, err := os.ReadDir(p.cfg.HLSDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".m3u8") || strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(p.cfg.HLSDir, name))
		}
	}
}

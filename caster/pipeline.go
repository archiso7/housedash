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

const pulseSinkName = "tv_sink"

// pipeline supervises the OS processes that turn a webpage into a live HLS
// audio+video stream with zero window manager and no physical sound card:
//
//  1. PulseAudio (optional, when EnableAudio=true) — creates an in-memory virtual
//     speaker ("tv_sink") and its loopback capture source ("tv_sink.monitor").
//  2. Xvfb     — creates an in-memory 1920x1080 virtual X11 display (:99)
//  3. Chromium — renders TargetURL fullscreen (--kiosk) onto :99 and sends all
//                browser audio (<audio>, Web Audio API, TTS) into tv_sink
//  4. ffmpeg   — captures :99 via x11grab (+ tv_sink.monitor via pulse) and
//                writes rolling 4s H.264 Baseline + AAC .ts segments into HLSDir
//
// Each child is supervised in its own loop so if Chromium, PulseAudio, or
// ffmpeg ever exits unexpectedly, it restarts automatically.
type pipeline struct {
	cfg    Config
	logger *log.Logger
}

func newPipeline(cfg Config, logger *log.Logger) *pipeline {
	return &pipeline{cfg: cfg, logger: logger}
}

// start prepares HLSDir and launches PulseAudio (if enabled) -> Xvfb ->
// Chromium -> ffmpeg in order. All children stop when ctx is cancelled.
func (p *pipeline) start(ctx context.Context) error {
	if err := os.MkdirAll(p.cfg.HLSDir, 0o755); err != nil {
		return fmt.Errorf("create HLS dir %s: %w", p.cfg.HLSDir, err)
	}
	// Clear any stale segments from a previous container run so the AIMIBO
	// never tries to play old timestamps.
	p.cleanHLSDir()

	// 1. If audio is enabled, start our private headless PulseAudio daemon first
	//    and wait for its UNIX socket so both Chromium and ffmpeg can attach.
	if p.cfg.EnableAudio {
		pulseDir := filepath.Dir(p.cfg.PulseSocket)
		if err := os.MkdirAll(pulseDir, 0o700); err != nil {
			return fmt.Errorf("create pulse dir %s: %w", pulseDir, err)
		}
		_ = os.Remove(p.cfg.PulseSocket)
		go p.supervise(ctx, "pulseaudio", p.pulseCmd)
		if err := p.waitForSocket(ctx, "pulseaudio", p.cfg.PulseSocket, 5*time.Second); err != nil {
			return err
		}
	}

	// 2. Start Xvfb and wait for its UNIX socket (/tmp/.X11-unix/X99).
	go p.supervise(ctx, "xvfb", p.xvfbCmd)
	dispNum := strings.TrimPrefix(p.cfg.Display, ":")
	x11Sock := filepath.Join("/tmp/.X11-unix", "X"+dispNum)
	if err := p.waitForSocket(ctx, "xvfb", x11Sock, 5*time.Second); err != nil {
		return err
	}

	// 3. Launch headless Chromium in kiosk mode pointed at TargetURL.
	go p.supervise(ctx, "chromium", p.chromiumCmd)

	// Give Chromium a moment to paint its first frame before ffmpeg starts
	// grabbing :99 so the very first HLS segment isn't a blank white window.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}

	// 4. Launch ffmpeg x11grab (+ optional pulse audio) -> HLS sliding window.
	go p.supervise(ctx, "ffmpeg", p.ffmpegCmd)
	return nil
}

// pulseCmd starts a minimal, self-contained PulseAudio daemon with NO hardware
// probing (-n skips /etc/pulse/default.pa). Instead it loads only:
//   - module-null-sink: a fake speaker in RAM (tv_sink) + capture monitor (tv_sink.monitor)
//   - module-native-protocol-unix: local UNIX socket that Chromium & ffmpeg connect to
//
// Passing --disable-shm=yes avoids Docker's small /dev/shm crashing PulseAudio.
func (p *pipeline) pulseCmd(ctx context.Context) *exec.Cmd {
	args := []string{
		"--daemonize=no",
		"--exit-idle-time=-1",
		"--disallow-exit",
		"--disable-shm=yes",
		"--log-target=stderr",
		"-n",
		"-L", fmt.Sprintf("module-null-sink sink_name=%s sink_properties=device.description=TV", pulseSinkName),
		"-L", fmt.Sprintf("module-native-protocol-unix auth-anonymous=1 socket=%s", p.cfg.PulseSocket),
	}
	cmd := exec.CommandContext(ctx, p.cfg.PulseBin, args...)
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+filepath.Dir(p.cfg.PulseSocket))
	return cmd
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
	//   --autoplay-policy=no-user-gesture-required      : allow <audio> / Web Audio without clicking the TV
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
	env := append(os.Environ(), "DISPLAY="+p.cfg.Display)
	if p.cfg.EnableAudio {
		// Point Chromium's PulseAudio client directly at our virtual null sink.
		env = append(env,
			"PULSE_SERVER=unix:"+p.cfg.PulseSocket,
			"PULSE_SINK="+pulseSinkName,
		)
	} else {
		args = append([]string{"--mute-audio"}, args...)
		cmd = exec.CommandContext(ctx, p.cfg.ChromiumBin, args...)
	}
	cmd.Env = env
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
		// Video input: virtual X11 display (:99)
		"-thread_queue_size", "512",
		"-f", "x11grab",
		"-draw_mouse", "0",
		"-framerate", strconv.Itoa(p.cfg.FPS),
		"-video_size", fmt.Sprintf("%dx%d", p.cfg.Width, p.cfg.Height),
		"-i", p.cfg.Display,
	}

	if p.cfg.EnableAudio {
		// Audio input: loopback monitor of our virtual PulseAudio null sink.
		// Even when Chromium is silent, tv_sink.monitor yields clean digital
		// silence so HLS segments always carry continuous, valid AAC audio.
		args = append(args,
			"-thread_queue_size", "512",
			"-f", "pulse",
			"-ac", "2",
			"-ar", "44100",
			"-i", pulseSinkName+".monitor",
		)
	}

	// Video encoder settings
	args = append(args,
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "stillimage",
		"-profile:v", "baseline",
		"-level", "4.0",
		"-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gop),
		"-keyint_min", strconv.Itoa(gop),
		"-sc_threshold", "0",
	)

	if p.cfg.EnableAudio {
		// AAC stereo 44.1kHz is universally supported by AirPlay 1 / HDMI TVs.
		// aresample=async=1 keeps audio timestamps locked smoothly to video.
		args = append(args,
			"-af", "aresample=async=1:first_pts=0",
			"-c:a", "aac",
			"-b:a", p.cfg.AudioBitrate,
			"-ar", "44100",
			"-ac", "2",
		)
	}

	// HLS sliding-window muxer settings
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(p.cfg.SegmentSec),
		"-hls_list_size", strconv.Itoa(p.cfg.ListSize),
		"-hls_flags", "delete_segments+independent_segments+temp_file",
		"-hls_segment_filename", segmentPattern,
		playlistPath,
	)

	cmd := exec.CommandContext(ctx, p.cfg.FFmpegBin, args...)
	if p.cfg.EnableAudio {
		cmd.Env = append(os.Environ(), "PULSE_SERVER=unix:"+p.cfg.PulseSocket)
	}
	return cmd
}

// supervise runs a child command and restarts it with a short pause if it exits
// while ctx is still active.
func (p *pipeline) supervise(ctx context.Context, name string, build func(context.Context) *exec.Cmd) {
	for {
		if ctx.Err() != nil {
			return
		}
		cmd := build(ctx)
		// Forward stderr so container logs show real pulseaudio/ffmpeg/chromium diagnostics.
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

func (p *pipeline) waitForSocket(ctx context.Context, name, sockPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			p.logger.Printf("[%s] socket ready (%s)", name, sockPath)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s: timed out waiting for socket %s", name, sockPath)
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

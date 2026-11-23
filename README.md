# housedash

Headless webpage-to-TV casting module in pure Go (zero external Go dependencies), built for driving an **AIMIBO wireless HDMI dongle** (AirPlay 1 / `EZAir1,1`) from a headless Linux server with **no window manager or desktop environment**.

Includes a runnable live-clock demo (`cmd/demo`) so you can verify the whole pipeline end-to-end on the TV, and an exported `caster` package (`github.com/archiso7/housedash/caster`) you can import directly once you build your house dashboard.

---

## How It Works

```text
Your Webpage (e.g. http://127.0.0.1:8090/)
  │
  ▼
Chromium (--kiosk, 1920x1080) inside Xvfb (:99) + PulseAudio null sink (tv_sink)
  │
  ▼
ffmpeg (x11grab @ 2fps + pulse tv_sink.monitor -> H.264 Baseline + AAC HLS in tmpfs)
  │
  ▼
Go HLS HTTP Server (:8899/live.m3u8 + watchdog tracking last .ts fetch)
  │
  ▼
Go AirPlay 1 Client (POST /play to AIMIBO:7000 + mDNS fallback + auto-reconnect)
  │
  ▼
AIMIBO Dongle (10.52.0.18) ──HDMI (video + audio)──▶ Panasonic TV
```

### Why HLS + AirPlay 1 `POST /play`?

We probed the AIMIBO dongle (`Office Tv`, `EZAir1,1` / `AppleTV3,2`, `srcvers=220.68`) across all its protocols:

- `PUT /photo` on port `7000`: returns `200 OK`, then silently sends `photo: stopped` after 20s because the firmware disables the AirPlay Photo feature bit (`features=0x0A7FEFF0`).
- Screen mirroring (`RTSP SETUP type=110` -> port `7100`): connects and clears the blue home screen, but expects FairPlay/AES-CTR encrypted H.264 frames.
- **`POST /play` on port `7000` with `Content-Type: text/parameters`**: completely unencrypted, needs no pairing, and seamlessly plays a live sliding-window HLS playlist (`live.m3u8` with no `#EXT-X-ENDLIST`) indefinitely.

Two AIMIBO quirks handled inside `caster/airplay.go`:
1. `Start-Position` must be formatted as a float (`0.000000`); integer `0` returns `400 Bad Request`.
2. We write raw HTTP/1.1 over `net.Conn` so `X-Apple-Session-ID` keeps exact header casing and stays on one persistent TCP connection for `GET /playback-info` heartbeats.

---

## Code Tour

Every file is intentionally small, stdlib-only, and heavily commented:

| Path | What it does |
|---|---|
| `caster/config.go` | `Config` struct, defaults (`1920x1080`, `2fps`, 4s HLS segments), validation |
| `caster/airplay.go` | Raw TCP AirPlay 1 client (`POST /play`, `GET /playback-info`, `POST /stop`) |
| `caster/mdns.go` | Pure-Go mDNS PTR query (`_airplay._tcp.local`) to rediscover `Office Tv` if its DHCP IP changes |
| `caster/hls_server.go` | Serves `live.m3u8` (no-cache) + `.ts` segments and records last-fetch time for stall detection |
| `caster/pipeline.go` | Supervises `Xvfb`, `chromium --kiosk`, and `ffmpeg` with automatic restart on crash |
| `caster/caster.go` | Top-level `Caster.Run(ctx)` tying everything together with exponential-backoff auto-reconnect |
| `caster/caster_test.go` | Unit tests covering the AirPlay wire format, mDNS matching, and HLS watchdog |
| `cmd/demo/main.go` | Runnable demo: serves `demo.html` (or any `TARGET_URL`) and casts it to the TV |
| `cmd/demo/demo.html` | Plasma-safe dark 1080p demo clock with subtle pixel-shift and optional 10pm–6am blackout (`?night=auto`) |

---

## Using `caster` in Your Own Dashboard

Once you write your dashboard in Go, you have two ways to wire it up:

### Option A: Import `caster` directly in your Go binary

```go
package main

import (
	"context"
	"log"

	"github.com/archiso7/housedash/caster"
)

func main() {
	// Start your dashboard HTTP server on 127.0.0.1:8090 in a goroutine...

	c := caster.New(caster.Config{
		TargetURL:   "http://127.0.0.1:8090/",
		DeviceHost:  "10.52.0.18",
		DeviceName:  "Office Tv",
		StreamHost:  "10.52.0.4",
		EnableAudio: true,
	})
	log.Fatal(c.Run(context.Background()))
}
```

### Option B: Run the caster as a separate container pointed at your dashboard

Set `TARGET_URL=http://127.0.0.1:8090/` (or `http://10.52.0.4:8090/`) in `.env` and run `docker compose up -d --build`.

---

## Audio Through the TV

With `EnableAudio: true` (the default in `compose.yml`), `caster/pipeline.go` starts a self-contained headless PulseAudio daemon with a virtual `tv_sink` speaker in RAM — no physical sound card required on the server. Chromium is launched with `--autoplay-policy=no-user-gesture-required` and `PULSE_SINK=tv_sink`, and `ffmpeg` captures `tv_sink.monitor` into AAC stereo alongside the video.

That means any normal browser `<audio>` element, Web Audio API synth/chime, or speech/TTS played by your dashboard page comes straight out of the TV speakers in sync with the video (note there is ~8–12s of HLS buffer latency).

To verify audio right away with the demo, set `DEMO_CHIME=once` (plays a soft chime ~12s after start) or `DEMO_CHIME=loop` (every 20s) in `.env`.

---

## Running & Testing on Your Server (`10.52.0.4`)

Because this repo is private, generate a read-only GitHub deploy key on `10.52.0.4` once, then every test cycle is just `git pull` + `docker compose up -d --build`:

```bash
# 1. One-time setup on 10.52.0.4: create a deploy key and clone
ssh liam@10.52.0.4
ssh-keygen -t ed25519 -f ~/.ssh/housedash_deploy -N ""
cat ~/.ssh/housedash_deploy.pub
# -> Add that public key in GitHub: archiso7/housedash -> Settings -> Deploy keys (read-only)

sudo mkdir -p /opt/housedash && sudo chown liam:liam /opt/housedash
GIT_SSH_COMMAND="ssh -i ~/.ssh/housedash_deploy" git clone git@github.com:archiso7/housedash.git /opt/housedash
cd /opt/housedash
git config core.sshCommand "ssh -i ~/.ssh/housedash_deploy"
cp .env.example .env

# 2. Build and start the demo (add DEMO_CHIME=loop in .env to test TV audio!)
docker compose up -d --build
docker compose logs -f

# 3. After pushing changes from your Mac or Linux laptop:
git pull && docker compose up -d --build

# 4. Stop casting:
docker compose down
```

`compose.yml` mounts `/tmp/hls` on `tmpfs` (RAM) so rolling 4-second video segments cause zero SSD wear, and uses `network_mode: host` so mDNS discovery and AirPlay LAN callbacks work cleanly.

### Plasma Burn-In Tip (10pm–6am)

Do **not** stop casting to the AIMIBO at night — the dongle immediately drops back to its bright blue static setup screen, which is worse for a plasma than keeping the stream running. Instead, have your dashboard page render pure `#000000` black from 22:00 to 06:00 while the HLS stream stays alive (see `cmd/demo/demo.html` for an example with `?night=auto`).

---

## Running Tests Locally

```bash
go test -v ./...
```

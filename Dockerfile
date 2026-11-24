# Stage 1: Build the zero-dependency Go binary
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY caster/ ./caster/
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -o /out/housedash-caster ./cmd/demo

# Stage 2: Minimal headless runtime (no desktop environment, no window manager)
# Installs Xvfb (virtual framebuffer), PulseAudio (virtual null speaker for TV
# audio), Chromium (kiosk renderer), ffmpeg (x11grab + pulse -> H.264/AAC HLS),
# and basic fonts so text renders crisply.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    chromium \
    ffmpeg \
    fonts-dejavu-core \
    fonts-liberation \
    pulseaudio \
    tzdata \
    xvfb \
    && rm -rf /var/lib/apt/lists/*

ENV TZ=America/Toronto

COPY --from=builder /out/housedash-caster /usr/local/bin/housedash-caster

ENTRYPOINT ["/usr/local/bin/housedash-caster"]

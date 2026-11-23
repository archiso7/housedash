# Stage 1: Build the zero-dependency Go binary
FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod ./
COPY caster/ ./caster/
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 go build -o /out/housedash-caster ./cmd/demo

# Stage 2: Minimal headless runtime (no desktop environment, no window manager)
# Only installs Xvfb (virtual framebuffer), Chromium (kiosk renderer), ffmpeg
# (x11grab -> H.264 HLS), and basic fonts so text renders crisply.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    chromium \
    ffmpeg \
    fonts-dejavu-core \
    fonts-liberation \
    tzdata \
    xvfb \
    && rm -rf /var/lib/apt/lists/*

ENV TZ=America/Toronto

COPY --from=builder /out/housedash-caster /usr/local/bin/housedash-caster

ENTRYPOINT ["/usr/local/bin/housedash-caster"]

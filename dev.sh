#!/usr/bin/env bash
# Helper script for running/testing Go packages in Docker on the server (10.52.0.4)
# with real LAN access (--network host) and raw ARP sockets (--cap-add NET_RAW).
#
# Run from /opt/housedash on the server after `git pull`:
#   ./dev.sh run ./cmd/whoshome
#   WATCH_MAC=aa:bb:cc:dd:ee:ff ./dev.sh run ./cmd/whoshome
#   ./dev.sh test ./whoshome/...
#   ./dev.sh deploy
#   ./dev.sh logs

set -euo pipefail

GO_IMAGE="${GO_IMAGE:-golang:1.25-bookworm}"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Env vars to forward into the Go container automatically when set.
FORWARD_VARS=(IFACE WATCH_MAC AIMIBO_HOST AIMIBO_PORT AIMIBO_NAME STREAM_HOST TARGET_URL ENABLE_AUDIO DEMO_CHIME)

usage() {
  cat <<'EOF'
Usage:
  ./dev.sh run <pkg-or-file> [args...]   Run `go run` in Docker with LAN + NET_RAW
  ./dev.sh test [pkg...]                 Run `go test -v` in Docker (default: ./...)
  ./dev.sh deploy                        Rebuild and restart docker compose
  ./dev.sh logs                          Follow docker compose logs

Optional env vars (forwarded automatically):
  IFACE, WATCH_MAC, GO_IMAGE
EOF
  exit 1
}

docker_go() {
  local tty_flag="-i"
  if [ -t 0 ] && [ -t 1 ]; then
    tty_flag="-it"
  fi

  local env_args=()
  for v in "${FORWARD_VARS[@]}"; do
    if [ -n "${!v:-}" ]; then
      env_args+=(-e "$v=${!v}")
    fi
  done

  exec docker run --rm "$tty_flag" \
    --network host \
    --cap-add NET_RAW \
    ${env_args[@]+"${env_args[@]}"} \
    -v "${REPO_DIR}:/src" \
    -v go-mod-cache:/go/pkg/mod \
    -w /src \
    "$GO_IMAGE" \
    go "$@"
}

cmd="${1:-}"
[ -z "$cmd" ] && usage
shift

case "$cmd" in
  run)
    [ $# -lt 1 ] && usage
    docker_go run "$@"
    ;;
  test)
    if [ $# -eq 0 ]; then
      docker_go test -v ./...
    else
      docker_go test -v "$@"
    fi
    ;;
  deploy)
    cd "$REPO_DIR"
    docker compose up -d --build
    ;;
  logs)
    cd "$REPO_DIR"
    docker compose logs -f --tail=50
    ;;
  *)
    usage
    ;;
esac

#!/usr/bin/env bash
# build.sh — build every mygrok artifact, and optionally ship it to your server.
#
# What it does, in order:
#   1. Cross-compile the four mygrok client variants
#        (darwin-arm64, darwin-amd64, linux-amd64, linux-arm64)
#   2. Stage them into cmd/mygrokd/embed/clients/ for //go:embed, so the
#      server can vend them from /dl and /install
#   3. Cross-compile mygrokd for the server's OS/arch
#   4. Install the native client locally (if the target dir is writable)
#   5. Rsync/scp the server binary to your box and restart the systemd unit
#
# Clients are stamped with MYGROK_HOST at link time, so a binary downloaded
# from your server already knows how to dial it — no config file needed on
# the machines that use it. A plain `go build` stamps nothing and requires
# --server / MYGROK_SERVER, which is the right default for a generic build.
#
# Flags (all optional):
#   --deploy         do step 5 (ship to MYGROK_HOST over SSH). Off by default:
#                    a plain ./build.sh builds and installs locally, nothing more.
#   --no-install     skip step 4 (don't touch the local bin dir)
#   --no-deploy      accepted for compatibility; deploy is already off by default
#   --skip-clients   skip rebuilding clients (useful when only mygrokd changed)
#
# Environment:
#   MYGROK_HOST      server hostname clients dial   (required for --deploy;
#                    also becomes the stamped default, e.g. tunnel.example.com)
#   MYGROK_PORT      tunnel control port            (default: 7000)
#   MYGROK_SSH_USER  ssh user on the server         (default: root)
#   MYGROK_SSH_KEY   ssh key path                   (default: ssh-agent/config)
#   MYGROK_SERVER_OS server GOOS/GOARCH             (default: linux/amd64)
#   MYGROK_BIN_DIR   local install dir              (default: first writable of
#                    /opt/homebrew/bin, /usr/local/bin, $HOME/.local/bin)
#   MYGROK_VERSION   version string                 (default: git describe)

set -euo pipefail

# --- args / defaults ---------------------------------------------------------

DO_INSTALL=1
DO_DEPLOY=0
DO_CLIENTS=1
for arg in "$@"; do
  case "$arg" in
    --no-install)   DO_INSTALL=0 ;;
    --deploy)       DO_DEPLOY=1 ;;
    --no-deploy)    DO_DEPLOY=0 ;;  # accepted for compatibility; deploy is opt-in now
    --skip-clients) DO_CLIENTS=0 ;;
    -h|--help)
      sed -n '2,/^$/p' "$0" | sed -e 's/^#$//' -e 's/^# //'
      exit 0
      ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

HOST="${MYGROK_HOST:-}"
PORT="${MYGROK_PORT:-7000}"
SSH_USER="${MYGROK_SSH_USER:-root}"
SSH_KEY="${MYGROK_SSH_KEY:-}"
SERVER_OS="${MYGROK_SERVER_OS:-linux/amd64}"
SERVER_GOOS="${SERVER_OS%/*}"
SERVER_GOARCH="${SERVER_OS#*/}"
VERSION="${MYGROK_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"

PKG=github.com/schappim/mygrok/internal/buildinfo
LDFLAGS="-s -w -X ${PKG}.Version=${VERSION}"
if [ -n "$HOST" ]; then
  LDFLAGS="$LDFLAGS -X ${PKG}.DefaultServer=${HOST}:${PORT}"
fi

# --- helpers -----------------------------------------------------------------

step() { printf "\n\033[1;36m==> %s\033[0m\n" "$*"; }
ok()   { printf "    \033[1;32m✓\033[0m %s\n" "$*"; }
warn() { printf "    \033[1;33m!\033[0m %s\n" "$*"; }
die()  { printf "    \033[1;31m✗\033[0m %s\n" "$*" >&2; exit 1; }

ssh_args() {
  printf '%s' "-o StrictHostKeyChecking=accept-new"
  [ -n "$SSH_KEY" ] && printf ' %s' "-i $SSH_KEY"
}

# pick_bin_dir returns the first writable directory on PATH we'd be happy
# to install into. Keeps `./build.sh` from needing sudo on a dev machine.
pick_bin_dir() {
  if [ -n "${MYGROK_BIN_DIR:-}" ]; then printf '%s' "$MYGROK_BIN_DIR"; return; fi
  for d in /opt/homebrew/bin /usr/local/bin "$HOME/.local/bin"; do
    [ -d "$d" ] && [ -w "$d" ] && { printf '%s' "$d"; return; }
  done
  printf '%s' "/usr/local/bin"
}
BIN_DIR="$(pick_bin_dir)"

step "Build settings"
echo "    version:  $VERSION"
echo "    server:   ${HOST:-(none — clients will require --server)}${HOST:+:$PORT}"
echo "    target:   $SERVER_GOOS/$SERVER_GOARCH"
echo "    bin dir:  $BIN_DIR"

# --- step 1: client cross-compile -------------------------------------------

if [ "$DO_CLIENTS" = "1" ]; then
  step "Cross-compiling clients"
  mkdir -p dist
  for tgt in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
    CGO_ENABLED=0 GOOS="${tgt%-*}" GOARCH="${tgt#*-}" \
      go build -trimpath -ldflags "$LDFLAGS" -o "dist/mygrok-$tgt" ./cmd/mygrok
    ok "dist/mygrok-$tgt"
  done

  step "Staging clients for //go:embed"
  mkdir -p cmd/mygrokd/embed/clients
  cp dist/mygrok-darwin-arm64 \
     dist/mygrok-darwin-amd64 \
     dist/mygrok-linux-amd64 \
     dist/mygrok-linux-arm64 \
     cmd/mygrokd/embed/clients/
  ok "embedded $(find cmd/mygrokd/embed/clients -name 'mygrok-*' | wc -l | tr -d ' ') variants"
else
  warn "skipping client rebuild (--skip-clients)"
  if ! find cmd/mygrokd/embed/clients -name 'mygrok-*' -print -quit | grep -q .; then
    warn "no staged clients — the server will 404 on /dl until you build them"
  fi
fi

# --- step 2: server cross-compile -------------------------------------------

step "Cross-compiling mygrokd ($SERVER_GOOS/$SERVER_GOARCH)"
CGO_ENABLED=0 GOOS="$SERVER_GOOS" GOARCH="$SERVER_GOARCH" \
  go build -trimpath -ldflags "$LDFLAGS" -o dist/mygrokd-server ./cmd/mygrokd
ok "dist/mygrokd-server ($(du -h dist/mygrokd-server | cut -f1))"

# --- step 3: native client for this machine ---------------------------------

step "Building native client"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o dist/mygrok ./cmd/mygrok
ok "dist/mygrok"

if [ "$DO_INSTALL" = "1" ]; then
  step "Installing to $BIN_DIR/mygrok"
  # install(1) rather than cp: macOS SIGKILLs an ad-hoc-signed Go binary
  # written over an existing file, because the kernel caches a "won't run"
  # verdict against the old inode. install(1) writes a fresh inode.
  if [ -w "$BIN_DIR" ]; then
    install -m 0755 dist/mygrok "$BIN_DIR/mygrok"
    ok "$BIN_DIR/mygrok ($(./dist/mygrok version | head -1))"
  elif [ -d "$BIN_DIR" ]; then
    sudo install -m 0755 dist/mygrok "$BIN_DIR/mygrok"
    ok "$BIN_DIR/mygrok (via sudo)"
  else
    warn "$BIN_DIR doesn't exist; skipping local install"
  fi
else
  warn "skipping local install (--no-install)"
fi

# --- step 4: deploy ----------------------------------------------------------

if [ "$DO_DEPLOY" = "1" ]; then
  [ -n "$HOST" ] || die "MYGROK_HOST not set — nothing to deploy to (use --no-deploy to build only)"
  if [ -n "$SSH_KEY" ] && [ ! -f "$SSH_KEY" ]; then
    die "SSH key not found at $SSH_KEY"
  fi

  step "Deploying mygrokd to $SSH_USER@$HOST"
  # shellcheck disable=SC2046  # ssh_args is deliberately word-split
  scp -q $(ssh_args) dist/mygrokd-server "$SSH_USER@$HOST:/tmp/mygrokd-new"
  ok "uploaded"

  # shellcheck disable=SC2046
  ssh -q $(ssh_args) "$SSH_USER@$HOST" '
    set -e
    sudo install -m 0755 /tmp/mygrokd-new /usr/local/bin/mygrokd
    rm -f /tmp/mygrokd-new
    # Bind :80/:443 without running as root.
    # Not required under systemd (the unit grants it ambiently) and fails
    # on some filesystems, so never let it abort the deploy.
    sudo setcap "cap_net_bind_service=+ep" /usr/local/bin/mygrokd 2>/dev/null || true
    sudo systemctl restart mygrokd
    sleep 2
    if ! systemctl is-active --quiet mygrokd; then
      echo "    !! mygrokd failed to start; recent logs:"
      sudo journalctl -u mygrokd -n 20 --no-pager
      exit 1
    fi
    echo "    server up: $(systemctl show -p ActiveState,SubState mygrokd | tr "\n" " ")"
  '
  ok "mygrokd restarted"

  step "Smoke test"
  # Read it all first: `curl | head` under pipefail reports failure on a
  # healthy server, because head exits and curl takes a SIGPIPE.
  if body="$(curl -fsS -m 10 "https://$HOST/install")" && \
     printf '%s' "$body" | head -1 | grep -q "bash"; then
    ok "https://$HOST/install responds"
  else
    warn "smoke test failed — check the server logs"
  fi
else
  warn "skipping deploy (--no-deploy)"
fi

step "Done"
echo
if [ -n "$HOST" ]; then
  echo "  Server:    https://$HOST/"
  echo "  Install:   curl -sSL https://$HOST/install | bash"
fi
echo "  Local:     $BIN_DIR/mygrok"
echo

#!/usr/bin/env bash
# install-server.sh — stand up a mygrok tunnel server on a fresh Linux box.
#
#   curl -sSL https://raw.githubusercontent.com/schappim/mygrok/main/deploy/install-server.sh \
#     | sudo bash -s -- --domain example.com --email you@example.com
#
# It will:
#   1. create the mygrokd user, /etc/mygrokd and /var/lib/mygrokd
#   2. install the mygrokd binary (release download, or build from source)
#   3. generate an auth token if you didn't supply one
#   4. write a systemd unit + a root-only environment file
#   5. allow :80/:443/:7000 through ufw or firewalld, if either is active
#   6. start the service and print the DNS records you still need to add
#
# Idempotent: re-running upgrades the binary and leaves your token, certs,
# and access rules alone.
#
# Options:
#   --domain <d>        public host; tunnels become <sub>.<d>       (required)
#   --email <e>         ACME contact address for Let's Encrypt      (required)
#   --token <t>         shared auth token       (default: generated for you)
#   --dns-provider <p>  none|route53|cloudflare|digitalocean        (default: none)
#   --version <v>       release tag to install                   (default: latest)
#   --binary <path>     install this mygrokd binary instead of downloading one
#   --from-source       build with the local Go toolchain instead of downloading
#   --no-start          install everything but don't start the service
#   --uninstall         stop and remove the service (keeps /var/lib/mygrokd)
#
# With --dns-provider you must also put the provider's credentials in
# /etc/mygrokd/mygrokd.env after install — see docs/server.md. Without one,
# certificates are issued per hostname on demand and nothing else is needed.

set -euo pipefail

# Everything below lives in main(), invoked by the single line at the end of
# the file. This is the standard guard for a script people pipe into bash: a
# truncated download — dropped connection, proxy hiccup — would otherwise
# execute however many complete lines arrived, which for an installer that
# creates users and rewrites systemd units is not a good failure mode. With
# the wrapper, a partial file defines a function and does nothing.

main() {

REPO="schappim/mygrok"
DOMAIN=""
EMAIL=""
TOKEN=""
DNS_PROVIDER="none"
DNS_PROVIDER_SET=0
VERSION="latest"
BINARY=""
FROM_SOURCE=0
DO_START=1
UNINSTALL=0

SVC_USER="mygrokd"
BIN=/usr/local/bin/mygrokd
ETC=/etc/mygrokd
VAR=/var/lib/mygrokd
UNIT=/etc/systemd/system/mygrokd.service
ENVFILE="$ETC/mygrokd.env"

# --- output ------------------------------------------------------------------

step() { printf "\n\033[1;36m==> %s\033[0m\n" "$*"; }
ok()   { printf "    \033[1;32m✓\033[0m %s\n" "$*"; }
warn() { printf "    \033[1;33m!\033[0m %s\n" "$*"; }
die()  { printf "\n\033[1;31m✗ %s\033[0m\n" "$*" >&2; exit 1; }

# --- args --------------------------------------------------------------------

while [ $# -gt 0 ]; do
  case "$1" in
    --domain)       DOMAIN="${2:?--domain needs a value}"; shift 2 ;;
    --email)        EMAIL="${2:?--email needs a value}"; shift 2 ;;
    --token)        TOKEN="${2:?--token needs a value}"; shift 2 ;;
    --dns-provider) DNS_PROVIDER="${2:?--dns-provider needs a value}"; DNS_PROVIDER_SET=1; shift 2 ;;
    --version)      VERSION="${2:?--version needs a value}"; shift 2 ;;
    --binary)       BINARY="${2:?--binary needs a value}"; shift 2 ;;
    --from-source)  FROM_SOURCE=1; shift ;;
    --no-start)     DO_START=0; shift ;;
    --uninstall)    UNINSTALL=1; shift ;;
    -h|--help)      sed -n '2,/^set -euo/p' "$0" | sed -e 's/^#$//' -e 's/^# //' -e '/^set -euo/d'; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[ "$(id -u)" = "0" ] || die "run as root (prefix with sudo)"

# --- uninstall ---------------------------------------------------------------

if [ "$UNINSTALL" = "1" ]; then
  step "Removing mygrokd"
  systemctl disable --now mygrokd 2>/dev/null || true
  rm -f "$UNIT"
  systemctl daemon-reload
  rm -f "$BIN"
  ok "service and binary removed"
  warn "kept $VAR (certs, access rules, passkeys) and $ETC (token)"
  warn "delete them yourself if you really mean it: rm -rf $VAR $ETC"
  exit 0
fi

[ -n "$DOMAIN" ] || die "--domain is required, e.g. --domain example.com"
[ -n "$EMAIL" ]  || die "--email is required (Let's Encrypt needs a contact address)"
case "$DOMAIN" in
  *.*) ;;
  *) die "--domain should be a registered domain like example.com, got '$DOMAIN'" ;;
esac

command -v systemctl >/dev/null || die "this script needs systemd"

# --- install the binary ------------------------------------------------------

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture: $ARCH" ;;
esac

# NOTE: these run inside an `if` condition, which disables errexit for the
# whole function body. Every step therefore needs its own `|| return 1` —
# without it a failed install(1) would be swallowed and we'd go on to write
# a systemd unit pointing at a binary that isn't there.
install_from_release() {
  local tag="$VERSION" url
  if [ "$tag" = "latest" ]; then
    tag="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep -m1 '"tag_name"' | cut -d'"' -f4)" || true
    [ -n "$tag" ] || return 1
  fi
  url="https://github.com/$REPO/releases/download/$tag/mygrokd-linux-$ARCH"
  step "Downloading mygrokd $tag ($ARCH)"
  curl -fsSL "$url" -o /tmp/mygrokd-new || return 1

  # Every release publishes SHA256SUMS. Check against it when we can get it
  # and sha256sum exists; a missing checksum file is not fatal (an older
  # release may predate it) but a MISMATCH always is.
  if command -v sha256sum >/dev/null \
     && curl -fsSL "https://github.com/$REPO/releases/download/$tag/SHA256SUMS" -o /tmp/mygrokd-sums 2>/dev/null; then
    local want got
    want="$(grep " mygrokd-linux-$ARCH\$" /tmp/mygrokd-sums | cut -d" " -f1 | head -1)"
    got="$(sha256sum /tmp/mygrokd-new | cut -d" " -f1)"
    rm -f /tmp/mygrokd-sums
    if [ -n "$want" ] && [ "$want" != "$got" ]; then
      rm -f /tmp/mygrokd-new
      die "checksum mismatch for mygrokd-linux-$ARCH (expected $want, got $got) — refusing to install"
    fi
    [ -n "$want" ] && ok "sha256 verified"
  else
    rm -f /tmp/mygrokd-sums 2>/dev/null || true
    warn "could not verify a checksum for this download"
  fi
  install -m 0755 /tmp/mygrokd-new "$BIN" || { rm -f /tmp/mygrokd-new; return 1; }
  rm -f /tmp/mygrokd-new
  ok "$BIN ($tag)"
}

# The server //go:embed's the client binaries it serves from /install and
# /dl, and a fresh checkout has none. Building only ./cmd/mygrokd would
# produce a server that starts fine and then 404s the very command its own
# landing page tells people to run — so build the clients first, exactly as
# build.sh does.
install_from_source() {
  step "Building from source"
  command -v go >/dev/null \
    || die "Go toolchain not found — install Go, or drop --from-source to use a release build"
  command -v git >/dev/null || die "git not found"

  local src clone_args
  src="$(mktemp -d)" || return 1
  # Honour --version here too. Without this a pinned tag would silently
  # build whatever is on main.
  clone_args="--depth 1"
  if [ "$VERSION" != "latest" ]; then
    clone_args="$clone_args --branch $VERSION"
  fi
  # shellcheck disable=SC2086  # clone_args is deliberately word-split
  git clone $clone_args "https://github.com/$REPO.git" "$src/mygrok" >/dev/null 2>&1 \
    || { rm -rf "$src"; die "git clone failed (is '$VERSION' a real tag?)"; }

  local ldflags="-s -w"
  (
    set -e
    cd "$src/mygrok"
    mkdir -p cmd/mygrokd/embed/clients
    for t in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
      printf "    building client %s...\n" "$t"
      CGO_ENABLED=0 GOOS="${t%-*}" GOARCH="${t#*-}" \
        go build -trimpath -ldflags "$ldflags" -o "cmd/mygrokd/embed/clients/mygrok-$t" ./cmd/mygrok
    done
    printf "    building server...\n"
    CGO_ENABLED=0 go build -trimpath -ldflags "$ldflags" -o "$src/mygrokd" ./cmd/mygrokd
  ) || { rm -rf "$src"; die "go build failed (a 512MB box may need swap to compile this)"; }

  install -m 0755 "$src/mygrokd" "$BIN" || { rm -rf "$src"; die "install to $BIN failed"; }
  rm -rf "$src"
  ok "$BIN (built from source, with clients embedded)"
}

if [ -n "$BINARY" ]; then
  step "Installing mygrokd from $BINARY"
  [ -f "$BINARY" ] || die "no such file: $BINARY"
  install -m 0755 "$BINARY" "$BIN"
  ok "$BIN"
elif [ "$FROM_SOURCE" = "1" ]; then
  install_from_source
elif ! install_from_release; then
  warn "no release build available for linux-$ARCH; falling back to source"
  install_from_source
fi

# Whatever route we took, make sure we ended up with something runnable
# before we build a service around it. We only care that the kernel could
# execute it at all: 126 means "not executable", 127 means "not found", and
# both are what a truncated download or a wrong-architecture binary look
# like. Any other exit status means the program ran and had an opinion,
# which is all we need to know here.
[ -x "$BIN" ] || die "$BIN is missing or not executable after install"
"$BIN" --version >/dev/null 2>&1
case $? in
  126|127) die "$BIN would not execute — wrong architecture, or a truncated download" ;;
esac

# Under systemd the unit grants CAP_NET_BIND_SERVICE ambiently, which is
# what actually lets mygrokd bind :80/:443. This setcap is for anyone who
# runs the binary by hand; it's a no-op for the service (NoNewPrivileges
# makes the kernel ignore file capabilities), so a failure here is fine.
if command -v setcap >/dev/null; then
  setcap "cap_net_bind_service=+ep" "$BIN" 2>/dev/null \
    && ok "granted cap_net_bind_service (for manual runs)" \
    || warn "setcap failed — harmless, the systemd unit grants the capability itself"
fi

# --- user, dirs, token -------------------------------------------------------

step "Preparing $SVC_USER, $ETC, $VAR"
id -u "$SVC_USER" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
install -d -m 0755 -o root      -g root      "$ETC"
install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$VAR"
ok "user and directories ready"

# Preserve an existing token across re-runs — regenerating it would lock
# out every client already configured against this server.
if [ -z "$TOKEN" ] && [ -f "$ENVFILE" ]; then
  TOKEN="$(grep -m1 '^MYGROK_AUTHTOKEN=' "$ENVFILE" | cut -d= -f2- || true)"
  [ -n "$TOKEN" ] && ok "reusing the existing auth token" || true
fi
GENERATED=0
if [ -z "$TOKEN" ]; then
  TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  GENERATED=1
  ok "generated a new auth token"
fi

# Written fresh each run, but any extra lines you added (DNS credentials,
# for instance) are carried over.
EXTRA=""
if [ -f "$ENVFILE" ]; then
  EXTRA="$(grep -v -e '^MYGROK_AUTHTOKEN=' -e '^MYGROK_PUBLIC_HOST=' "$ENVFILE" || true)"
fi
umask 077
{
  echo "MYGROK_AUTHTOKEN=$TOKEN"
  echo "MYGROK_PUBLIC_HOST=$DOMAIN"
  [ -n "$EXTRA" ] && echo "$EXTRA" || true
} > "$ENVFILE"
chmod 0600 "$ENVFILE"
chown root:root "$ENVFILE"
ok "$ENVFILE (root-only)"

# --- systemd unit ------------------------------------------------------------

# Re-running must not quietly undo configuration. If the unit already names
# a DNS provider and this run didn't ask for one, keep what's there —
# otherwise an upgrade would drop the operator's wildcard certificates back
# to per-hostname issuance without saying so.
if [ "$DNS_PROVIDER_SET" = "0" ] && [ -f "$UNIT" ]; then
  EXISTING_DNS="$(sed -n 's/.*--dns-provider=\([a-z0-9]*\).*/\1/p' "$UNIT" | head -1)"
  if [ -n "$EXISTING_DNS" ] && [ "$EXISTING_DNS" != "$DNS_PROVIDER" ]; then
    DNS_PROVIDER="$EXISTING_DNS"
    ok "keeping --dns-provider=$DNS_PROVIDER from the existing unit"
  fi
fi

step "Writing $UNIT"
cat > "$UNIT" <<UNITEOF
[Unit]
Description=mygrok tunnel server
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target
# Give up after 5 failed starts in 5 minutes rather than restarting forever.
# A hot restart loop against Let's Encrypt burns the failed-validation limit
# and locks the operator out of certificates for an hour.
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
EnvironmentFile=$ENVFILE
ExecStart=$BIN \\
  --public-host=$DOMAIN \\
  --http=:80 \\
  --https=:443 \\
  --tunnel=:7000 \\
  --dns-provider=$DNS_PROVIDER \\
  --cert-email=$EMAIL \\
  --cert-dir=$VAR/certs \\
  --ip-list=$VAR/iplist.json \\
  --passkeys=$VAR/passkeys.json \\
  --tunnel-locks=$VAR/tunnellocks.json \\
  --invites=$VAR/invites.json
Restart=always
RestartSec=10

# Bind :80/:443 as an unprivileged user. This has to be an ambient
# capability, not a setcap on the binary: NoNewPrivileges=true makes the
# kernel ignore file capabilities entirely, so a setcap'd binary would
# fail with "permission denied" on :80 and nothing would say why.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

# The service only ever needs its own state directory.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$VAR
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
UNITEOF
chmod 0644 "$UNIT"
systemctl daemon-reload
ok "unit installed"

# --- firewall ----------------------------------------------------------------

if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "^Status: active"; then
  step "Opening ports in ufw"
  for p in 80/tcp 443/tcp 7000/tcp; do ufw allow "$p" >/dev/null && ok "$p"; done
elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
  step "Opening ports in firewalld"
  for p in 80/tcp 443/tcp 7000/tcp; do firewall-cmd --permanent --add-port="$p" >/dev/null && ok "$p"; done
  firewall-cmd --reload >/dev/null
else
  warn "no active ufw/firewalld found — make sure 80, 443 and 7000/tcp are reachable"
  warn "(on AWS/GCP that means the security group or firewall rule, not the box)"
fi

# --- start -------------------------------------------------------------------

# Work out this box's public address for the DNS instructions below. Try
# locally first — on a cloud VM the default route usually has it — and only
# ask a third party if that fails, since doing so tells them this host just
# became a tunnel server. MYGROK_PUBLIC_IP=<ip> skips the lookup entirely.
PUBIP="${MYGROK_PUBLIC_IP:-}"
if [ -z "$PUBIP" ]; then
  # `|| true`: under errexit an assignment takes the exit status of its
  # command substitution, so a box without iproute2 would kill the script
  # here — after everything is already installed, and just before the
  # summary that tells the operator what to do next.
  PUBIP="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.*src \([0-9.]*\).*/\1/p' | head -1 || true)"
  case "$PUBIP" in
    ""|10.*|192.168.*|172.1[6-9].*|172.2[0-9].*|172.3[01].*|127.*)
      # Behind NAT (or nothing useful found) — the local address isn't the
      # one DNS needs, so fall back to an external lookup.
      PUBIP="$(curl -fsS -m 5 https://api.ipify.org 2>/dev/null || echo '<this-server-ip>')"
      ;;
  esac
fi

if [ "$DO_START" = "1" ]; then
  # `enable --now` starts a stopped service but does nothing to a running
  # one, so on an upgrade the old binary would stay resident and the whole
  # documented "just re-run the installer" flow would be a no-op.
  if systemctl is-active --quiet mygrokd; then
    step "Restarting mygrokd"
    systemctl enable mygrokd >/dev/null 2>&1 || true
    systemctl restart mygrokd
  else
    step "Starting mygrokd"
    systemctl enable --now mygrokd
  fi
  sleep 2
  if systemctl is-active --quiet mygrokd; then
    ok "running"
  else
    warn "mygrokd did not start; recent logs:"
    journalctl -u mygrokd -n 25 --no-pager || true
    warn "this is expected if DNS isn't pointed here yet — fix DNS, then: systemctl restart mygrokd"
  fi
else
  warn "not started (--no-start). Start it with: systemctl enable --now mygrokd"
fi

# --- what's left -------------------------------------------------------------

# Only show the token on an interactive terminal. Otherwise — piped into a
# log, captured by cloud-init, or on an EC2 console log readable via
# ec2:GetConsoleOutput — it would be persisted in cleartext somewhere the
# 0600 env file was specifically chosen to avoid.
if [ -t 1 ]; then
  TOKEN_DISPLAY="$TOKEN"
else
  TOKEN_DISPLAY='$(sudo grep MYGROK_AUTHTOKEN '"$ENVFILE"' | cut -d= -f2-)'
fi

cat <<SUMMARY

$(printf '\033[1;36m==> Almost there — add these DNS records\033[0m')

    A     *.$DOMAIN        $PUBIP      TTL 60
    A     tunnel.$DOMAIN   $PUBIP      TTL 60

  The wildcard serves every tunnel; the tunnel.* record is what clients
  dial, and it needs its own entry because a wildcard doesn't cover it.

$(printf '\033[1;36m==> Then, on your laptop\033[0m')

    curl -sSL https://tunnel.$DOMAIN/install | bash
    export MYGROK_AUTHTOKEN=$TOKEN_DISPLAY
    mygrok http 3000 --subdomain=app

    → https://app.$DOMAIN

SUMMARY

if [ "$GENERATED" = "1" ]; then
  printf '\033[1;33m  Read it with: sudo grep MYGROK_AUTHTOKEN %s\033[0m\n\n' "$ENVFILE"
fi

if [ "$DNS_PROVIDER" != "none" ]; then
  printf '\033[1;33m  --dns-provider=%s needs credentials in %s.\033[0m\n' "$DNS_PROVIDER" "$ENVFILE"
  printf '  See https://github.com/%s/blob/main/docs/server.md#certificates, then: systemctl restart mygrokd\n\n' "$REPO"
fi
}

main "$@"

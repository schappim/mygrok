---
name: mygrok
description: Use this skill when the user wants to expose a local port or folder to the public internet via mygrok — a self-hosted ngrok-style tunnel. Covers running tunnels (mygrok http / mygrok serve), exposing local MCP servers as claude.ai custom connectors (mygrok mcp, capability-URL auth), installing tunnels as long-running services (launchd / systemd), managing IP allow/block lists, onboarding visitors with WebAuthn passkeys (admin invite, grant, revoke), claiming custom hostnames via CNAME + on-demand TLS, and self-updating the binary.
---

# mygrok

`mygrok` is a CLI for forwarding `https://<subdomain>.<publicHost>` to a local port or folder. The server (`mygrokd`) runs on a box you control with wildcard DNS pointed at it; the client is a single Go binary on every machine that wants to expose something.

Throughout this file, `example.com` stands in for whatever domain the server is configured with (`mygrokd --public-host=`), and `tunnel.example.com` for its management host. Substitute the real one — `mygrok version` prints the server a downloaded binary was built for.

This skill is the operating manual. `docs/` in the repo has the full design rationale; this file is the day-to-day reference for driving the CLI.

## When to use this skill

Trigger this skill when the user asks to:

- expose a local port or folder publicly (`tunnel`, `share`, `forward`, `expose`)
- start / stop / list / inspect a long-running tunnel service
- lock down or open up access (IP rules, passkey gate, basic auth)
- onboard a new visitor (invite link, grant access)
- claim a custom hostname (CNAME) for a tunnel
- update the client binary
- diagnose "tunnel offline", `subdomain in use`, `auth token required`, or other mygrok errors

If the user is asking *how the server itself is deployed* (DNS records, certificates, the systemd unit), point them to `docs/server.md` instead — that's not driven through the CLI.

## Prereqs to verify before running anything

1. `mygrok` is on PATH: `command -v mygrok`. If missing, install via `curl -sSL https://tunnel.example.com/install | bash` (set `MYGROK_INSTALL_DIR=$HOME/bin` to skip sudo).
2. Auth token is resolvable. Resolution order: `--auth=` flag → `MYGROK_AUTHTOKEN` env → `~/.mygrok/authtoken` (mode 0600). If none, the user must export `MYGROK_AUTHTOKEN` before any command other than standalone `mygrok serve <dir>` (no tunnel).
3. For brand-new boxes, also `mygrok update` after install to pick up any client-side changes shipped since the last embed.

## The most common requests, mapped to commands

### "Tunnel my local Rails app on :3000 to https://jarvis.example.com"

```bash
mygrok http 3000 --subdomain=jarvis
```

Foreground; auto-reconnects on disconnect. Add `--basic-auth=user:pass` to gate the tunnel, or `--hostname=app.example.com` to also serve it under a custom domain.

If a `.mygrok.toml` in cwd (or any parent) declares `subdomain = "jarvis"` and `port = 3000`, the bare command works:

```bash
mygrok            # equivalent to `mygrok http`
```

### "Make jarvis survive reboots"

```bash
mygrok service install 3000 --subdomain=jarvis
```

- macOS → writes `~/Library/LaunchAgents/dev.mygrok.agent.<name>.plist` (0600) and `launchctl load -w`s it. The auth token and any `--basic-auth` live in the plist's environment, never in argv. Agents from before 1.0 used the `cloud.schappi.mygrok.` prefix; `list`, `status`, and `uninstall` still find those.
- Linux → writes `/etc/systemd/system/mygrok-<name>.service`, `daemon-reload && enable --now`.

The auth token, basic-auth creds, and any custom hostnames are baked into the unit at install time. Use `--name=<id>` if you want the service identifier different from the subdomain (e.g., to install two tunnels from the same project tree).

Manage:

```bash
mygrok service list
mygrok service status   <name>
mygrok service logs     <name>      # tail -f / journalctl -f
mygrok service uninstall <name>
```

### "Expose this MCP server so I can add it to claude.ai" (MCP connector)

```bash
mygrok mcp 8790 --subdomain=jarvis            # foreground; prints the connector URL
mygrok service install 8790 --subdomain=jarvis --mcp   # permanent; also prints it
```

The printed URL looks like `https://jarvis.example.com/<token>/mcp` — the
token IS the credential (claude.ai custom connectors can't send API keys, so
auth rides in the URL). Paste it into claude.ai → Settings → Connectors →
*Add custom connector*, and never share it in public places. Wrong-token
requests get a bare 404 and never reach the backend; valid ones are forwarded
with the token stripped, so the local MCP server serves plain `/mcp`.

- Token persists in `~/.mygrok/mcp/<subdomain>.secret` — same URL after restarts. Delete the file (or pass `--secret=<new>`) to rotate, then update the connector in claude.ai.
- Backend already enforces its own secret path? `--secret=<that-token> --no-strip`.
- To wrap a CLI/program as a connector: put a minimal streamable-HTTP MCP server in front of it on `127.0.0.1:<port>` (Python `mcp` SDK works well), then `mygrok mcp <port>`. SSE and long tool calls pass through — the tunnel is raw TCP.

### "Share this folder publicly"

```bash
mygrok serve                   # cwd at <random-6-letters>.example.com
mygrok serve gallery           # cwd at gallery.example.com
mygrok serve gallery ~/photos  # ~/photos at gallery.example.com
mygrok serve ./photos          # standalone, no tunnel — http://127.0.0.1:8080
```

`serve` wraps `http.FileServer` (directory listings, range requests, content-type sniffing) and adds a default favicon when the folder lacks one.

`mygrok serve` infers form from positional arguments: a token that matches the subdomain charset (lowercase letters, digits, hyphens, no `/` or `.`) is treated as the subdomain; anything else is treated as the directory.

### "Update the client binary on this box"

```bash
mygrok update
```

Pulls `https://tunnel.<host>/dl/mygrok-<os>-<arch>` and `install(1)`s it over the running file (fresh inode — important on macOS, where naive `cp`/`mv` of an ad-hoc-signed Go binary triggers SIGKILL on next exec). Long-running launchd/systemd processes keep running their old code until next restart.

### "Lock down jarvis to office IP"

```bash
mygrok admin allow jarvis 203.0.113.7 --note=office
```

Other admin IP rule commands:

```bash
mygrok admin rules                     # show everything
mygrok admin rules jarvis              # just one tunnel
mygrok admin rules global              # global rules only
mygrok admin block global 198.51.100.0/24 --note="scanner range"
mygrok admin unallow jarvis 203.0.113.7
mygrok admin unblock global 198.51.100.0/24
mygrok admin tunnels                   # list registered + configured tunnels
```

Scope is `global` or a subdomain. Address can be a single IP or CIDR (v4 or v6). All admin commands accept `--server=`, `--auth=`, and `--config=`. All except `login` also accept `--json`.

Eval order on each request: global block → per-tunnel block → per-tunnel allowlist (if non-empty, must match) → global allowlist (if non-empty AND no per-tunnel allowlist, must match) → otherwise allow. **Default is open** — a tunnel with no rules accepts everyone, so casual `mygrok serve ./photos` stays public unless you explicitly lock it.

### "Onboard a new person to a tunnel via passkey"

```bash
# 1. Issue invite (one-shot, 7-day TTL, 192-bit token).
mygrok admin invite alice
# → prints https://tunnel.example.com/invite/<48-hex>

# 2. Send that URL to Alice via Slack/email/whatever.
#    She opens it in a browser, picks Touch ID / phone / hardware key.
#    User "alice" is created and her first credential is bound.

# 3. Give her access to specific tunnels.
mygrok admin grant jarvis alice
mygrok admin grant admin-app alice
```

Removal:

```bash
mygrok admin revoke jarvis alice         # drop her from one tunnel
mygrok admin users delete alice          # nuclear: remove user + all credentials
```

Other passkey-related commands:

```bash
mygrok admin invites                     # list pending + consumed invites
mygrok admin invites revoke <token>      # cancel a pending invite
mygrok admin users                       # list all users + credential count
mygrok admin lock <subdomain>            # convenience: grant every existing user
mygrok admin unlock <subdomain>          # remove all grants from a tunnel
mygrok admin passkeys                    # list every credential across all users
mygrok admin passkeys delete <id>        # nuke one credential
```

The visitor flow: locked tunnel → 302 to `tunnel.<host>/auth?return=<url>` → discoverable WebAuthn assertion → 24 h cookie scoped to `.<publicHost>` → redirect back. Cookie covers every tunnel under the wildcard zone. **Do not combine `--hostname=<custom>` with the passkey gate** — the cookie's not scoped to the custom origin.

If zero credentials exist server-wide, the gate auto-bypasses. That's intentional — a misclicked lock can't lock you out before any passkey exists.

**Adding another device (self-serve).** A signed-in user can manage their own passkeys at `https://tunnel.<publicHost>/account` — register an extra device for a new laptop/phone, or delete an old credential they no longer use. No admin involvement, no second invite. The page refuses to delete the *last* passkey on file (to avoid self-lockout). If the user is locked out of *all* their devices, the only path is admin-side: `mygrok admin users delete <name>` then `mygrok admin invite <name>` for a fresh start.

### "Open the admin web UI"

```bash
mygrok admin                       # opens browser to magic-link login
mygrok admin login --open=false    # just print the URL (e.g., over SSH)
```

The magic link encodes `MYGROK_AUTHTOKEN` once. The server validates it, sets a 24 h `mygrok_admin` session cookie, and 302-redirects to a clean URL — so the token only appears in browser history for one redirect hop.

### "I forgot the full admin command surface"

`mygrok admin help` is the source of truth. It documents every subcommand, every flag, JSON schemas of all `--json` output, exit codes (`0` ok, `2` bad args, `3` server error), and environment variables. Pipe it into your context if anything in this skill is ambiguous.

### "Make same-WiFi visitors bypass the cloud round-trip" (LAN-direct)

```bash
mygrok http 3000 --subdomain=jarvis --lan=auto
# or pin: --lan=192.168.1.50
# tweak the local TLS port: --lan-port=8443 (default)
```

Server writes `jarvis-lan.example.com → <RFC1918 IP>` through whichever `--dns-provider` is configured (route53, cloudflare, or digitalocean — `mygrokd --lan` requires one), ships the wildcard cert+key down the control channel, and the client serves HTTPS on its LAN IP. Visitors whose public IP matches the client's get a 307 to `jarvis-lan.example.com`; off-LAN visitors keep using the tunnel. Real Let's Encrypt cert via the `*.<publicHost>` wildcard — no browser warnings, no `/etc/hosts`, no local CA.

Caveats to flag if the user asks:

- **CGNAT**: if the ISP shares one public IP across many subscribers, same-NAT detection misfires — would 307 random strangers to a LAN URL they can't reach. Opt in only when the public IP is yours.
- **Custom hostnames + LAN-direct don't combine.** Same scoping issue as the passkey gate.
- **Subdomain ending in `-lan` is rejected** when `--lan` is set (collides with sister-hostname pattern).
- **The wildcard cert+key is held in memory by every connected client** for the life of the tunnel — it is never written to disk. Still: any machine with the shared token can obtain a key valid for your whole zone. Trust your boxes, or leave `--lan` off.
- **Escape hatch**: append `?nolan=1` to force the public path even on the LAN.
- **Server disable**: `mygrokd --lan=false` refuses all LAN handshakes.

## Custom hostnames (CNAME)

```bash
mygrok http 3000 --subdomain=jarvis --hostname=app.example.com
# multiple:
mygrok http 3000 --subdomain=jarvis --hostname=app.example.com,api.example.com
```

DNS prereq at the user's provider:

```
app.example.com.  CNAME  tunnel.example.com.
# or A <tunnel-server-ip> if CNAME-at-apex isn't available
```

First HTTPS hit triggers on-demand TLS issuance via TLS-ALPN-01 (or HTTP-01 on :80). The server's `OnDemand.DecisionFunc` only allows issuance for hostnames currently registered by an active tunnel — so dead/unregistered names can't farm certs.

Caveats:

- DNS must already resolve to our IP before the first HTTPS request, otherwise cert acquisition fails and the request 502s until the next try.
- Hostnames under the server's own `--public-host` zone are rejected — those use the wildcard cert path via `--subdomain`.
- Don't combine custom hostnames with the passkey gate (see above).

## Config file (`.mygrok.toml`)

Auto-discovered: cwd → parents up to `/` → `~/.mygrok/config.toml`. The first match wins. Use `--config=<path>` to override.

```toml
subdomain = "jarvis"
port      = 3000
host      = "127.0.0.1"                 # optional
server    = "tunnel.example.com:7000"   # optional
auth      = "..."                       # prefer env or ~/.mygrok/authtoken
```

Precedence: CLI flag > env (`MYGROK_AUTHTOKEN`, `MYGROK_SERVER`) > config file > built-in default. The positional `<port>` wins over `port = ...`.

`mygrok service install` reads the same config — so `cd ~/code/myapp && mygrok service install` will bake the project's subdomain/port/auth into the launchd/systemd unit.

## Typical error → fix lookup

| Error from CLI/server | Cause | Fix |
|---|---|---|
| `auth token required` | No `--auth`, no `MYGROK_AUTHTOKEN`, no `~/.mygrok/authtoken` | Set one of the three. |
| `dial server: lookup tunnel.example.com: no such host` | DNS lag or A record missing | `dig tunnel.example.com`; restore the A record if needed. |
| Styled "tunnel offline" page in browser | No client connected for that subdomain | Check the client process / `mygrok service status`. |
| `server rejected: subdomain in use` | Another live client (different `client_id`) holds the name | Pick a different subdomain or stop the other client. (Same `client_id` would auto-takeover.) |
| `server rejected: invalid or reserved subdomain` | Reserved (`www`, `api`, `admin`, `mail`) or non-DNS-safe | Pick a valid name. |
| `expected SETTINGS frame` over HTTPS | h2 leaked into ALPN | Server bug, not a user fix; report. |
| Cert acquisition fails on a custom hostname | DNS hasn't propagated yet | Wait, retry. The next request usually succeeds. |
| 502 on a custom hostname after DNS is fine | Underlying tunnel disconnected | Restart the local client. |

## Useful one-liners

```bash
# Live-tail server-side logs (only if the user has SSH access to the box):
ssh <user>@tunnel.example.com 'sudo journalctl -u mygrokd -f'

# Inspect rules with jq:
mygrok admin rules --json | jq '.tunnels_configured'

# Sanity-ping the public health of the server:
curl -sI https://tunnel.example.com/ | head -1
```

## What this skill deliberately does NOT cover

- Standing up a *new* `mygrokd` server (DNS records, cloud provisioning, certificate config). See `docs/server.md`.
- Editing `cmd/mygrokd/*.go` source. That's a code change, not a CLI workflow.
- HTTP/2 / gRPC / raw TCP forwarding — not supported by the wire protocol.

If the user asks about those, be upfront that they're out of scope for the skill and point at `docs/`.

## Cross-references inside the repo

- `docs/server.md` — standing up a server, DNS, certificates, troubleshooting.
- `docs/cli.md` — every command and flag, including `mygrokd`'s.
- `docs/access-control.md` — basic auth, IP rules, passkeys.
- `docs/architecture.md` — protocol, reconnect behaviour, LAN-direct, custom hostnames.
- `.mygrok.example.toml` — annotated config template.
- `examples/minimal.toml`, `examples/global.config.toml` — minimal + global-defaults variants.
- `mygrok admin help` — authoritative CLI reference (parses `--json` schemas, exit codes, env vars).

# CLI reference

Every command that talks to a server accepts `--server=` and `--config=`;
all but `mygrok update` also accept `--auth=` (update only fetches a public
binary). Values resolve **CLI flag → environment → config file → built-in
default**; see [Config file](config.md).

- [`mygrok`](#mygrok-no-subcommand) · [`http`](#mygrok-http-port) ·
  [`mcp`](#mygrok-mcp-port) · [`serve`](#mygrok-serve-subdomain-dir)
- [`admin`](#mygrok-admin) · [`service`](#mygrok-service-install-port) ·
  [`update`](#mygrok-update) · [`version`](#mygrok-version)
- [`mygrokd`](#mygrokd-the-server) — the server
- [Environment variables](#environment-variables) · [Exit codes](#exit-codes)

---

## `mygrok` (no subcommand)

Defaults to `mygrok http`. Useful when a [`.mygrok.toml`](config.md) supplies
the port and subdomain — `cd` into the project and run `mygrok`. With no
config and no flags it prints usage and exits 2.

---

## `mygrok http <port>`

Run a tunnel in the foreground. Reconnects automatically with exponential
backoff, capped at 30 seconds.

| Flag | Default | |
|---|---|---|
| `--subdomain=<name>` | *(required)* | Public subdomain. Lowercase letters, digits, dashes; 63 chars max. `www`, `api`, `admin`, and `mail` are reserved — see [why](server.md#the-wildcard-vs-specific-rule-that-catches-everyone). |
| `--server=<host:port>` | build-time / env / config | Tunnel control endpoint. |
| `--auth=<token>` | env / file | Auth token. |
| `--host=<addr>` | `127.0.0.1` | Local host to forward to. `0.0.0.0` when forwarding from another box. |
| `--basic-auth=<u>:<p>` | *(off)* | HTTP basic auth. Rejected requests get a `401` from the client, before your app sees them. Constant-time compare; credentials never leave your machine. |
| `--hostname=<host>` | *(none)* | Extra public hostname to claim, e.g. `app.example.com`. Comma-separate for several. See [custom hostnames](architecture.md#custom-hostnames-cname). |
| `--lan=auto\|<ip>` | *(off)* | Enable [LAN-direct](architecture.md#lan-direct). `auto` picks the first RFC1918 address on this box. |
| `--lan-port=<n>` | `8443` | Port for the LAN-direct TLS listener. |
| `--config=<path>` | auto-discover | Explicit `.mygrok.toml`. |

```bash
mygrok http 3000 --subdomain=jarvis
mygrok http 3000 --subdomain=jarvis --basic-auth=alice:s3cret
mygrok http 8080 --subdomain=orders --hostname=api.example.com
mygrok http 3000 --subdomain=jarvis --lan=auto
```

The request log shows the real visitor IP, method, status, duration, and
path — the server rewrites `X-Forwarded-For`/`X-Real-IP` with the true
source, so this matches what your app sees.

---

## `mygrok mcp <port>`

Expose a local MCP server as a claude.ai custom connector. Same tunnel as
`mygrok http`, plus a capability-URL gate — claude.ai connectors can't send
API keys, so the credential is an unguessable token in the path.

```bash
mygrok mcp 8790 --subdomain=tools
#   MCP connector URL (the URL is the credential — treat it as a secret):
#     https://tools.example.com/<token>/mcp
```

| Flag | Default | |
|---|---|---|
| `--secret=<token>` | load or generate | Capability token. Persisted to `~/.mygrok/mcp/<subdomain>.secret` (0600) so the URL survives restarts. |
| `--no-strip` | *(off)* | Forward the path with the token segment intact, for backends enforcing their own secret prefix. |
| `--path=<p>` | `/mcp` | Path suffix shown in the printed URL. The gate passes any suffix through. |

All `mygrok http` flags work here too. See **[MCP connectors](mcp.md)** for
the full picture.

---

## `mygrok serve [<subdomain>] [<dir>]`

A static file server with an optional tunnel in front. Wraps
`http.FileServer` — directory listings, content-type sniffing, range
requests, 304s — and supplies a default favicon when the folder has none.

The first positional is the subdomain when it looks like one (lowercase
letters, digits, hyphens, no `/`, `.`, `\`, or `~`); otherwise it's the
directory.

| Form | |
|---|---|
| `mygrok serve` | cwd at a random six-letter subdomain |
| `mygrok serve gallery` | cwd at `gallery.<your-domain>` |
| `mygrok serve gallery ./photos` | `./photos` at `gallery.<your-domain>` |
| `mygrok serve ./photos` | `./photos` on `127.0.0.1:8080`, no tunnel |

| Flag | Default | |
|---|---|---|
| `--port=<n>` | `8080` | Local port. `--port=0` picks a free one — handy with a subdomain, since the public URL is what you share. |
| `--host=<addr>` | `127.0.0.1` | Bind interface. `0.0.0.0` to also serve on the LAN. |
| `--subdomain=<name>` | *(positional)* | Explicit override. With this set, the first positional is the directory. |
| `--index=<file>` | `index.html` | Directory index filename. |
| `--basic-auth=<u>:<p>` | *(off)* | Works in both standalone and tunnel mode. |
| `--hostname=<host>` | *(none)* | Tunnel mode only. |

Standalone mode logs requests in the same format as the tunnel agent. In
tunnel mode the agent already logs, so the inner server stays quiet rather
than double-logging.

---

## `mygrok admin`

Manage the server: IP rules, passkey users, invites, and per-tunnel grants.
Bare `mygrok admin` opens the web UI via a magic link; every subcommand talks
to the HTTP API directly so scripts and LLMs can drive it without parsing
HTML.

### Access and rules

```
mygrok admin                              open the admin page in your browser
mygrok admin login [--open=false]         print (and optionally open) the URL
mygrok admin tunnels [--json]             list registered + configured tunnels
mygrok admin rules [<scope>] [--json]     show rules: everywhere, global, or one tunnel
mygrok admin allow   <scope> <ip|cidr> [--note=<t>]
mygrok admin block   <scope> <ip|cidr> [--note=<t>]
mygrok admin unallow <scope> <ip|cidr>
mygrok admin unblock <scope> <ip|cidr>
```

`<scope>` is `global` or a subdomain. `<ip|cidr>` is a single address
(`203.0.113.7`, `2001:db8::1`) or a range (`10.0.0.0/8`, `2001:db8::/64`).

### Users and passkeys

```
mygrok admin invite  <name>                    create a single-use invite link
mygrok admin invites [revoke <token>]          list / revoke pending invites
mygrok admin users   [delete <id-or-name>]     list / delete users
mygrok admin grant   <subdomain> <id-or-name>  let a user reach a tunnel
mygrok admin revoke  <subdomain> <id-or-name>  take that away
mygrok admin lock    <subdomain>               grant every existing user
mygrok admin unlock  <subdomain>               drop all grants on a tunnel
mygrok admin passkeys [list]                   all credentials, across users
mygrok admin passkeys delete <id>              revoke one credential
```

### Examples

```bash
mygrok admin allow jarvis 203.0.113.7 --note=office
mygrok admin block global 198.51.100.0/24 --note=scanner
mygrok admin rules --json | jq '.tunnels_configured'

mygrok admin invite alice
mygrok admin grant jarvis alice
```

**Auth.** Subcommands use HTTP basic auth — any username, password is
`MYGROK_AUTHTOKEN`. The browser flow swaps the token for a session cookie on
a single redirect hop, so it doesn't linger in history.

**`mygrok admin help`** prints the complete reference: every subcommand,
every flag, JSON schemas for all output, and exit codes. It's written to be
pasted into an LLM prompt.

---

## `mygrok service install <port>`

Install a tunnel as an OS service so it survives reboots.

| Flag | Default | |
|---|---|---|
| `--subdomain=<name>` | *(required)* | |
| `--name=<svc>` | same as subdomain | Service identifier, used for filenames and `service list/uninstall`. |
| `--auth=<token>` | env / file | Stored with the service at install time. |
| `--host=<addr>` | `127.0.0.1` | |
| `--basic-auth=<u>:<p>` | *(off)* | Whitespace is rejected at install time so the argv parses cleanly. |
| `--hostname=<host>` | *(none)* | Comma-separate for several. |
| `--lan=auto\|<ip>`, `--lan-port=<n>` | *(off)*, `8443` | Baked into the unit. |
| `--mcp` | *(off)* | Run `mygrok mcp` instead of `mygrok http`. Accepts `--secret`, `--no-strip`, `--path`. |

**macOS** writes `~/Library/LaunchAgents/dev.mygrok.agent.<name>.plist` and
`launchctl load -w`s it. Per-user agent, runs at login, logs to
`~/Library/Logs/mygrok-<name>.log`.

**Linux** writes `/etc/systemd/system/mygrok-<name>.service` plus a
`0600 root:root` environment file at `/etc/mygrok/<name>.env` holding the
auth token — units in `/etc/systemd/system` are world-readable and
`systemctl cat` will show one to any user, so the token doesn't go there.
Runs at boot, logs to journald. Uses `sudo`.

```bash
mygrok service install 3000 --subdomain=jarvis
mygrok service install 8080 --subdomain=app --name=app-prod
mygrok service install 8790 --subdomain=tools --mcp
```

Several tunnels on one machine is just several services with different
`--name`s.

### `mygrok service uninstall <name>`

Stops and removes it. macOS: unload + delete the plist. Linux: `disable
--now`, remove the unit and its environment file, `daemon-reload`.

### `mygrok service list` · `status <name>` · `logs <name>`

`list` shows what's installed here. `status` runs `launchctl list` or
`systemctl status`. `logs` tails the log file or `journalctl -f`.

Agents installed by older versions (label prefix `cloud.schappi.mygrok.`)
are still recognised by `list`, `status`, and `uninstall`.

---

## `mygrok update`

Pull the current client for this OS/arch from your server and install it over
the running executable.

| Flag | Default | |
|---|---|---|
| `--server=<host:port>` | env / trusted config | Host is used to build `https://<host>/dl/mygrok-<os>-<arch>`. A `.mygrok.toml` found by walking up from the current directory is ignored here — this command installs an executable, so only an explicit `--config` or your own `~/.mygrok/config.toml` may choose the source. |
| `--from=<url>` | *(none)* | Explicit URL, overrides `--server`. |

Uses `install(1)` so the destination gets a fresh inode. That matters on
macOS: the kernel caches a "won't run" verdict against an inode, and an
ad-hoc-signed Go binary written over an existing file with `cp` or `mv` gets
SIGKILLed on the next exec. Falls back to `sudo install` when the target
isn't writable.

The running process keeps its old code — it has the now-deleted inode's text
segment mapped. New invocations get the new binary; long-running services
pick it up on their next restart.

---

## `mygrok version`

Prints the version, and the default server if this build has one stamped in.
That second line is the quickest way to tell a binary downloaded from your
server apart from a generic Homebrew or `go install` build.

```
mygrok v1.0.0
default server: tunnel.example.com:7000
```

---

## `mygrokd` (the server)

Usually configured once by [`deploy/install-server.sh`](../deploy/install-server.sh)
and left alone. Full deployment guide: **[docs/server.md](server.md)**.

| Flag | Default | |
|---|---|---|
| `--public-host=<host>` | *(required)* | Base public host. Tunnel URLs become `<subdomain>.<public-host>`. Also settable as `MYGROK_PUBLIC_HOST`. |
| `--auth=<token>` | `MYGROK_AUTHTOKEN` | Shared auth token. Prefer the environment variable — a command line is visible in `ps`. |
| `--http=<addr>` | `:80` | Public HTTP listener. `""` disables. |
| `--https=<addr>` | *(off)* | Public HTTPS listener, e.g. `:443`. |
| `--tunnel=<addr>` | `:7000` | Control-plane listener that clients dial. |
| `--dns-provider=<p>` | `none` | `none`, `route53`, `cloudflare`, or `digitalocean`. Enables wildcard certificates via DNS-01 and LAN-direct records. |
| `--cert-domains=<list>` | derived | Names to pre-issue for. Defaults to `*.<public-host>,<public-host>` with a DNS provider, `tunnel.<public-host>` without. |
| `--cert-email=<e>` | *(none)* | ACME contact address. |
| `--cert-staging` | `false` | Use Let's Encrypt staging. Untrusted certificates, but no rate limits while you're iterating. |
| `--cert-dir=<path>` | `/var/lib/mygrokd/certs` | Certificate and ACME state cache. |
| `--ip-list=<path>` | `/var/lib/mygrokd/iplist.json` | IP allow/block lists. |
| `--passkeys=<path>` | `/var/lib/mygrokd/passkeys.json` | Users and their credentials. |
| `--tunnel-locks=<path>` | `/var/lib/mygrokd/tunnellocks.json` | Per-tunnel `allowed_users`. |
| `--invites=<path>` | `/var/lib/mygrokd/invites.json` | Pending and consumed invites. |
| `--lan=<bool>` | `false` | Enable [LAN-direct](architecture.md#lan-direct). Requires `--dns-provider`. |
| `--version` | | Print version and exit. |

Without `--https`, mygrokd serves plain HTTP only — fine behind another
terminating proxy, wrong on the open internet.

---

## Environment variables

| | |
|---|---|
| `MYGROK_AUTHTOKEN` | Shared auth token. Read by both client and server. |
| `MYGROK_SERVER` | Default server address for the client (`host:port`). |
| `MYGROK_PUBLIC_HOST` | Server's public host, instead of `--public-host`. |
| `MYGROK_INSTALL_DIR` | Where the install script puts the binary. Default `/usr/local/bin`. |
| `NO_COLOR` | Set to anything to disable coloured request logs. |
| `AWS_*` / `CLOUDFLARE_API_TOKEN` / `DO_AUTH_TOKEN` | DNS provider credentials. See [certificates](server.md#certificates). |

The client also reads `~/.mygrok/authtoken` (make it `0600`) when no flag or
environment variable supplies a token.

---

## Exit codes

| | |
|---|---|
| `0` | Success |
| `1` | Runtime failure — bad config, unreachable server, missing file |
| `2` | Bad arguments; usage was printed |
| `3` | `mygrok admin` only: the server returned an error |

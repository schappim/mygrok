# Architecture

- [The shape of it](#the-shape-of-it) · [Protocol](#protocol)
- [Resilient reconnect](#resilient-reconnect) · [Real-IP forwarding](#real-ip-forwarding)
- [Custom hostnames (CNAME)](#custom-hostnames-cname) · [LAN-direct](#lan-direct)
- [Repo layout](#repo-layout)

---

## The shape of it

```
                        Public                                Private
                        ──────                                ───────

  curl https://foo.example.com
              │
              │  HTTPS
              ▼
   ┌──────────────────────┐                            ┌────────────────┐
   │   mygrokd            │   yamux stream over TCP    │   mygrok       │
   │                      │ ◄────────────────────────► │  (your laptop) │
   │  :443  public HTTPS  │       (control conn :7000) │                │
   │  :80   public HTTP   │                            │                │
   │  :7000 tunnel ctrl   │                            └───────┬────────┘
   └──────────────────────┘                                    │
                                                               ▼
                                                       127.0.0.1:3000
                                                       (your local app)
```

**`mygrokd`** listens on three ports:

- **`:7000`** — the control plane. Clients dial in, send a JSON handshake,
  and the connection is then wrapped in [yamux](https://github.com/hashicorp/yamux).
- **`:80` and `:443`** — public traffic. Each accepted connection has its
  `Host` header peeked, the subdomain looked up in the registry, and the
  connection full-duplex copied through a fresh yamux stream to the matching
  client.

**`mygrok`** dials the server, registers a subdomain, and for each stream the
server pushes, dials `localhost:<port>` and copies bytes both ways.

There is **no HTTP parsing past the Host line**. Headers, bodies, chunked
encoding, WebSocket frames — all raw TCP. The only hard requirement is
HTTP/1.1, which is why ALPN advertises `http/1.1` and nothing else:
advertising `h2` would make browsers send HTTP/2 frames that local backends
can't decode.

`tunnel.<publicHost>` is special-cased and never routes to a tunnel. It
serves the install script, the embedded client binaries, the landing page,
and the admin UI (`cmd/mygrokd/admin.go`).

### One connection per client

A client holds exactly one TCP connection to the server. Every concurrent
request rides its own yamux stream over that connection. This is what makes
a single NAT hole-punch enough, and why a client behind a home router doesn't
need any port forwarding.

Streams are opened by the server (one per public request) and accepted by the
client. Either side *can* open them; in practice only the server does.

### One local connection per request

The client deliberately dials a **fresh** local TCP connection per request
rather than reusing one across pipelined requests on a stream. Local servers
close idle keep-alive sockets on their own schedule — Puma's
`persistent_timeout` defaults to 20 seconds — and reusing one would race the
FIN, surfacing to the visitor as a spurious "unexpected EOF" or connection
reset.

On a `101 Switching Protocols`, framing stops mattering: the client forwards
the 101 and switches to pumping raw bytes in both directions until either
side half-closes.

---

## Protocol

Defined in [`internal/proto/proto.go`](../internal/proto/proto.go). Two JSON
lines, then yamux.

**Client → server:**

```json
{
  "version": "1",
  "auth": "<token>",
  "subdomain": "foo",
  "proto": "http",
  "client_id": "<128-bit hex>",
  "hostnames": ["app.example.com"],
  "lan_ip": "192.168.1.50",
  "lan_port": 8443
}
```

`client_id` is random per process — see [resilient reconnect](#resilient-reconnect).
`hostnames`, `lan_ip`, and `lan_port` are optional.

**Server → client:**

```json
{
  "ok": true,
  "url": "https://foo.example.com",
  "urls": ["https://foo.example.com", "http://foo.example.com"]
}
```

…or:

```json
{ "ok": false, "error": "subdomain in use" }
```

When LAN-direct is negotiated the response also carries `lan_hostname`,
`lan_cert_pem`, and `lan_key_pem`.

After that line both sides switch **the same TCP connection** to yamux — the
server runs `yamux.Server`, the client `yamux.Client`. Both configure
`KeepAliveInterval=10s` and `ConnectionWriteTimeout=5s`, tighter than the
library defaults of 30s/10s, so a dead peer is detected in about 15 seconds
instead of 40. The underlying socket also has `SO_KEEPALIVE` with a 15-second
probe interval, as a backstop for the case where yamux's own pings are stuck
behind a blocked write.

---

## Resilient reconnect

When a WAN link flaps or a router's NAT entry expires, the server can still
be holding a `yamux.Session` for a subdomain whose client is gone. Naively,
the reconnecting client gets `subdomain in use` and burns 30–60 seconds in
backoff waiting for the corpse to time out.

Client and server cooperate to avoid that:

1. The client generates a random `client_id` at startup and sends it in every
   `Hello`.
2. `registry.claim()` matches on it. Same `client_id` as the current holder →
   the old session is closed and replaced atomically. A *different*
   `client_id` still gets `subdomain in use`, so two genuinely different
   clients can't evict each other.
3. `registry.release()` is fenced: a goroutine whose session was pre-empted
   can't clobber the new owner's registration when its `defer` finally runs.
4. When the server calls `session.Open()` for a public request and gets an
   error, the dead session is evicted immediately rather than lingering until
   the next failed heartbeat.
5. On the client, `subdomain in use` triggers a 1-second-plus-jitter retry
   rather than the 2–30s exponential backoff, which is meant for "server is
   down", not "server says wait". After 60 seconds of healthy session the
   backoff counter resets, so a long-lived tunnel doesn't keep climbing.

Most blips now heal in under a second. The worst case — a quietly dead peer
with nothing competing for the name — drops from ~40s to ~15s.

---

## Real-IP forwarding

Before passing a public request to the tunnel, the server rewrites the raw
header block: it **strips** any inbound `X-Forwarded-For` and `X-Real-IP`
(the public internet is not a trusted proxy) and **injects** values from
`conn.RemoteAddr()`.

Your app's `request.remote_ip` is therefore accurate, rate limiting keyed on
client IP works, and the client's request log shows the real visitor rather
than a dash.

---

## Custom hostnames (CNAME)

A tunnel lives at `<subdomain>.<publicHost>`. To also serve it from a domain
you own:

```bash
mygrok http 3000 --subdomain=jarvis --hostname=app.example.com
```

At your DNS provider, point the name at the tunnel server:

```
app.example.com.  CNAME  tunnel.example.com.
# or, where CNAME isn't allowed (apex records):
app.example.com.  A      203.0.113.10
```

The first visit to `https://app.example.com` triggers on-demand certificate
issuance via certmagic — Let's Encrypt validates over TLS-ALPN-01, which
rides the existing `:443` handshake. (HTTP-01 is disabled: the `:80`
listener does its own Host-header routing and never serves
`/.well-known/acme-challenge`, so an HTTP-01 attempt could only fail.)
Later visits use the cached certificate, renewed like any other.

### How issuance is gated

Anyone can point a CNAME at your server. To stop scanners farming free
certificates, `OnDemand.DecisionFunc` only allows names that are *currently
registered* by a live tunnel:

1. Client sends `--hostname=app.example.com` in its `Hello`.
2. Server validates it — DNS-shaped, not under `publicHost`, not already
   claimed by a different tunnel — and records it in `registry.customHosts`.
3. First TLS handshake for that name asks `DecisionFunc`; the registry says
   yes; certmagic issues and caches.
4. Requests route through `registry.lookup(host)` to the right session.
5. On disconnect, `release()` removes the mapping.

Several at once:

```bash
mygrok http 3000 --subdomain=jarvis --hostname=app.example.com,api.example.com
```

### Caveats

- DNS must resolve to the server *before* the first HTTPS request —
  TLS-ALPN-01 requires Let's Encrypt to reach you at that name on `:443`.
  Until then, requests 502.
- Hostnames under your own `publicHost` are rejected; those belong to
  `--subdomain`.
- Custom hostnames don't combine with the [passkey gate](access-control.md#passkey-lockdown)
  or [LAN-direct](#lan-direct) — both depend on cookies and certificates
  scoped to the wildcard zone, and a CNAME is a different origin.

---

## LAN-direct

When the client and a visitor are on the same LAN, sending every byte to a
server in another city is wasted latency. LAN-direct short-circuits it:
same-LAN visitors are redirected to a sister hostname whose **public** DNS A
record points at the client's private IP, and the client serves them directly
using the wildcard certificate the server shipped down at connect time.

Off-LAN visitors go through the tunnel exactly as before.

It's opt-in on both sides — `--lan` on the client, `--lan=true` and a
`--dns-provider` on the server.

```bash
mygrok http 3000 --subdomain=jarvis --lan=auto
mygrok http 3000 --subdomain=jarvis --lan=192.168.1.50   # pin it
```

`auto` picks the first RFC1918 IPv4 on a non-loopback interface that's up.
The banner gains a line:

```
  Forwarding   https://jarvis.example.com               -> 127.0.0.1:3000
               http://jarvis.example.com                -> 127.0.0.1:3000
  LAN-direct   https://jarvis-lan.example.com:8443      -> 127.0.0.1:3000 (same-LAN bypass)
```

### At handshake time

1. Client puts `lan_ip` and `lan_port` in its `Hello`.
2. Server checks the IP is RFC1918, refuses any subdomain ending in `-lan`
   (it would collide with the sister-hostname pattern), and writes
   `<sub>-lan.<publicHost> → <lan_ip>` with a 60-second TTL through the
   configured DNS provider.
3. Server reads the wildcard certificate and key from `--cert-dir` and sends
   them in the response.
4. Client opens a TLS listener on `<lan_ip>:<lan_port>` and proxies to the
   same local backend.
5. On disconnect — clean exit, takeover, or a crash that severs yamux — the
   server best-effort deletes the record so nobody is left pointed at a dead
   address.

### At request time

1. A visitor whose public IP matches the client's gets a `307 Temporary
   Redirect` to `https://<sub>-lan.<publicHost><path>`. 307 preserves method
   and body, so POSTs survive.
2. Public DNS returns the client's private IP.
3. The browser connects to it and validates a real Let's Encrypt certificate.
   No warnings, no `/etc/hosts`, no local CA.
4. Subsequent navigation stays on the LAN URL.

Escape hatch: append `?nolan=1` to force the public path.

### Trade-offs

- **Every connected client holds a copy of the wildcard certificate and
  key.** Fine if you trust every machine you've installed `mygrok` on;
  not fine if `MYGROK_AUTHTOKEN` has been shared beyond people you'd give a
  certificate to.
- **Your private IP is in public DNS** while a tunnel is up. Mostly harmless
   — RFC1918 addresses leak from mail headers and logs constantly — but it
  does publish topology.
- **CGNAT breaks the premise.** If your ISP shares your "public" IP with
  thousands of others, you'd redirect strangers to a LAN URL they can't
  reach. This is the main reason it's opt-in.
- **IPv4 only.**
- **Subdomains can't end in `-lan`** while LAN-direct is on.
- **Server-side kill switch:** `--lan=false` on `mygrokd` refuses it globally
  regardless of what clients ask for.

---

## Repo layout

```
mygrok/
├── cmd/
│   ├── mygrok/                 client
│   │   ├── main.go             dispatch, `http`, reconnect loop, request log
│   │   ├── serve.go            `serve` static file server
│   │   ├── mcp.go              `mcp` capability-URL gate
│   │   ├── admin.go            `admin` CLI + HTTP API client
│   │   ├── service.go          launchd + systemd install/uninstall
│   │   ├── update.go           `update` self-update
│   │   ├── config.go           .mygrok.toml lookup, server resolution
│   │   ├── basicauth.go        constant-time basic auth
│   │   └── lan.go              LAN-direct TLS listener
│   └── mygrokd/                server
│       ├── main.go             listeners, routing, TLS, registry, header injection
│       ├── admin.go            management host: install, /dl, landing, admin UI
│       ├── auth.go             admin sessions, passkey gate wiring
│       ├── dnsprovider.go      --dns-provider → libdns provider
│       ├── ipacl.go            allow/block lists
│       ├── passkeys.go         WebAuthn users, credentials, sessions
│       ├── tunnellocks.go      per-tunnel allowed_users
│       ├── invites.go          single-use invite tokens
│       ├── lan.go              LAN-direct DNS records + cert reading
│       └── embed/clients/      client binaries (//go:embed target)
├── internal/
│   ├── proto/                  JSON handshake messages
│   ├── branding/               default favicon, shared by both binaries
│   └── buildinfo/              link-time version + default server
├── deploy/                     server bootstrap, systemd unit, cloud-init
├── docs/                       these files, screenshots, terminal renders
├── skills/mygrok/              Claude Code skill
├── examples/                   sample .mygrok.toml files
└── build.sh                    cross-compile, embed, install, deploy
```

The server embeds all four client binaries so `/install` and
`/dl/mygrok-<os>-<arch>` work with no external storage. That's why
`build.sh` builds clients *before* the server, and why a fresh checkout has
only a `.keep` in `embed/clients/` — enough for `go build ./...` to succeed
without them.

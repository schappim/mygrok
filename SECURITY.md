# Security

## Reporting a vulnerability

Email **marcus@schappi.com** with "mygrok security" in the subject, or open a
[private security advisory](https://github.com/schappim/mygrok/security/advisories/new).
Please don't open a public issue for anything exploitable.

Include what you can: what you did, what happened, and what an attacker gets
out of it. A proof of concept helps a lot. I'll acknowledge within a few days
and keep you posted on the fix.

This is a small project maintained by one person. There's no bounty, but
you'll get credit in the release notes if you want it.

## What mygrok is, security-wise

mygrok punches a hole from the public internet to a port on your machine.
That's the entire point of it, and it's worth being clear-eyed about what
that means.

**The auth token is the whole front door.** Anyone holding it can register
tunnels on your server. It's a shared secret, not per-user credentials —
rotate it in `/etc/mygrokd/mygrokd.env` and every client's config if you
think it's leaked.

**A tunnel is public by default.** Anyone who guesses or discovers the URL
reaches your local app, with no authentication in front of it. Three
mechanisms exist to change that, and they compose:

| | Good for |
|---|---|
| `--basic-auth=user:pass` | A quick shared password. Enforced by the client, before your app sees the request. |
| [IP allow/block lists](docs/access-control.md#ip-access-control) | Fixed offices, known ranges, kicking out a scanner. Enforced at the server edge. |
| [Passkeys](docs/access-control.md#passkey-lockdown) | Real per-person access with WebAuthn. Per-tunnel grants, revocable per device. |

**`mygrok mcp` uses capability URLs.** The token in the path *is* the
credential. Anything that logs, proxies, or shoulder-surfs that URL has full
access. Treat it like a password.

**Your local app sees a public request.** Whatever you're forwarding is now
internet-facing. A development server with debug endpoints, an unauthenticated
admin panel, a database GUI — all reachable. mygrokd rewrites `X-Forwarded-For`
and `X-Real-IP` with the true client IP so your app's rate limiting and IP
logic work correctly, but it doesn't otherwise filter anything.

**The control plane on `:7000` is not encrypted.** This is the most
important thing on this page. The client opens a plain TCP connection and
sends the shared auth token as JSON before anything else, on every connect
and every reconnect. After the server terminates TLS for a public request,
it re-emits that request — headers, cookies, body — over the same plaintext
socket to the client.

So anyone who can observe the path between a client and the server sees the
auth token (which is also the admin credential) and all tunnelled traffic.
Over the open internet that means your hosting provider's network and every
transit AS in between; on a café Wi-Fi it means anyone on that Wi-Fi.

Encrypting the control plane is the top item on the roadmap. Until it ships,
treat `:7000` as you would any unencrypted protocol: fine over links you
trust, not fine from a network you don't.

## What we do about it

- **HTTPS for visitors.** Certificates come from Let's Encrypt
  automatically, on `:443`, with a plaintext `:80` that 308s the management
  host to HTTPS and sets HSTS. This covers the visitor↔server hop only; see
  the control-plane note above for the server↔client hop.
- **On-demand issuance is gated.** When a wildcard covers the zone, names
  under it are served from that one cached certificate. When there is no
  wildcard (`--dns-provider=none`), a name only gets an ACME order if a
  tunnel is actually registered for it — so a scanner sweeping `:443` with
  invented SNI can't burn through your Let's Encrypt quota.
- **Constant-time comparison** for the tunnel auth token, admin token, MCP
  capability token, and session IDs.
- **Cross-site protection on admin mutations.** POSTs carrying an `Origin`
  or `Referer` from anywhere but the management host are rejected — HTTP
  Basic credentials are replayed by browsers regardless of `SameSite`, so
  the cookie's own protection isn't enough on its own.
- **mygrok's session cookies are stripped** before a request is forwarded
  into a tunnel. They're zone-scoped, so a tunnel operator who could read
  one would hold a credential good against other people's tunnels.
- **No third-party requests from any served page.** The landing page, the
  offline page, and the admin UI use system fonts. Nothing a visitor loads
  contacts anyone but you.
- **Secrets stay out of world-readable files.** The server's token lives in a
  `0600 root:root` environment file. `mygrok service install` on Linux does
  the same — the token goes to `/etc/mygrok/<name>.env`, never into the unit
  file, because `systemctl cat` will show a unit to any user.
- **Unprivileged by default.** The server runs as its own user with exactly
  one writable directory and `CAP_NET_BIND_SERVICE` for the low ports.
- **Reserved subdomains.** `www`, `api`, `admin`, and `mail` can't be
  claimed, so a tunnel can't shadow infrastructure on the same zone.
- **The management host is exempt from IP rules**, so a bad rule can't lock
  you out of the admin UI that would let you fix it.

## Known limitations

These are design trade-offs, documented rather than hidden:

- **The control plane is plaintext.** See above. This is the one that would
  surprise people most, so it's stated twice on purpose.
- **One shared token, no per-client identity.** Every client with the token
  is equally trusted. There's no per-client revocation short of rotating it
  for everyone.
- **LAN-direct hands every client the wildcard certificate's private key.**
  That's how a client can serve `<sub>-lan.<publicHost>` from your LAN with
  a real certificate and no browser warning. It means any machine holding
  the shared token can obtain a key valid for your entire zone. `--lan` is
  off by default on both sides for this reason; only turn it on if you'd
  hand every one of those machines that key deliberately.
- **The LAN-direct listener enforces none of the gates.** It proxies
  straight to your backend, so it sees neither the server's IP rules nor its
  passkey gate. mygrok refuses `--lan` together with `--basic-auth` or MCP
  mode outright, but an IP-restricted or passkey-locked tunnel with `--lan`
  on still has an unguarded door for anyone sharing the visitor's public IP.
- **On-demand mode publishes every tunnel name to Certificate Transparency.**
  With `--dns-provider=none`, each subdomain gets its own certificate, and
  every issued certificate is logged publicly and permanently. If tunnel
  names shouldn't be world-readable, configure a DNS provider and use the
  wildcard, which discloses only `*.<your-domain>`.
- **Passkey sessions don't follow custom hostnames.** The session cookie is
  scoped to the wildcard zone, so a passkey-locked tunnel reached through a
  `--hostname=` CNAME is a different origin and won't carry the cookie.
  Don't combine `--hostname=` with the passkey gate.
- **`mygrok service install` stores credentials on disk.** Unattended
  restarts need them. The auth token and any `--basic-auth` go into a
  `0600 root:root` environment file on Linux and the `0600` plist's
  environment on macOS — never into argv, and never into the world-readable
  systemd unit — but they are on disk.
- **LAN-direct publishes a public DNS record for a private IP.** That's how
  the same-NAT bypass works. The record is validated to be RFC1918, but it
  does tell anyone who looks what your internal addressing is.
- **No audit log.** Request logs go to the client's stdout. There's no
  server-side record of who reached which tunnel when.

## Supported versions

The latest release gets security fixes. Given the size of the project,
please upgrade rather than expecting backports.

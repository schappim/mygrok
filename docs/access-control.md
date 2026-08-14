# Access control

A tunnel is **public by default**. Anyone with the URL reaches your local
app. That's deliberate — you want `mygrok serve ./photos` to just work — but
it means anything sensitive needs one of these in front of it.

Three mechanisms, and they stack:

| | Enforced by | Good for |
|---|---|---|
| [Basic auth](#basic-auth) | The client, before your app | A quick shared password |
| [IP rules](#ip-access-control) | The server edge, before the tunnel | Fixed offices, known ranges, evicting a scanner |
| [Passkeys](#passkey-lockdown) | The server edge, before the tunnel | Real per-person access, revocable per device |

---

## Basic auth

```bash
mygrok http 3000 --subdomain=jarvis --basic-auth=alice:s3cret
mygrok serve gallery ./photos --basic-auth=alice:s3cret
mygrok service install 3000 --subdomain=jarvis --basic-auth=alice:s3cret
```

The client checks the `Authorization` header and returns
`401 WWW-Authenticate: Basic realm="mygrok"` before dialling your backend.
Constant-time comparison, and the credentials never leave your machine — the
server has no idea the tunnel is protected.

Unauthorised requests tear down the stream rather than draining a possibly
large request body. Browsers immediately retry with credentials, so the
practical effect is invisible.

Good for a demo link you're sending to one person. Not good for anything
where you'd care who used it — there's one password, no revocation, and no
record of who logged in.

---

## IP access control

Per-tunnel allow and block lists, plus global rules across every tunnel.
Empty lists mean **open**, so adding a rule to one tunnel doesn't change the
others.

The management host (`tunnel.<publicHost>`) is exempt from all of it. You
cannot lock yourself out of the admin UI with a bad rule, and `/install`
stays reachable from a new machine.

### From the CLI

```bash
mygrok admin allow jarvis 203.0.113.7 --note=office
mygrok admin block global 198.51.100.0/24 --note=scanner
mygrok admin unallow jarvis 203.0.113.7
mygrok admin rules --json | jq
```

`<scope>` is `global` or a subdomain. Entries are single IPs
(`203.0.113.7`, `2001:db8::1`) or CIDR (`10.0.0.0/8`, `2001:db8::/64`).

### From the web UI

```
https://tunnel.<your-domain>/admin/ips
```

![The IP access control overview: global blocklist entries above a table of live tunnels with their per-tunnel rule counts](screenshots/admin-overview.png)

`mygrok admin` builds `…/admin/ips?key=<your-token>` and opens it. The server
validates the key, sets a 24-hour `mygrok_admin` session cookie
(HttpOnly, Secure, SameSite=Lax), and redirects to a clean URL — so the token
appears in browser history for exactly one hop. Sessions live in memory;
restarting `mygrokd` invalidates them all.

`mygrok admin --open=false` prints the URL instead of opening it, for when
you're on a box without a browser.

You can also visit the page directly and let the browser prompt for basic
auth — any username, password is `MYGROK_AUTHTOKEN`.

**`/admin/ips`** is the overview: global lists on top, then every tunnel that
is live or merely has rules configured. The form at the bottom jumps to any
subdomain you type, so you can write rules before a tunnel has ever
connected.

**`/admin/ips/<subdomain>`** is that tunnel's own lists. Changes here never
touch the global rules.

![Per-tunnel rules for the staging tunnel, showing an allowlist with two annotated entries and the passkey gate above it](screenshots/admin-tunnel.png)

### Evaluation order

First match wins:

1. **Global blocklist hit** → 403. Banned everywhere.
2. **Per-tunnel blocklist hit** → 403.
3. **Per-tunnel allowlist non-empty and no match** → 403. A tunnel's own
   allowlist *replaces* the global one rather than adding to it.
4. **Per-tunnel allowlist empty, global allowlist non-empty, no match** →
   403. This is how you get a default-deny posture.
5. Otherwise → allow.

Blocklist always beats allowlist: listing an address in both, at any scope,
rejects it.

The check uses the connection's `RemoteAddr()`, not `X-Forwarded-For` —
mygrokd is the public edge and there is no upstream proxy to trust.

It also runs **before** basic auth and before the request crosses the tunnel,
so a blocked visitor gets a 403 straight from the server without touching
your app or costing you tunnel bandwidth.

### Storage

`/var/lib/mygrokd/iplist.json` by default (`--ip-list=`):

```json
{
  "global": {
    "blocked": [{ "raw": "198.51.100.0/24", "note": "scanner range" }]
  },
  "tunnels": {
    "jarvis": {
      "allowed": [
        { "raw": "203.0.113.7", "note": "office" },
        { "raw": "10.0.0.0/8" }
      ]
    },
    "admin-app": {
      "allowed": [{ "raw": "203.0.113.7" }],
      "blocked": [{ "raw": "10.20.30.40" }]
    }
  }
}
```

Missing file means empty lists, which means allow everything. Hand-editing
works; mygrokd reloads on restart. Older flat
`{"allowed":[…],"blocked":[…]}` files are migrated into the `global` bucket
on first load and rewritten in the new shape.

### Notes

- Custom hostnames route by the registered subdomain, so a rule on `jarvis`
  applies whether the request arrives at `jarvis.example.com` or at a
  `--hostname=app.example.com` CNAME.
- A tunnel with no rules and no global allowlist accepts everyone. Adding a
  per-tunnel allowlist locks down *that* tunnel only.

---

## Passkey lockdown

Multi-user WebAuthn with per-tunnel grants. Different people get different
tunnels, and you add or remove someone without touching anyone else's setup.

### Onboarding, end to end

```bash
# 1. Issue a single-use invite tagged with their name.
mygrok admin invite alice
# → https://tunnel.example.com/invite/<token>

# 2. Send Alice that URL. She opens it, her browser offers Touch ID /
#    her phone / a hardware key, and a user "alice" is created with her
#    first credential bound to it.

# 3. Give her the tunnels she should reach.
mygrok admin grant jarvis alice
mygrok admin grant admin-app alice
```

From then on, any browser she's signed in on opens those tunnels straight
through. Everything else is unaffected.

![The passkey invites page, showing a pending single-use invite for alice with created and expiry timestamps](screenshots/admin-invites.png)

Taking access away:

```bash
mygrok admin revoke jarvis alice    # one tunnel
mygrok admin users delete alice     # the user and all their credentials
```

### What a visitor sees

1. Browser hits `https://jarvis.example.com/anything`. mygrokd finds no valid
   `mygrok_pk` cookie — or finds one whose user isn't on this tunnel's
   `allowed_users`.
2. `302 → https://tunnel.example.com/auth?return=<original-url>`.
3. The login page runs a *discoverable* WebAuthn assertion: the browser
   offers whichever passkeys exist for this domain, they pick one.
4. The server identifies the user from the credential ID and sets:
   ```
   Set-Cookie: mygrok_pk=<sid>; Domain=.example.com; Path=/;
               Max-Age=86400; HttpOnly; Secure; SameSite=Lax
   ```
5. Back to the original URL. The gate now sees a known user, confirms they're
   on the allow list, and lets the request through.

The cookie is scoped to your whole zone, so signing in once covers every
tunnel they've been granted.

### Invites

- Single-use, 7-day expiry.
- The token is 24 random bytes.
- Redeeming atomically creates the user, registers their first credential,
  consumes the invite, and issues a session — so they land signed in.
- Pending and consumed invites are listed at `/admin/invites` or via
  `mygrok admin invites`.

### Extra devices, self-serve

Once someone has one passkey, they add more themselves at:

```
https://tunnel.<your-domain>/account
```

It shows their current credentials and a "register another device" form that
runs the WebAuthn create ceremony against their existing user record. No
admin involvement, no second invite. They can delete old credentials there
too — the UI refuses to delete the last one, so nobody locks themselves out.

The sign-in page mentions `/account` so people discover it exists.

If someone loses every device they'd registered, self-serve can't help: an
admin has to `mygrok admin users delete <name>` and issue a fresh invite.

### The safety net

If **zero** credentials are registered anywhere, the gate is bypassed even on
a locked tunnel. A misclicked *Lock*, or a `grant` naming a user who hasn't
redeemed their invite yet, therefore can't lock you out before any passkey
exists.

### CLI

```
mygrok admin invite  <name>
mygrok admin invites [revoke <token>]
mygrok admin users   [delete <id-or-name>]
mygrok admin grant   <subdomain> <id-or-name>
mygrok admin revoke  <subdomain> <id-or-name>
mygrok admin lock    <subdomain>            grant every existing user
mygrok admin unlock  <subdomain>            drop all grants
mygrok admin passkeys [list]
mygrok admin passkeys delete <id>
```

All of these take `--json`. Registration itself isn't scriptable — WebAuthn
needs a browser and an authenticator — but every administrative action is.

### Storage

Three JSON files beside `iplist.json`, all configurable:

| | |
|---|---|
| `/var/lib/mygrokd/passkeys.json` | Users and credentials. `0600`, public keys only. |
| `/var/lib/mygrokd/tunnellocks.json` | Per-tunnel `allowed_users`. |
| `/var/lib/mygrokd/invites.json` | Pending and consumed invites. `0600`. |

Single-user `passkeys.json` and `{"locked":[…]}` `tunnellocks.json` files
from older versions migrate on first load: credentials get rebound to a
legacy `owner` user, and locks become entries with empty allow lists.
mygrokd logs one warning per migrated lock so you know to re-run
`grant <sub> <user>`.

### Caveats

- **The cookie is scoped to `.<publicHost>`**, so it covers your wildcard
  zone but not custom hostnames. A passkey-locked tunnel reached via
  `--hostname=app.example.com` is a different origin: the visitor would be
  redirected to `/auth`, sign in successfully, and come back without the
  cookie. Don't combine `--hostname=` with the passkey gate.
- **WebAuthn needs HTTPS.** Locking a tunnel does nothing if you're testing
  over plain HTTP.
- **Sessions live in process memory.** Restarting `mygrokd` signs everyone
  out. That's intentional — it's the kill switch. Credentials and grants
  persist; only sessions reset.

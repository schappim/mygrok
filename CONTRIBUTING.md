# Contributing

Bug reports, fixes, and provider support are all welcome. This is a small
project; a short issue describing what you hit beats a long one describing
what you'd like.

## Getting set up

```bash
git clone https://github.com/schappim/mygrok.git
cd mygrok
go build ./...
go test ./...
```

Go 1.25 or newer. No other dependencies, no code generation, no build tags.

## Running the whole thing locally

You don't need a server or a domain to develop against. `*.localhost`
resolves to loopback in every modern browser, which is enough for a
complete stack:

```bash
export MYGROK_AUTHTOKEN=dev-token

# Terminal 1 — the server
go run ./cmd/mygrokd \
  --public-host=localhost --http=:8088 --https="" --tunnel=:7100 \
  --cert-dir=/tmp/mygrok-dev/certs \
  --ip-list=/tmp/mygrok-dev/iplist.json \
  --passkeys=/tmp/mygrok-dev/passkeys.json \
  --tunnel-locks=/tmp/mygrok-dev/locks.json \
  --invites=/tmp/mygrok-dev/invites.json

# Terminal 2 — a tunnel
go run ./cmd/mygrok serve demo ./some-dir --port=9000 --server=127.0.0.1:7100
```

Then:

- `http://demo.localhost:8088/` — through the tunnel
- `http://tunnel.localhost:8088/` — the landing page
- `http://tunnel.localhost:8088/admin/ips?key=dev-token` — the admin UI
- `http://nothing.localhost:8088/` — the offline page

Passkeys need a secure context, so WebAuthn flows won't work over plain
HTTP on a non-`localhost` origin. Everything else does.

## Tests

```bash
go test ./...                       # everything
go test ./cmd/mygrokd/ -run TestIPACL -v
go test ./... -race                 # before anything touching concurrency
```

The suite is table-driven and hermetic — no network, no fixed ports, temp
dirs via `t.TempDir()`. Please keep it that way; a test that needs the
internet will fail in CI and on someone's plane.

New behaviour should come with a test. Look at `cmd/mygrokd/ipacl_test.go`
for the house style: a slice of cases, one loop, failure messages that say
what was expected and what happened.

## Style

Follow the code that's already there. Specifically:

- `gofmt` (CI enforces it). `go vet ./...` must be clean.
- **Comments explain why, not what.** The existing comments are load-bearing
  — they record the reason a thing is the way it is, usually because the
  obvious approach was tried and broke. If you're removing one, make sure
  the reason is gone too.
- Errors say what to do next. `--subdomain is required (pass on CLI or set
  subdomain = ... in .mygrok.toml)` beats `invalid argument`.
- No new dependencies without a reason worth stating in the PR.

## Things that would genuinely help

- **More DNS providers.** `cmd/mygrokd/dnsprovider.go` is a switch over
  [libdns](https://github.com/libdns) providers. Adding Gandi, Namecheap, or
  Hetzner DNS is a dozen lines plus a test.
- **`armv7` client builds** for older Raspberry Pis. The installer explicitly
  rejects them today.
- **IPv6 for LAN-direct.** Currently IPv4-only, and it says so.
- **A `--hostname=` + passkey story.** They don't work together because the
  session cookie can't span origins. Solving it properly probably means a
  token hand-off on the redirect.

## Pull requests

1. Branch off `main`.
2. `go test ./...` and `gofmt -l .` clean.
3. One logical change per PR. A refactor and a fix in one diff is two PRs.
4. In the description: what broke, or what this makes possible. Include the
   command you ran to see it working.

Commit messages in the imperative — "Add Hetzner DNS provider", not "added"
or "adding".

## Releases

Tagging `v*` builds cross-platform binaries and publishes a GitHub release.
The Homebrew formula points at the release tarball and needs its `sha256`
bumped afterwards.

```bash
git tag -a v1.2.0 -m "v1.2.0"
git push origin v1.2.0
```

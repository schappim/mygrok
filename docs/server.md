# Run your own tunnel server

You need two things: a Linux box with a public IP, and a domain you can add
DNS records to. Any $4–6/month VPS is plenty — mygrokd idles at a few MB of
RAM and the traffic is whatever your tunnels carry.

- [The five-minute version](#the-five-minute-version)
- [Pick a box](#pick-a-box) — DigitalOcean · Hetzner · AWS · Vultr/Linode
- [DNS](#dns)
- [What the installer sets up](#what-the-installer-sets-up)
- [Certificates](#certificates)
- [Upgrading](#upgrading)
- [Uninstalling](#uninstalling)
- [Troubleshooting](#troubleshooting)

---

## The five-minute version

```bash
# On the server, as root:
curl -sSL https://raw.githubusercontent.com/schappim/mygrok/main/deploy/install-server.sh \
  | sudo bash -s -- --domain example.com --email you@example.com
```

It prints an auth token and the two DNS records to add. Add them, wait for
propagation, then on your laptop:

```bash
curl -sSL https://tunnel.example.com/install | bash
export MYGROK_AUTHTOKEN=<the-token-it-printed>
mygrok http 3000 --subdomain=app
# → https://app.example.com
```

That's the whole thing. Everything below is detail you can skip until you
need it.

### Installer options

| Flag | Default | |
|---|---|---|
| `--domain <d>` | *(required)* | Public host. Tunnels become `<sub>.<d>`. |
| `--email <e>` | *(required)* | ACME contact address. Let's Encrypt mails expiry warnings here. |
| `--token <t>` | generated | Shared auth token. Omit and one is generated for you. |
| `--dns-provider <p>` | `none` | `none`, `route53`, `cloudflare`, or `digitalocean`. See [Certificates](#certificates). |
| `--version <v>` | `latest` | Release tag to install. |
| `--binary <path>` | | Install a local binary instead of downloading. For air-gapped boxes or your own build. |
| `--from-source` | | Build with the local Go toolchain instead of downloading a release. |
| `--no-start` | | Install everything, don't start the service. |
| `--uninstall` | | Stop and remove the service. Keeps `/var/lib/mygrokd`. |

Re-running is safe: it upgrades the binary and rewrites the unit, but keeps
your token, certificates, IP rules, and passkeys.

---

## Pick a box

Requirements are the same everywhere — 512MB RAM, a public IPv4, and
inbound **80**, **443**, and **7000/tcp**.

Port 7000 is the control plane clients dial. If you'd rather not expose
another port, `--tunnel=:443` won't work (443 is the public listener), but
you can front it with anything that does TCP passthrough.

### DigitalOcean

```bash
doctl compute droplet create mygrok \
  --region syd1 \
  --image debian-12-x64 \
  --size s-1vcpu-512mb-10gb \
  --ssh-keys "$(doctl compute ssh-key list --format ID --no-header | head -1)" \
  --wait

IP=$(doctl compute droplet get mygrok --format PublicIPv4 --no-header)
ssh root@"$IP" 'curl -sSL https://raw.githubusercontent.com/schappim/mygrok/main/deploy/install-server.sh \
  | bash -s -- --domain example.com --email you@example.com'
```

DigitalOcean droplets have no firewall by default, so nothing else to open.
If you attach a cloud firewall, allow 80, 443, and 7000 inbound.

Using DigitalOcean DNS for your domain? Add `--dns-provider digitalocean`
and put a personal access token in the env file — see [Certificates](#certificates).

### Hetzner

```bash
hcloud server create --name mygrok --image debian-12 --type cx22 --location fsn1
IP=$(hcloud server ip mygrok)
ssh root@"$IP" 'curl -sSL https://raw.githubusercontent.com/schappim/mygrok/main/deploy/install-server.sh \
  | bash -s -- --domain example.com --email you@example.com'
```

Cheapest of the lot, and the CX22 is generous. Hetzner firewalls are
opt-in; if you create one, allow 80, 443, 7000.

### AWS EC2

```bash
# Security group: 80, 443, 7000 in from anywhere; 22 from your IP only.
aws ec2 create-security-group --group-name mygrok --description "mygrok tunnel server"
SG=$(aws ec2 describe-security-groups --group-names mygrok \
      --query 'SecurityGroups[0].GroupId' --output text)
for p in 80 443 7000; do
  aws ec2 authorize-security-group-ingress --group-id "$SG" \
    --protocol tcp --port $p --cidr 0.0.0.0/0
done
aws ec2 authorize-security-group-ingress --group-id "$SG" \
  --protocol tcp --port 22 --cidr "$(curl -s https://api.ipify.org)/32"

# t4g.nano is ARM and costs about as much as a coffee per month.
aws ec2 run-instances \
  --image-id resolve:ssm:/aws/service/debian/release/12/latest/arm64 \
  --instance-type t4g.nano \
  --security-group-ids "$SG" \
  --key-name your-key \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=mygrok}]'
```

Then SSH in and run the installer. On EC2 you also get the nicest
credential story for wildcard certificates: attach an IAM role with
Route 53 permissions and use `--dns-provider route53` with **no static
keys on the box at all**. See [Certificates](#certificates).

Give the instance an Elastic IP before you point DNS at it, or you'll be
redoing your records after the first stop/start.

### Vultr, Linode, Scaleway, a Raspberry Pi on your desk

Same script. Anything with systemd, a public IP, and those three ports.

A home server works if your ISP gives you a static IP (or you keep a
dynamic-DNS record fresh) and you can port-forward 80/443/7000.

---

## DNS

Two records, both pointing at the server's IP:

```
*.example.com.       60   IN   A   203.0.113.10
tunnel.example.com.  60   IN   A   203.0.113.10
```

- **`*.example.com`** is what makes arbitrary subdomains work. `app`,
  `staging`, whatever a client asks for — it all resolves to your server,
  and mygrokd routes on the `Host` header.
- **`tunnel.example.com`** is the control plane clients dial, plus the
  admin UI and installer. It needs its own record because a wildcard does
  **not** cover the name it's a sibling of — and because a specific record
  always wins over a wildcard, this one has to exist explicitly.

A 60-second TTL keeps a server move cheap. Raise it once you're settled.

### The wildcard-vs-specific rule that catches everyone

DNS gives out the most specific match. If `blog.example.com` already has an
A record pointing at your website, then `mygrok http 3000 --subdomain=blog`
will register happily on the server and receive nothing — visitors go to the
website instead. Pick a subdomain with no existing record, or use a
dedicated zone (`*.tunnels.example.com`) so tunnels can never collide with
real hosts.

mygrokd refuses `www`, `api`, `admin`, and `mail` outright for this reason.

### Using a subdomain instead of the apex

Prefer keeping tunnels off your main zone:

```
*.t.example.com.       60  IN  A  203.0.113.10
tunnel.t.example.com.  60  IN  A  203.0.113.10
```

…and install with `--domain t.example.com`. Tunnels become
`app.t.example.com`. Nothing else changes.

### Verifying

```bash
dig +short tunnel.example.com          # → your server's IP
dig +short anything-here.example.com   # → same IP, via the wildcard
```

If the second one is empty, your provider may not support wildcards on that
zone level. Cloudflare, Route 53, DigitalOcean, and Hetzner DNS all do.

### Cloudflare users: turn the orange cloud off

Set both records to **DNS only** (grey cloud), not proxied. Proxying breaks
things three ways: Cloudflare terminates TLS so Let's Encrypt validation
fails, port 7000 isn't proxied at all, and WebSocket/streaming behaviour
changes underneath you.

---

## What the installer sets up

| Path | |
|---|---|
| `/usr/local/bin/mygrokd` | The server binary. |
| `/etc/mygrokd/mygrokd.env` | Auth token and any DNS credentials. `0600 root:root`. |
| `/etc/systemd/system/mygrokd.service` | The unit. |
| `/var/lib/mygrokd/` | Certificates, IP rules, passkeys, invites. `0700`, owned by the service user. |

The service runs as an unprivileged `mygrokd` user with
`AmbientCapabilities=CAP_NET_BIND_SERVICE` — that's what lets it bind 80
and 443 without root. (A `setcap` on the binary would *not* work here:
`NoNewPrivileges=true` makes the kernel ignore file capabilities.)

It's further confined with `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp`, and a `ReadWritePaths` of exactly one directory.

### Useful commands

```bash
systemctl status mygrokd
journalctl -u mygrokd -f
systemctl restart mygrokd
```

---

## Certificates

mygrokd gets TLS certificates from Let's Encrypt automatically. There are
two modes, and the right one depends on whether you want to hand over DNS
credentials.

### On-demand (default, no credentials)

`--dns-provider=none`. The first HTTPS request to a new subdomain triggers a
TLS-ALPN-01 challenge, which works because the name already resolves to
this box. The certificate is cached and renewed automatically.

- **Pro:** zero configuration, no cloud credentials anywhere.
- **Con:** roughly a one-second stall on a subdomain's first HTTPS request,
  and Let's Encrypt's rate limits apply per registered domain (50 new
  certificates per week at time of writing). Fine for a handful of stable
  tunnels; not fine if you spin up dozens of throwaway subdomains a day.
- **Also:** every certificate issued is published to public Certificate
  Transparency logs, permanently. In this mode that means every tunnel name
  you ever use becomes world-readable. The wildcard below discloses only
  `*.example.com`.

Issuance is gated on a live registration: a name only gets an ACME order if
a tunnel is actually connected for it, so a scanner sweeping `:443` with
invented SNI can't exhaust your quota.

### Wildcard (one cert covers everything)

Set `--dns-provider` to a provider that hosts your zone. mygrokd solves
DNS-01, gets a single `*.example.com` certificate, and every tunnel — new
ones included — serves TLS instantly with no per-name ACME traffic and no
rate-limit exposure.

Credentials go in `/etc/mygrokd/mygrokd.env` (root-only), then
`systemctl restart mygrokd`.

**Route 53**

```
AWS_ACCESS_KEY_ID=AKIA...
AWS_SECRET_ACCESS_KEY=...
AWS_REGION=ap-southeast-2
```

On EC2, skip the keys entirely and attach an instance role — the AWS SDK
picks it up. Either way the policy needs `route53:ChangeResourceRecordSets`
and `route53:ListResourceRecordSets` on your hosted zone, plus
`route53:GetChange` and `route53:ListHostedZonesByName` on `*`.

**Cloudflare**

```
CLOUDFLARE_API_TOKEN=...
```

Create the token at *My Profile → API Tokens* with **Zone → DNS → Edit**,
scoped to just this zone.

**DigitalOcean**

```
DO_AUTH_TOKEN=...
```

A personal access token with write scope.

### Switching later

Add credentials, change `--dns-provider=` in the unit (or re-run the
installer with the new flag), restart. Existing certificates keep working;
the wildcard is picked up on the next renewal cycle.

### Testing without burning rate limits

Add `--cert-staging` to the unit's `ExecStart` to use Let's Encrypt's
staging CA. Browsers will complain about the certificate — that's expected,
staging isn't publicly trusted — but you can iterate freely. Remove the
flag and delete `/var/lib/mygrokd/certs` when you're done.

---

## Upgrading

### Coming from a pre-1.0 server

Three flag changes will stop an existing unit from starting. All three are
deliberate — each one used to be an implicit default that only made sense for
the single deployment this code grew up in.

**`--public-host` is now required.** It used to default to the author's own
domain. Add it explicitly, or set `MYGROK_PUBLIC_HOST` in the environment
file.

**Wildcard certificates now need `--dns-provider`.** DNS-01 used to be
hardwired to Route 53. If your unit has `--cert-domains=*.example.com,…`,
add `--dns-provider=route53` (or `cloudflare` / `digitalocean`) alongside it.
Without one, mygrokd refuses to start rather than quietly dropping to
per-hostname certificates:

```
tls setup: cert domain "*.example.com" is a wildcard, which requires DNS-01
— set --dns-provider (one of: route53, cloudflare, digitalocean)
```

Your existing `AWS_*` environment variables keep working as-is.

**`--lan` now defaults to off.** LAN-direct publishes a public DNS record
containing a private IP and hands every connected client a copy of your
wildcard certificate — reasonable when you own every machine, not a
reasonable default for everyone. If you were relying on it, add `--lan=true`.

Nothing else changes: certificates, IP rules, passkeys, invites, and grants
are all read from the same paths and formats.

### Routine upgrades

Re-run the installer. It replaces the binary and restarts, keeping your
token, certificates, and rules:

```bash
curl -sSL https://raw.githubusercontent.com/schappim/mygrok/main/deploy/install-server.sh \
  | sudo bash -s -- --domain example.com --email you@example.com
```

If you build from a checkout instead, `./build.sh` cross-compiles and ships
it over SSH:

```bash
MYGROK_HOST=tunnel.example.com MYGROK_SSH_USER=root ./build.sh
```

Clients update themselves against whatever the server is serving:

```bash
mygrok update
```

---

## Uninstalling

```bash
sudo /path/to/install-server.sh --uninstall
```

Stops and removes the service and binary, and deliberately leaves
`/var/lib/mygrokd` and `/etc/mygrokd` alone so a reinstall picks up where
you left off. Delete them yourself when you actually mean it.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `public listen: listen tcp :80: bind: permission denied` | Running outside systemd without the capability | Run under the unit, or `sudo setcap cap_net_bind_service=+ep /usr/local/bin/mygrokd` |
| `public host required: --public-host=example.com` | No `--public-host` and no `MYGROK_PUBLIC_HOST` | Pass one. Tunnel URLs are built from it. |
| Certificates never issue | Port 80/443 blocked upstream | Check the cloud firewall / security group, not just the box |
| Certificates never issue, ports open | DNS not pointing here yet | `dig +short tunnel.example.com` should be this server's IP |
| `cert domain "*.example.com" is a wildcard, which requires DNS-01` | Wildcard in `--cert-domains` with `--dns-provider=none` | Set a DNS provider, or drop the wildcard and let on-demand handle it |
| Tunnel registers but gets no traffic | A specific A record beats the wildcard | `dig +short <sub>.example.com` — if it isn't your server, that name is taken |
| `subdomain in use` | Another client holds it | Pick another name, or stop the other client |
| Client can't connect at all | Port 7000 blocked | It's a separate port from 80/443; open it explicitly |
| `--lan needs --dns-provider` | LAN-direct publishes DNS records | Configure a provider, or drop `--lan` |

Logs first, always:

```bash
journalctl -u mygrokd -n 100 --no-pager
```

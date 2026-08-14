# Config file (`.mygrok.toml`)

Drop a `.mygrok.toml` in your project (or any parent directory) and mygrok
finds it. This:

```bash
mygrok http 3000 --subdomain=jarvis --server=tunnel.example.com:7000
```

becomes this:

```bash
cd ~/projects/jarvis
mygrok
```

---

## Format

Every field is optional. Set only what you want pinned.

```toml
# .mygrok.toml
subdomain = "jarvis"
port      = 3000
host      = "127.0.0.1"                 # optional, this is the default
server    = "tunnel.example.com:7000"   # optional if your build has one stamped in
auth      = "..."                       # optional — prefer env or ~/.mygrok/authtoken
```

| Key | Type | Default | |
|---|---|---|---|
| `subdomain` | string | *(none)* | Public subdomain. Lowercase letters, digits, dashes; 63 chars max. |
| `port` | integer | *(none)* | Local port. The positional `<port>` argument still wins. |
| `host` | string | `127.0.0.1` | Local host to dial. `0.0.0.0` when forwarding from another machine. |
| `server` | string | build-time default | Tunnel control endpoint. |
| `auth` | string | *(none)* | Shared token. Prefer `MYGROK_AUTHTOKEN` or `~/.mygrok/authtoken` over committing this. |

---

## Lookup order

The client stops at the first match:

1. `--config=<path>` — explicit override; a missing file is an error.
2. `.mygrok.toml`, then `.mygrok`, in the current directory.
3. The same names in each parent directory, walking up to `/`. Same idea as
   git finding `.git`.
4. `~/.mygrok/config.toml` — your global default.

When a config is found, mygrok prints `(config: /path/to/.mygrok.toml)` to
stderr so you always know which one is in play.

---

## Precedence

Per setting, the first non-empty source wins:

```
CLI flag (--subdomain=…)          ← strongest
  environment (MYGROK_AUTHTOKEN, MYGROK_SERVER)
    config file
      built-in default            ← weakest
```

Environment sits between flags and config on purpose: a token exported in
your shell beats an `auth = "..."` in a checked-in file, which is the
behaviour you want when someone commits a placeholder.

The "built-in default" for `server` is whatever was stamped into the binary
at build time. Clients downloaded from your own `mygrokd` have their server's
address baked in; Homebrew and `go install` builds have nothing, and will
tell you so rather than dialling something arbitrary.

---

## Where to put your token

Four options, in the order most people should reach for them:

**`~/.mygrok/authtoken`** — one file, every project, nothing in your shell
history:

```bash
mkdir -p ~/.mygrok
printf '%s' '<token>' > ~/.mygrok/authtoken
chmod 600 ~/.mygrok/authtoken
```

**`MYGROK_AUTHTOKEN`** in your shell profile. Convenient, but it's in the
environment of everything you run.

**`~/.mygrok/config.toml`** — good if you're already keeping `server` there:

```toml
server = "tunnel.example.com:7000"
auth   = "<token>"
```

**`--auth=<token>`** on the command line. Fine for a one-off; it lands in
your shell history and is visible in `ps`.

Don't put it in a project's `.mygrok.toml` — that file usually gets
committed. mygrok supports it because sometimes you have a good reason, but
the default assumption is that a repo config is public.

---

## Patterns

### Global server + auth, per-project subdomain

The setup that scales past two projects. Once:

```bash
mkdir -p ~/.mygrok
cat > ~/.mygrok/config.toml <<'EOF'
server = "tunnel.example.com:7000"
auth   = "<token>"
EOF
chmod 600 ~/.mygrok/config.toml
```

Then per project:

```bash
cd ~/code/myapp
cat > .mygrok.toml <<'EOF'
subdomain = "myapp"
port      = 3000
EOF

mygrok            # → https://myapp.example.com
```

Deliberately leave `subdomain` and `port` out of the global file — set
globally, every `mygrok` in every directory would fight over the same name.

### Services read the same config

```bash
cd ~/code/myapp          # contains .mygrok.toml
mygrok service install   # port and subdomain come from the file
```

### A monorepo with several tunnels

Config lookup walks *up*, so each app can have its own:

```
~/code/platform/
├── .mygrok.toml          server + auth for everything here
├── web/.mygrok.toml      subdomain = "web",  port = 3000
└── svc/.mygrok.toml      subdomain = "svc",  port = 8080
```

`cd web && mygrok` and `cd svc && mygrok` each pick up their own file, and
both inherit the parent's server. Note that lookup stops at the *first* file
found — it doesn't merge layers — so if `web/.mygrok.toml` exists, the parent
is never read. Put shared settings in `~/.mygrok/config.toml` instead, which
is always the final fallback.

### Overriding for one run

```bash
mygrok --subdomain=myapp-preview          # different name, same config
mygrok --config=./deploy/staging.toml     # a completely different file
MYGROK_SERVER=tunnel.other.com:7000 mygrok
```

---

## Sample files in this repo

- [`.mygrok.example.toml`](../.mygrok.example.toml) — annotated per-project
  template. Copy it to `.mygrok.toml`.
- [`examples/minimal.toml`](../examples/minimal.toml) — the smallest useful
  config: subdomain and port.
- [`examples/global.config.toml`](../examples/global.config.toml) — for
  `~/.mygrok/config.toml`.

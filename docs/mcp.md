# MCP connectors

`mygrok mcp` exposes a local [MCP](https://modelcontextprotocol.io) server to
claude.ai as a custom connector — so a tool running on your laptop becomes
something Claude can call in a normal conversation.

```bash
mygrok mcp 8790 --subdomain=tools
```

```
  mygrok tunnel active

  Forwarding   https://tools.example.com                -> 127.0.0.1:8790

  MCP connector URL (the URL is the credential — treat it as a secret):
    https://tools.example.com/Xk3n…9fQ/mcp
```

Paste that URL into **claude.ai → Settings → Connectors → Add custom
connector**.

---

## Why the URL is the credential

claude.ai's custom connectors can't send an API key or a basic-auth header.
The only thing under your control is the URL, so that's where the secret
goes: an unguessable token as the first path segment.

- Requests whose first segment doesn't match get a bare `404` — no hint that
  anything is there. The comparison is constant-time and happens before basic
  auth, so an unauthorised probe learns nothing.
- Matching requests are forwarded with the token segment removed, so your
  local server just serves `/mcp` as usual.

This is a [capability URL](https://www.w3.org/TR/capability-urls/): possession
is authorisation. Treat it exactly like a password. Anything that logs full
URLs — a proxy, a browser extension, a screen share — has your connector.

The token is generated on first use and saved to
`~/.mygrok/mcp/<subdomain>.secret` (mode 0600), so the URL survives restarts
and you don't have to re-paste it into claude.ai every morning.

---

## Flags

| Flag | Default | |
|---|---|---|
| `--secret=<token>` | load or generate | Use a specific token instead of the persisted one. |
| `--no-strip` | *(off)* | Forward the path with the token intact, for backends that enforce their own secret prefix. |
| `--path=<p>` | `/mcp` | Path suffix shown in the printed URL. The gate itself passes any suffix through. |

Every `mygrok http` flag works too — `--subdomain`, `--host`, `--basic-auth`,
`--hostname`, `--config`, and so on.

---

## Making it permanent

```bash
mygrok service install 8790 --subdomain=tools --mcp
```

Installs a launchd agent or systemd unit and prints the connector URL. The
token is **not** baked into the unit — the running agent reloads it from
`~/.mygrok/mcp/<subdomain>.secret`, so the URL stays stable across restarts
and doesn't sit in a service file.

---

## Wrapping a CLI

The pattern that makes this worth the trouble: put a small MCP server in
front of a program you already have, bind it to `127.0.0.1`, and point
`mygrok mcp` at it.

```python
# tools.py — needs `pip install mcp`
from mcp.server.fastmcp import FastMCP
import subprocess

mcp = FastMCP("workshop")

@mcp.tool()
def disk_usage(path: str = "/") -> str:
    """Report disk usage for a path on the workshop machine."""
    return subprocess.run(
        ["df", "-h", path], capture_output=True, text=True, check=True
    ).stdout

if __name__ == "__main__":
    mcp.run(transport="streamable-http")
```

```bash
python tools.py --port 8790          # or however your framework binds it
mygrok mcp 8790 --subdomain=tools
```

The tunnel is raw TCP passthrough, so SSE streams and long-running tool calls
work unmodified — nothing buffers a response waiting for it to finish.

---

## Keeping it safe

- **The URL is a password.** Don't paste it in a shared channel, a ticket, or
  a screenshot.
- **Rotate it** by deleting `~/.mygrok/mcp/<subdomain>.secret` and
  restarting. A new token is generated; update the connector in claude.ai.
- **Bind your MCP server to `127.0.0.1`**, not `0.0.0.0`. mygrok is the only
  thing that should be able to reach it.
- **Assume every tool is callable.** Anyone with the URL can invoke anything
  your server exposes, with whatever privileges it runs as. Give it the
  narrowest set of tools that does the job.
- **Layer on IP rules** if the calls will only ever come from known
  addresses: `mygrok admin allow tools <cidr>`. Note that claude.ai calls
  come from Anthropic's infrastructure, not from your browser, so allowlist
  those ranges rather than your own IP.

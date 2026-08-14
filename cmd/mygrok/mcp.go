package main

// `mygrok mcp` — expose a local MCP (Model Context Protocol) server as a
// claude.ai custom connector.
//
// claude.ai custom connectors can't send API keys or basic-auth headers, so
// the tunnel authenticates with a capability URL instead: an unguessable
// token is required as the first path segment. The agent checks it
// (constant-time), strips it, and forwards the rest of the path to the local
// backend; any other path gets a plain 404. The full URL is the credential —
// treat it like a password.
//
//	mygrok mcp 8790 --subdomain=tools
//	→ connector URL https://tools.<publicHost>/<token>/mcp
//
// The token is generated on first use and persisted to
// ~/.mygrok/mcp/<subdomain>.secret so the connector URL survives restarts.
// Backends that already enforce their own secret path can pass
// --secret=<token> --no-strip to have the tunnel require the same token but
// forward the path unmodified.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mcpSecret is the capability token requests must carry as their first path
// segment, or "" when MCP mode is off. mcpStrip controls whether the token
// segment is removed before the request is forwarded. Package-level for the
// same reason as basicAuthExpected: mygrok runs one tunnel per process.
var (
	mcpSecret string
	mcpStrip  = true
)

// mcpConnectorPath, when non-empty, is appended to the tunnel's public URL
// in the startup banner so the user can copy the exact connector URL.
var mcpConnectorPath string

func cmdMCP(args []string) {
	// Pull out the mcp-specific flags, then delegate everything else
	// (port, --subdomain, --server, config resolution, reconnect loop)
	// to cmdHTTP.
	secretFlag, rest := extractFlag(args, "--secret")
	noStrip, rest2 := extractBoolFlag(rest, "--no-strip")
	mcpPath, rest3 := extractFlag(rest2, "--path")
	args = rest3

	if mcpPath == "" {
		mcpPath = "/mcp"
	}
	if !strings.HasPrefix(mcpPath, "/") {
		mcpPath = "/" + mcpPath
	}

	// The secret file is keyed by subdomain; resolve it the same way
	// cmdHTTP will (CLI flag, falling back to config).
	subdomain := scanFlag(args, "--subdomain")
	if subdomain == "" {
		if cfg, _, err := loadConfig(scanFlag(args, "--config")); err == nil && cfg != nil {
			subdomain = cfg.Subdomain
		}
	}
	if subdomain == "" {
		fmt.Fprintln(os.Stderr, "--subdomain is required (pass on CLI or set subdomain = ... in .mygrok.toml)")
		os.Exit(2)
	}

	secret, err := resolveMCPSecret(mcpSecretDir(), subdomain, secretFlag)
	if err != nil {
		exitf("%v", err)
	}

	mcpSecret = secret
	mcpStrip = !noStrip
	mcpConnectorPath = "/" + secret + mcpPath

	cmdHTTP(args)
}

// mcpSecretDir is where per-subdomain capability tokens live.
func mcpSecretDir() string {
	return filepath.Join(homeDir(), ".mygrok", "mcp")
}

// resolveMCPSecret returns the capability token for a subdomain:
// an explicit flag value wins (and is persisted), else the previously
// persisted token, else a freshly generated one (persisted). Persisting
// keeps the connector URL stable across restarts and service reinstalls.
func resolveMCPSecret(dir, subdomain, explicit string) (string, error) {
	if !validServiceName(subdomain) {
		return "", fmt.Errorf("invalid subdomain %q", subdomain)
	}
	path := filepath.Join(dir, subdomain+".secret")

	if explicit != "" {
		if strings.ContainsAny(explicit, "/ \t\r\n") {
			return "", fmt.Errorf("--secret must not contain slashes or whitespace")
		}
		if err := writeMCPSecret(path, explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}

	if b, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	s := base64.RawURLEncoding.EncodeToString(raw[:])
	if err := writeMCPSecret(path, s); err != nil {
		return "", err
	}
	return s, nil
}

func writeMCPSecret(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// mcpGate validates that path's first segment equals secret and returns the
// remainder of the path (what the backend should see when stripping is on).
// The token comparison is constant-time; comparing lengths first would leak
// only the token length, which is not secret.
// mcpRedactPath replaces the first path segment with a placeholder, for
// logging. In MCP mode that segment is the capability token — the whole
// credential — and per-request log lines are written to a file on disk
// (~/Library/Logs on macOS) or to journald. Printing it there would hand
// the connector to anyone who can read logs, which is exactly the thing
// `mygrok mcp` tells you to protect.
func mcpRedactPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/<token>"
	}
	if i := strings.IndexByte(path[1:], '/'); i >= 0 {
		return "/<token>" + path[1+i:]
	}
	return "/<token>"
}

func mcpGate(path, secret string) (rest string, ok bool) {
	if secret == "" {
		return path, true
	}
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	seg := path[1:]
	rest = ""
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		rest = seg[i:]
		seg = seg[:i]
	}
	if subtle.ConstantTimeCompare([]byte(seg), []byte(secret)) != 1 {
		return "", false
	}
	if rest == "" {
		rest = "/"
	}
	return rest, true
}

// extractFlag removes --name=<v> or --name <v> from args, returning the
// value and the remaining args. Used to strip mcp-only flags before args
// are handed to cmdHTTP's flag set (which would reject unknown flags).
func extractFlag(args []string, name string) (value string, rest []string) {
	pre := name + "="
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == name && i+1 < len(args) {
			value = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, pre) {
			value = strings.TrimPrefix(a, pre)
			continue
		}
		rest = append(rest, a)
	}
	return value, rest
}

// extractBoolFlag removes a bare --name (or --name=true/false) from args.
func extractBoolFlag(args []string, name string) (set bool, rest []string) {
	pre := name + "="
	for _, a := range args {
		if a == name {
			set = true
			continue
		}
		if strings.HasPrefix(a, pre) {
			set = strings.TrimPrefix(a, pre) != "false"
			continue
		}
		rest = append(rest, a)
	}
	return set, rest
}

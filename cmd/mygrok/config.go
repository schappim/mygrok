package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/schappim/mygrok/internal/buildinfo"
)

// Config is what `.mygrok.toml` deserializes into.
//
// All fields are optional. Anything left empty falls back to the next layer
// in the resolution order: CLI flag → env → config → default.
type Config struct {
	Subdomain string `toml:"subdomain"`
	Port      int    `toml:"port"`
	Host      string `toml:"host"`
	Server    string `toml:"server"`
	Auth      string `toml:"auth"`
}

// configFilenames is the set of basenames searched when walking up directories.
var configFilenames = []string{".mygrok.toml", ".mygrok"}

// loadConfig resolves and reads the active config. If explicit is non-empty,
// only that path is tried and a missing file is an error. Otherwise the
// search walks upward from cwd, then falls back to ~/.mygrok/config.toml.
//
// Returns (nil, "", nil) when no config exists anywhere.
func loadConfig(explicit string) (*Config, string, error) {
	path := explicit
	if path == "" {
		path = findConfig()
	}
	if path == "" {
		return nil, "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, path, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return nil, path, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &cfg, path, nil
}

// findConfig walks up from cwd looking for any of configFilenames, then falls
// back to ~/.mygrok/config.toml. Returns "" if nothing is found.
func findConfig() string {
	if dir, err := os.Getwd(); err == nil {
		for {
			for _, name := range configFilenames {
				p := filepath.Join(dir, name)
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					return p
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".mygrok", "config.toml")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// globalConfigPath is ~/.mygrok/config.toml, the lowest-priority lookup and
// the only config location that isn't influenced by where you happen to be
// standing.
func globalConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mygrok", "config.toml")
}

// configIsTrusted reports whether a config found at path was chosen by the
// user rather than by the current directory.
//
// The directory walk is a convenience — `cd` into a project and mygrok
// finds its settings — but it also means a repository can supply settings
// simply by containing a file. That's fine for a subdomain or a port. It is
// not fine for anything that decides which server we talk to, which is why
// `mygrok update` (it downloads a binary and installs it, with sudo if
// needed) only honours a config the user pointed at explicitly or their own
// global one.
func configIsTrusted(path, explicit string) bool {
	if path == "" {
		return false
	}
	if explicit != "" && path == explicit {
		return true
	}
	if g := globalConfigPath(); g != "" && path == g {
		return true
	}
	return false
}

// resolveServer applies the standard precedence for the tunnel server
// address — env > config file > link-time default — and returns the value
// that should become the --server flag's default. CLI flags override it
// naturally by being parsed afterwards.
//
// The result may be empty: a stock `go build` stamps no DefaultServer, and
// a fresh install has no config. Callers run it through requireServer once
// flag parsing is done so the user gets one clear message instead of a
// confusing "dial tcp :0" later.
func resolveServer(cfg *Config) string {
	def := buildinfo.DefaultServer
	if cfg != nil && cfg.Server != "" {
		def = cfg.Server
	}
	if v := os.Getenv("MYGROK_SERVER"); v != "" {
		def = v
	}
	return def
}

// requireServer exits with setup instructions when no server address was
// resolved. Binaries downloaded from a running mygrokd have one stamped in;
// `go install` / Homebrew builds do not, so this is the first thing those
// users hit.
func requireServer(server string) string {
	if strings.TrimSpace(server) != "" {
		return strings.TrimSpace(server)
	}
	exitf(`no tunnel server configured.

mygrok needs to know which mygrokd to dial. Pick one:

  --server=tunnel.example.com:7000        one-off
  export MYGROK_SERVER=tunnel.example.com:7000
  echo 'server = "tunnel.example.com:7000"' >> ~/.mygrok/config.toml

Don't have a server yet? Stand one up in a few minutes:
  https://github.com/schappim/mygrok/blob/main/docs/server.md`)
	return "" // unreachable
}

// publicHostFor derives the base public host from a tunnel server address,
// so the CLI can print the URL a tunnel will appear at before it has
// connected. `tunnel.example.com:7000` → `example.com`.
//
// It's a display-time guess, not authority: once connected, the server sends
// back the real URLs in its hello response and those are what get printed.
// The guess only has to hold for the conventional `tunnel.<zone>` deployment
// that deploy/install-server.sh sets up.
func publicHostFor(server string) string {
	h := strings.TrimSpace(server)
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(strings.ToLower(h), ".")
	// A bare IP or single label has no zone to strip.
	if net.ParseIP(h) != nil || !strings.Contains(h, ".") {
		return h
	}
	if rest, ok := strings.CutPrefix(h, "tunnel."); ok && strings.Contains(rest, ".") {
		return rest
	}
	return h
}

// scanFlag pulls the value of `--key=value` or `--key value` out of args
// without modifying it. Used to read --config before flag.Parse runs.
func scanFlag(args []string, key string) string {
	pre := key + "="
	for i, a := range args {
		if a == key && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, pre) {
			return strings.TrimPrefix(a, pre)
		}
	}
	return ""
}

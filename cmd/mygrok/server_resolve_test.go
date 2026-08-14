package main

import (
	"testing"

	"github.com/schappim/mygrok/internal/buildinfo"
)

func TestResolveServerPrecedence(t *testing.T) {
	// Env beats config beats the link-time default.
	t.Setenv("MYGROK_SERVER", "")
	restore := buildinfo.DefaultServer
	buildinfo.DefaultServer = "stamped.example.com:7000"
	t.Cleanup(func() { buildinfo.DefaultServer = restore })

	if got := resolveServer(nil); got != "stamped.example.com:7000" {
		t.Errorf("no config, no env: got %q, want the stamped default", got)
	}

	cfg := &Config{Server: "cfg.example.com:7000"}
	if got := resolveServer(cfg); got != "cfg.example.com:7000" {
		t.Errorf("config set: got %q, want the config value", got)
	}

	t.Setenv("MYGROK_SERVER", "env.example.com:7000")
	if got := resolveServer(cfg); got != "env.example.com:7000" {
		t.Errorf("env set: got %q, want the env value to win over config", got)
	}
	if got := resolveServer(nil); got != "env.example.com:7000" {
		t.Errorf("env set, no config: got %q, want the env value", got)
	}
}

func TestResolveServerEmptyWhenNothingConfigured(t *testing.T) {
	t.Setenv("MYGROK_SERVER", "")
	restore := buildinfo.DefaultServer
	buildinfo.DefaultServer = ""
	t.Cleanup(func() { buildinfo.DefaultServer = restore })

	// A stock `go build` with no config must produce an empty result, so
	// requireServer can print setup instructions instead of the client
	// silently dialling nothing.
	if got := resolveServer(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := resolveServer(&Config{}); got != "" {
		t.Errorf("empty config: got %q, want empty", got)
	}
}

func TestPublicHostFor(t *testing.T) {
	cases := []struct {
		server string
		want   string
	}{
		{"tunnel.example.com:7000", "example.com"},
		{"tunnel.example.com", "example.com"},
		{"TUNNEL.Example.COM:7000", "example.com"},
		{"tunnel.example.com.:7000", "example.com"}, // trailing root dot
		// Not the conventional tunnel.<zone> shape: use the host as-is.
		{"tunnels.example.com:7000", "tunnels.example.com"},
		{"vpn.corp.example.com:7000", "vpn.corp.example.com"},
		// "tunnel.<single-label>" has no zone left to strip.
		{"tunnel.local:7000", "tunnel.local"},
		// Bare IPs and single labels pass through untouched.
		{"203.0.113.10:7000", "203.0.113.10"},
		{"203.0.113.10", "203.0.113.10"},
		{"[2001:db8::1]:7000", "2001:db8::1"},
		{"localhost:7000", "localhost"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := publicHostFor(c.server); got != c.want {
			t.Errorf("publicHostFor(%q) = %q, want %q", c.server, got, c.want)
		}
	}
}

func TestSvcSpecPublicURL(t *testing.T) {
	cases := []struct {
		name string
		spec svcSpec
		want string
	}{
		{
			name: "conventional deployment",
			spec: svcSpec{Subdomain: "app", Server: "tunnel.example.com:7000"},
			want: "https://app.example.com",
		},
		{
			name: "server host is the zone itself",
			spec: svcSpec{Subdomain: "app", Server: "tunnels.example.com:7000"},
			want: "https://app.tunnels.example.com",
		},
		{
			name: "no server configured falls back to a placeholder",
			spec: svcSpec{Subdomain: "app"},
			want: "https://app.<your-domain>",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.spec.publicURL(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

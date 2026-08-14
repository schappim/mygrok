package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPRedactPath(t *testing.T) {
	const secret = "Xk3n9fQvery-secret-token"
	cases := []struct {
		in   string
		want string
	}{
		{"/" + secret + "/mcp", "/<token>/mcp"},
		{"/" + secret, "/<token>"},
		{"/" + secret + "/mcp/messages?sid=1", "/<token>/mcp/messages?sid=1"},
		{"/", "/<token>"},
		{"", "/<token>"},
		{"no-leading-slash", "/<token>"},
	}
	for _, c := range cases {
		got := mcpRedactPath(c.in)
		if got != c.want {
			t.Errorf("mcpRedactPath(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, secret) {
			t.Errorf("mcpRedactPath(%q) leaked the token: %q", c.in, got)
		}
	}
}

func TestMCPRedactPathNeverLeaksAWrongGuessEither(t *testing.T) {
	// The 404 path logs a caller's guess. A near-miss of the real token is
	// still sensitive, so the first segment goes regardless of whether it
	// matched.
	got := mcpRedactPath("/almost-the-right-token/mcp")
	if strings.Contains(got, "almost-the-right-token") {
		t.Errorf("got %q, want the first segment redacted", got)
	}
}

func TestCheckLANCompatibility(t *testing.T) {
	cases := []struct {
		name      string
		lanIP     string
		basicAuth string
		mcp       string
		wantErr   bool
	}{
		{name: "lan off, anything goes", lanIP: "", basicAuth: "Basic x", mcp: "tok", wantErr: false},
		{name: "lan alone is fine", lanIP: "192.168.1.5", wantErr: false},
		{name: "lan with basic auth is refused", lanIP: "192.168.1.5", basicAuth: "Basic x", wantErr: true},
		{name: "lan with mcp is refused", lanIP: "192.168.1.5", mcp: "tok", wantErr: true},
		{name: "lan with both is refused", lanIP: "192.168.1.5", basicAuth: "Basic x", mcp: "tok", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkLANCompatibility(c.lanIP, c.basicAuth, c.mcp)
			if (err != nil) != c.wantErr {
				t.Fatalf("got err=%v, wantErr=%v", err, c.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "--lan") {
				t.Errorf("error should name the flag at fault: %v", err)
			}
		})
	}
}

func TestConfigIsTrusted(t *testing.T) {
	global := globalConfigPath()
	if global == "" {
		t.Skip("no home directory in this environment")
	}
	walked := filepath.Join(t.TempDir(), ".mygrok.toml")

	// A config found by walking up from the cwd is attacker-controllable by
	// anyone who can get you to clone a repository.
	if configIsTrusted(walked, "") {
		t.Error("a walked-up config must not be trusted")
	}
	// Pointing at it explicitly is a decision the user made.
	if !configIsTrusted(walked, walked) {
		t.Error("an explicitly passed --config must be trusted")
	}
	// The user's own global config is theirs.
	if !configIsTrusted(global, "") {
		t.Error("~/.mygrok/config.toml must be trusted")
	}
	if configIsTrusted("", "") {
		t.Error("no config at all is not a trusted config")
	}
	// An explicit path that isn't the one we actually loaded shouldn't
	// launder a walked config into a trusted one.
	if configIsTrusted(walked, "/some/other/path.toml") {
		t.Error("trust must be decided by the path actually loaded")
	}
}

func TestResolveBasicAuth(t *testing.T) {
	t.Setenv("MYGROK_BASIC_AUTH", "")
	if got := resolveBasicAuth("alice:s3cret"); got != "alice:s3cret" {
		t.Errorf("flag: got %q", got)
	}
	if got := resolveBasicAuth(""); got != "" {
		t.Errorf("nothing set: got %q, want empty", got)
	}

	// The env var is how `mygrok service install` keeps the password out of
	// argv and out of the world-readable unit file.
	t.Setenv("MYGROK_BASIC_AUTH", "bob:hunter2")
	if got := resolveBasicAuth(""); got != "bob:hunter2" {
		t.Errorf("env: got %q", got)
	}
	if got := resolveBasicAuth("alice:s3cret"); got != "alice:s3cret" {
		t.Errorf("flag must win over env: got %q", got)
	}
}

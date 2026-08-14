package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMCPGate(t *testing.T) {
	cases := []struct {
		path     string
		secret   string
		wantRest string
		wantOK   bool
	}{
		{"/tok/mcp", "tok", "/mcp", true},
		{"/tok", "tok", "/", true},
		{"/tok/", "tok", "/", true},
		{"/tok/a/b?ignored", "tok", "/a/b?ignored", true}, // query never reaches Path in practice
		{"/wrong/mcp", "tok", "", false},
		{"/mcp", "tok", "", false},
		{"/", "tok", "", false},
		{"", "tok", "", false},
		{"/tokX/mcp", "tok", "", false},
		{"/to/mcp", "tok", "", false},
		{"/anything", "", "/anything", true}, // gate disabled
	}
	for _, c := range cases {
		rest, ok := mcpGate(c.path, c.secret)
		if ok != c.wantOK || (ok && rest != c.wantRest) {
			t.Errorf("mcpGate(%q, %q) = (%q, %v), want (%q, %v)",
				c.path, c.secret, rest, ok, c.wantRest, c.wantOK)
		}
	}
}

func TestResolveMCPSecret(t *testing.T) {
	dir := t.TempDir()

	// Generated on first use, persisted, stable on second call.
	s1, err := resolveMCPSecret(dir, "jarvis", "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(s1) < 40 {
		t.Errorf("generated secret too short: %q", s1)
	}
	s2, err := resolveMCPSecret(dir, "jarvis", "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if s1 != s2 {
		t.Errorf("secret not stable: %q vs %q", s1, s2)
	}

	// File permissions are owner-only.
	fi, err := os.Stat(filepath.Join(dir, "jarvis.secret"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %v, want 0600", fi.Mode().Perm())
	}

	// Explicit secret wins and is persisted.
	s3, err := resolveMCPSecret(dir, "jarvis", "my-explicit-token")
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if s3 != "my-explicit-token" {
		t.Errorf("explicit secret not honoured: %q", s3)
	}
	s4, _ := resolveMCPSecret(dir, "jarvis", "")
	if s4 != "my-explicit-token" {
		t.Errorf("explicit secret not persisted: %q", s4)
	}

	// Separate subdomains get separate secrets.
	other, _ := resolveMCPSecret(dir, "other", "")
	if other == s4 {
		t.Error("subdomains share a secret")
	}

	// Rejects bad input.
	if _, err := resolveMCPSecret(dir, "Bad_Sub!", ""); err == nil {
		t.Error("invalid subdomain accepted")
	}
	if _, err := resolveMCPSecret(dir, "jarvis", "has/slash"); err == nil {
		t.Error("secret with slash accepted")
	}
}

func TestExtractFlag(t *testing.T) {
	v, rest := extractFlag([]string{"8790", "--secret=abc", "--subdomain=x"}, "--secret")
	if v != "abc" || strings.Join(rest, " ") != "8790 --subdomain=x" {
		t.Errorf("got %q rest=%v", v, rest)
	}
	v, rest = extractFlag([]string{"--secret", "abc", "8790"}, "--secret")
	if v != "abc" || strings.Join(rest, " ") != "8790" {
		t.Errorf("space form: got %q rest=%v", v, rest)
	}
	set, rest := extractBoolFlag([]string{"8790", "--no-strip"}, "--no-strip")
	if !set || strings.Join(rest, " ") != "8790" {
		t.Errorf("bool form: got %v rest=%v", set, rest)
	}
	set, _ = extractBoolFlag([]string{"--no-strip=false"}, "--no-strip")
	if set {
		t.Error("--no-strip=false parsed as set")
	}
}

// TestProxyToLocalMCPGate runs requests through the real proxy path with the
// MCP gate active: valid token forwards (stripped by default, intact with
// --no-strip), anything else is a bare 404 that never reaches the backend.
func TestProxyToLocalMCPGate(t *testing.T) {
	backendPaths := make(chan string, 8)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendPaths <- r.URL.RequestURI()
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "ok")
	}))
	defer backend.Close()
	target := strings.TrimPrefix(backend.URL, "http://")

	log.SetOutput(io.Discard)
	defer log.SetOutput(nil)

	mcpSecret = "sekrit-token"
	defer func() { mcpSecret = ""; mcpStrip = true }()

	type tc struct {
		name    string
		strip   bool
		path    string
		want    int
		backend string // expected backend path ("" = must not reach backend)
	}
	cases := []tc{
		{"valid stripped", true, "/sekrit-token/mcp", 200, "/mcp"},
		{"valid with query", true, "/sekrit-token/mcp?a=1", 200, "/mcp?a=1"},
		{"valid no-strip", false, "/sekrit-token/mcp", 200, "/sekrit-token/mcp"},
		{"wrong token", true, "/nope/mcp", 404, ""},
		{"bare path", true, "/mcp", 404, ""},
		{"root", true, "/", 404, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mcpStrip = c.strip

			clientSide, serverSide := pairTCP(t)
			defer clientSide.Close()
			done := make(chan struct{})
			go func() {
				proxyToLocal(serverSide, target)
				close(done)
			}()

			if _, err := fmt.Fprintf(clientSide, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", c.path); err != nil {
				t.Fatalf("write: %v", err)
			}
			br := bufio.NewReader(clientSide)
			resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			if resp.StatusCode != c.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, c.want)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if c.backend != "" {
				select {
				case got := <-backendPaths:
					if got != c.backend {
						t.Errorf("backend saw %q, want %q", got, c.backend)
					}
				case <-time.After(2 * time.Second):
					t.Error("backend never saw the request")
				}
			} else {
				select {
				case got := <-backendPaths:
					t.Errorf("request leaked past the gate to backend: %q", got)
				case <-time.After(100 * time.Millisecond):
				}
			}

			_ = clientSide.(interface{ CloseWrite() error }).CloseWrite()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("proxyToLocal did not return")
			}
		})
	}
}

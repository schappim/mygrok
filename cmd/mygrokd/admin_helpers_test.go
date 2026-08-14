package main

import (
	"strings"
	"testing"
)

func TestParseRequestLine(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantMethod string
		wantPath   string
	}{
		{"simple GET", "GET / HTTP/1.1\r\nHost: x\r\n\r\n", "GET", "/"},
		{"POST with path and query", "POST /admin/ips?x=1 HTTP/1.1\r\n\r\n", "POST", "/admin/ips?x=1"},
		{"LF-only line ending", "DELETE /thing HTTP/1.0\n\n", "DELETE", "/thing"},
		{"empty raw", "", "", ""},
		{"truncated (no version)", "GET /\r\n", "GET", "/"}, // 2 parts is OK; we only need method+path
		{"only method", "GET\r\n", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, p := parseRequestLine([]byte(tc.raw))
			if m != tc.wantMethod || p != tc.wantPath {
				t.Errorf("got (%q, %q) want (%q, %q)", m, p, tc.wantMethod, tc.wantPath)
			}
		})
	}
}

func TestValidBinaryName(t *testing.T) {
	good := []string{
		"mygrok-darwin-arm64",
		"mygrok-darwin-amd64",
		"mygrok-linux-amd64",
		"mygrok-linux-arm64",
		"mygrok-windows-amd64",
	}
	for _, n := range good {
		if !validBinaryName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	bad := []string{
		"",
		"some-binary",                   // wrong prefix
		"mygrok-darwin-arm64; rm -rf /", // not all lowercase alnum/-
		"mygrok-UPPER-amd64",            // uppercase
		"mygrok-darwin-amd64.exe",       // dot
		"mygrok_darwin",                 // underscore
		"mygrok-../etc/passwd",          // path traversal
	}
	for _, n := range bad {
		if validBinaryName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestStatusText(t *testing.T) {
	cases := map[int]string{
		200: "OK",
		400: "Bad Request",
		404: "Not Found",
		500: "Internal Server Error",
		502: "Bad Gateway",
		418: "OK", // fallback
	}
	for code, want := range cases {
		if got := statusText(code); got != want {
			t.Errorf("statusText(%d): got %q want %q", code, got, want)
		}
	}
}

func TestShortID(t *testing.T) {
	// Short IDs are returned unchanged.
	if got := shortID("short"); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := shortID(""); got != "" {
		t.Errorf("got %q", got)
	}

	// Long IDs get a … in the middle.
	long := strings.Repeat("a", 8) + strings.Repeat("b", 16) + strings.Repeat("c", 8)
	got := shortID(long)
	if !strings.Contains(got, "…") {
		t.Errorf("expected ellipsis, got %q", got)
	}
	if !strings.HasPrefix(got, "aaaaaaaa") {
		t.Errorf("expected aaaaaaaa prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "cccccccc") {
		t.Errorf("expected cccccccc suffix, got %q", got)
	}
}

func TestBytesEqual(t *testing.T) {
	if !bytesEqual([]byte("abc"), []byte("abc")) {
		t.Error("equal slices should match")
	}
	if bytesEqual([]byte("abc"), []byte("abd")) {
		t.Error("different slices should not match")
	}
	if bytesEqual([]byte("abc"), []byte("abcd")) {
		t.Error("different-length slices should not match")
	}
	if !bytesEqual(nil, []byte{}) {
		t.Error("nil and empty should match")
	}
}

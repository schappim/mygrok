package main

import (
	"bufio"
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

func TestInjectClientIPStripsForwardedAndRealIP(t *testing.T) {
	in := []byte("GET / HTTP/1.1\r\n" +
		"Host: x\r\n" +
		"X-Forwarded-For: 1.2.3.4\r\n" +
		"x-real-ip: 5.6.7.8\r\n" +
		"User-Agent: tests\r\n" +
		"\r\n")
	out := injectClientIP(in, "9.9.9.9")
	s := string(out)

	// Old forged headers must be gone.
	if strings.Contains(s, "1.2.3.4") {
		t.Errorf("inbound XFF leaked through:\n%s", s)
	}
	if strings.Contains(s, "5.6.7.8") {
		t.Errorf("inbound X-Real-IP leaked through:\n%s", s)
	}
	if !strings.Contains(s, "X-Forwarded-For: 9.9.9.9") {
		t.Errorf("missing injected XFF:\n%s", s)
	}
	if !strings.Contains(s, "X-Real-IP: 9.9.9.9") {
		t.Errorf("missing injected X-Real-IP:\n%s", s)
	}
	// Other headers must be preserved.
	if !strings.Contains(s, "User-Agent: tests") {
		t.Errorf("User-Agent removed:\n%s", s)
	}
	if !strings.HasSuffix(s, "\r\n\r\n") {
		t.Errorf("must end with CRLF blank line:\n%q", s)
	}
}

func TestInjectClientIPNoExistingHeaders(t *testing.T) {
	in := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	out := injectClientIP(in, "10.0.0.1")
	s := string(out)
	if !strings.Contains(s, "X-Forwarded-For: 10.0.0.1") || !strings.Contains(s, "X-Real-IP: 10.0.0.1") {
		t.Errorf("expected injected headers:\n%s", s)
	}
}

func TestInjectClientIPHandlesLFOnlyLineEndings(t *testing.T) {
	in := []byte("GET / HTTP/1.0\nHost: x\nX-Real-IP: bad\n\n")
	out := injectClientIP(in, "10.0.0.1")
	s := string(out)
	if strings.Contains(s, "bad") {
		t.Errorf("inbound X-Real-IP not stripped in LF mode:\n%s", s)
	}
	if !strings.Contains(s, "X-Real-IP: 10.0.0.1") {
		t.Errorf("missing injected X-Real-IP:\n%s", s)
	}
}

func TestInjectClientIPNoBlankLineUnchanged(t *testing.T) {
	in := []byte("GET / HTTP/1.1\r\nHost: x\r\n")
	out := injectClientIP(in, "10.0.0.1")
	if !bytes.Equal(in, out) {
		t.Errorf("malformed input should be returned unchanged")
	}
}

func TestReadHeadersAndHost(t *testing.T) {
	raw := "GET /foo HTTP/1.1\r\nHost: alice.example.com\r\nUser-Agent: x\r\n\r\nbody"
	br := bufio.NewReader(strings.NewReader(raw))
	headers, host, err := readHeadersAndHost(br)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if host != "alice.example.com" {
		t.Errorf("got host %q", host)
	}
	if !bytes.HasSuffix(headers, []byte("\r\n\r\n")) {
		t.Errorf("headers must end with blank line, got %q", string(headers))
	}
	if bytes.Contains(headers, []byte("body")) {
		t.Errorf("body must not be consumed: %q", string(headers))
	}
	// Body should still be there.
	rest, _ := br.ReadString(0)
	_ = rest
}

func TestReadHeadersAndHostCaseInsensitive(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nhost:   alice.example.com   \r\n\r\n"
	br := bufio.NewReader(strings.NewReader(raw))
	_, host, err := readHeadersAndHost(br)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if host != "alice.example.com" {
		t.Errorf("host should be trimmed and case-insensitive: got %q", host)
	}
}

func TestReadHeadersAndHostTooLarge(t *testing.T) {
	big := "GET / HTTP/1.1\r\nX-Big: " + strings.Repeat("a", 70*1024) + "\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(big))
	_, _, err := readHeadersAndHost(br)
	if err == nil {
		t.Error("expected error for oversized headers")
	}
}

func TestRemoteIPFrom(t *testing.T) {
	tcp := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 4242}
	if got := remoteIPFrom(tcp); got != "203.0.113.7" {
		t.Errorf("got %q", got)
	}
	ipv6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 80}
	if got := remoteIPFrom(ipv6); got != "2001:db8::1" {
		t.Errorf("ipv6: got %q", got)
	}
}

type addrStr string

func (a addrStr) Network() string { return "tcp" }
func (a addrStr) String() string  { return string(a) }

func TestRemoteIPFromMalformed(t *testing.T) {
	// SplitHostPort fails → returns the original string. Defensive fallback.
	if got := remoteIPFrom(addrStr("not-a-host-port")); got != "not-a-host-port" {
		t.Errorf("got %q", got)
	}
}

func TestWantsJSON(t *testing.T) {
	yes := []string{
		"GET / HTTP/1.1\r\nAccept: application/json\r\n\r\n",
		"GET / HTTP/1.1\r\nAccept: text/html, application/json\r\n\r\n",
		"GET / HTTP/1.1\r\nAccept: APPLICATION/JSON\r\n\r\n",
	}
	for _, r := range yes {
		if !wantsJSON([]byte(r)) {
			t.Errorf("wantsJSON should be true for:\n%s", r)
		}
	}
	no := []string{
		"GET / HTTP/1.1\r\nAccept: text/html\r\n\r\n",
		"GET / HTTP/1.1\r\n\r\n", // no Accept at all
	}
	for _, r := range no {
		if wantsJSON([]byte(r)) {
			t.Errorf("wantsJSON should be false for:\n%s", r)
		}
	}
}

func TestReadAdminCookie(t *testing.T) {
	tests := []struct {
		name string
		hdrs string
		want string
	}{
		{
			"single cookie",
			"GET / HTTP/1.1\r\nCookie: " + adminCookieName + "=abc123\r\n\r\n",
			"abc123",
		},
		{
			"multiple cookies",
			"GET / HTTP/1.1\r\nCookie: foo=bar; " + adminCookieName + "=xyz; baz=qux\r\n\r\n",
			"xyz",
		},
		{
			"case-insensitive header name",
			"GET / HTTP/1.1\r\ncookie: " + adminCookieName + "=val\r\n\r\n",
			"val",
		},
		{"no cookie header", "GET / HTTP/1.1\r\n\r\n", ""},
		{
			"other cookies only",
			"GET / HTTP/1.1\r\nCookie: foo=bar\r\n\r\n",
			"",
		},
		{
			"malformed entry (no equals) is skipped",
			"GET / HTTP/1.1\r\nCookie: noequal; " + adminCookieName + "=ok\r\n\r\n",
			"ok",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := readAdminCookie([]byte(tc.hdrs))
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestMergeSorted(t *testing.T) {
	got := mergeSorted([]string{"b", "a"}, []string{"c", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}

	// Empty + empty.
	if got := mergeSorted(nil, nil); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}

	// Only one side has values.
	got = mergeSorted([]string{"z", "a"}, nil)
	if len(got) != 2 || got[0] != "a" || got[1] != "z" {
		t.Errorf("got %v", got)
	}
}

func TestAdminSessionsIssueAndValidate(t *testing.T) {
	s := &adminSessions{m: map[string]time.Time{}}
	if s.valid("") {
		t.Error("empty session id should be invalid")
	}
	if s.valid("never-issued") {
		t.Error("unknown session id should be invalid")
	}
	id := s.issue()
	if id == "" {
		t.Fatal("issue returned empty id")
	}
	if !s.valid(id) {
		t.Error("freshly issued id should be valid")
	}

	// Force-expire it and re-check.
	s.mu.Lock()
	s.m[id] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.valid(id) {
		t.Error("expired session id should be invalid")
	}
}

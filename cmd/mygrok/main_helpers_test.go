package main

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestSplitCSV(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a.example.com", []string{"a.example.com"}},
		{"a.example.com,b.example.com", []string{"a.example.com", "b.example.com"}},
		{"  a.example.com  ,  b.example.com  ", []string{"a.example.com", "b.example.com"}},
		{"a,,b", []string{"a", "b"}},         // empty entries dropped
		{"a,b,a,c", []string{"a", "b", "c"}}, // dedup preserves order
		{"  a  ,a", []string{"a"}},           // trim then dedup
	}
	for _, tc := range tests {
		got := splitCSV(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestResolveLANFlag(t *testing.T) {
	t.Run("empty disables", func(t *testing.T) {
		ip, err := resolveLANFlag("")
		if err != nil || ip != "" {
			t.Errorf("got %q,%v want \"\",nil", ip, err)
		}
		ip, err = resolveLANFlag("   ")
		if err != nil || ip != "" {
			t.Errorf("whitespace: got %q,%v want \"\",nil", ip, err)
		}
	})
	t.Run("literal RFC1918", func(t *testing.T) {
		ip, err := resolveLANFlag("192.168.1.42")
		if err != nil || ip != "192.168.1.42" {
			t.Errorf("got %q,%v", ip, err)
		}
	})
	t.Run("not RFC1918 rejected", func(t *testing.T) {
		if _, err := resolveLANFlag("8.8.8.8"); err == nil {
			t.Error("expected error for public IP")
		}
	})
	t.Run("invalid IPv4 rejected", func(t *testing.T) {
		if _, err := resolveLANFlag("not-an-ip"); err == nil {
			t.Error("expected error for garbage")
		}
		if _, err := resolveLANFlag("999.999.999.999"); err == nil {
			t.Error("expected error for out-of-range")
		}
	})
	t.Run("ipv6 rejected (we need IPv4)", func(t *testing.T) {
		if _, err := resolveLANFlag("fc00::1"); err == nil {
			t.Error("expected error for ipv6")
		}
	})
}

func TestIsSubdomainInUse(t *testing.T) {
	if !isSubdomainInUse(errors.New("subdomain in use")) {
		t.Error("exact phrase should match")
	}
	if !isSubdomainInUse(fmt.Errorf("server: %s", "subdomain in use")) {
		t.Error("wrapped phrase should match")
	}
	if isSubdomainInUse(nil) {
		t.Error("nil error should not match")
	}
	if isSubdomainInUse(errors.New("some other error")) {
		t.Error("unrelated error should not match")
	}
}

func TestJitterStaysUnderBound(t *testing.T) {
	bound := 100 * time.Millisecond
	for i := 0; i < 200; i++ {
		j := jitter(bound)
		if j < 0 || j >= bound {
			t.Fatalf("jitter %v out of bounds [0, %v)", j, bound)
		}
	}
}

func TestIsUpgradeRequest(t *testing.T) {
	r := func(headers map[string]string) *http.Request {
		req, _ := http.NewRequest("GET", "/", nil)
		for k, v := range headers {
			req.Header.Add(k, v)
		}
		return req
	}

	if isUpgradeRequest(r(nil)) {
		t.Error("no upgrade header should be false")
	}
	if !isUpgradeRequest(r(map[string]string{"Connection": "upgrade"})) {
		t.Error("lowercase upgrade token should match")
	}
	if !isUpgradeRequest(r(map[string]string{"Connection": "keep-alive, Upgrade"})) {
		t.Error("comma-separated tokens should match")
	}
	if !isUpgradeRequest(r(map[string]string{"Upgrade": "websocket"})) {
		t.Error("Upgrade header alone should match (some clients send no Connection token)")
	}
	if isUpgradeRequest(r(map[string]string{"Connection": "close"})) {
		t.Error("close should not match")
	}
}

func TestStatusColor(t *testing.T) {
	if statusColor(500) != 31 {
		t.Errorf("5xx should be red (31), got %d", statusColor(500))
	}
	if statusColor(404) != 33 {
		t.Errorf("4xx should be yellow (33), got %d", statusColor(404))
	}
	if statusColor(301) != 36 {
		t.Errorf("3xx should be cyan (36), got %d", statusColor(301))
	}
	if statusColor(200) != 32 {
		t.Errorf("2xx should be green (32), got %d", statusColor(200))
	}
	if statusColor(0) != 0 {
		t.Errorf("0 should map to no-color (0), got %d", statusColor(0))
	}
}

func TestFormatDuration(t *testing.T) {
	if got := formatDuration(0); got != "-" {
		t.Errorf("zero: got %q", got)
	}
	if got := formatDuration(-5 * time.Millisecond); got != "-" {
		t.Errorf("negative: got %q", got)
	}
	if got := formatDuration(500 * time.Microsecond); got != "500µs" {
		t.Errorf("got %q", got)
	}
	if got := formatDuration(120 * time.Millisecond); got != "120ms" {
		t.Errorf("got %q", got)
	}
	if got := formatDuration(15 * time.Second); got != "15.0s" {
		t.Errorf("got %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 5); got != "hello" {
		t.Errorf("exact-length should pass through: %q", got)
	}
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("short should pass through: %q", got)
	}
	if got := truncate("0123456789abcdef", 5); got != "01234..." {
		t.Errorf("got %q", got)
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("MYGROK_TEST_ENV_OR", "")
	if got := envOr("MYGROK_TEST_ENV_OR", "fallback"); got != "fallback" {
		t.Errorf("empty env should yield fallback, got %q", got)
	}
	t.Setenv("MYGROK_TEST_ENV_OR", "explicit")
	if got := envOr("MYGROK_TEST_ENV_OR", "fallback"); got != "explicit" {
		t.Errorf("set env should win, got %q", got)
	}
}

func TestGenerateClientIDIsHex32(t *testing.T) {
	a := generateClientID()
	b := generateClientID()
	if a == b {
		t.Error("two IDs in a row collided — vanishingly unlikely")
	}
	if len(a) != 32 {
		t.Errorf("len=%d want 32", len(a))
	}
	for _, r := range a {
		isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isHex {
			t.Errorf("non-hex char %q in id %q", r, a)
		}
	}
}

func TestRandomSubdomainIsValid(t *testing.T) {
	for i := 0; i < 50; i++ {
		s := randomSubdomain()
		if len(s) != 6 {
			t.Fatalf("len=%d want 6 (got %q)", len(s), s)
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				t.Fatalf("invalid char %q in %q", r, s)
			}
		}
	}
}

func TestClientLooksLikeSubdomain(t *testing.T) {
	good := []string{"a", "abc", "abc-def", "a1b2"}
	for _, s := range good {
		if !looksLikeSubdomain(s) {
			t.Errorf("%q should be subdomain", s)
		}
	}
	bad := []string{
		"",
		"./foo",
		"~/foo",
		"foo.com", // contains '.'
		"some\\path",
		"-leading",
		"trailing-",
		"UPPER",
		"under_score",
	}
	for _, s := range bad {
		if looksLikeSubdomain(s) {
			t.Errorf("%q should NOT be subdomain (is path-like)", s)
		}
	}
}

func TestRemoteIPHost(t *testing.T) {
	if got := remoteIPHost("192.0.2.5:443"); got != "192.0.2.5" {
		t.Errorf("got %q", got)
	}
	if got := remoteIPHost("[2001:db8::1]:80"); got != "2001:db8::1" {
		t.Errorf("got %q", got)
	}
	// Malformed input falls through unchanged.
	if got := remoteIPHost("nohost"); got != "nohost" {
		t.Errorf("got %q", got)
	}
}

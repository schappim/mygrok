package main

import (
	"strings"
	"testing"
)

func TestValidSubdomain(t *testing.T) {
	good := []string{"a", "abc", "abc123", "my-tunnel", "x" + strings.Repeat("y", 62)}
	for _, s := range good {
		if !validSubdomain(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	bad := []string{
		"",
		strings.Repeat("a", 64),
		"-leading",
		"trailing-",
		"UPPER",   // only lowercase allowed
		"has.dot", // dots are not subdomain chars
		"under_score",
		"with space",
		"emoji😀",
	}
	for _, s := range bad {
		if validSubdomain(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestValidateCustomHost(t *testing.T) {
	good := []string{
		"example.com",
		"foo.example.com",
		"a-b.example.com",
		"1.example.com",
		"x.y.z.io",
	}
	for _, h := range good {
		if err := validateCustomHost(h); err != nil {
			t.Errorf("%q should be valid: %v", h, err)
		}
	}
	bad := []string{
		"",
		"nodot",                          // no '.'
		"-leading.example.com",           // leading hyphen
		"trailing-.example.com",          // trailing hyphen
		"UPPER.example.com",              // uppercase
		".example.com",                   // empty leading label
		"example..com",                   // empty middle label
		"under_score.example.com",        // underscore
		strings.Repeat("a", 64) + ".com", // label > 63
		strings.Repeat("a.", 130) + "co", // total length > 253
	}
	for _, h := range bad {
		if err := validateCustomHost(h); err == nil {
			t.Errorf("%q should be invalid", h)
		}
	}
}

func TestNormalizeHostnames(t *testing.T) {
	public := "tunnels.test"

	got, err := normalizeHostnames(nil, public)
	if err != nil || got != nil {
		t.Errorf("nil input: got %v %v", got, err)
	}

	got, err = normalizeHostnames([]string{"Example.com", "  app.example.com  ", "example.com"}, public)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"example.com", "app.example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q want %q", i, got[i], want[i])
		}
	}

	// Hostnames under the public host are rejected — those belong to
	// --subdomain.
	if _, err := normalizeHostnames([]string{"foo." + public}, public); err == nil {
		t.Error("expected error for hostname under public host")
	}
	if _, err := normalizeHostnames([]string{public}, public); err == nil {
		t.Error("expected error for exact match of public host")
	}

	// Invalid hostnames bubble up.
	if _, err := normalizeHostnames([]string{"bad_host"}, public); err == nil {
		t.Error("expected error for invalid host")
	}

	// Public host comparison is case insensitive.
	if _, err := normalizeHostnames([]string{"foo.TUNNELS.TEST"}, public); err == nil {
		t.Error("expected error for case-mismatched public host suffix")
	}
}

func TestSubdomainOf(t *testing.T) {
	tests := []struct {
		host string
		base string
		want string
	}{
		{"alice.example.com", "example.com", "alice"},
		{"a.b.example.com", "example.com", "a.b"},
		{"example.com", "example.com", ""},
		{"other.com", "example.com", ""},
		{"badexample.com", "example.com", ""},              // not a true subdomain
		{"alice.example.com:8443", "example.com", "alice"}, // strips port
		{"ALICE.EXAMPLE.COM", "example.com", "alice"},      // lowercases
		{"alice.example.com.", "example.com", ""},          // trailing dot is not handled
		{"sub-with-dash.example.com", "example.com", "sub-with-dash"},
	}
	for _, tc := range tests {
		got := subdomainOf(tc.host, tc.base)
		if got != tc.want {
			t.Errorf("subdomainOf(%q, %q) = %q, want %q", tc.host, tc.base, got, tc.want)
		}
	}
}

func TestIsManagementHost(t *testing.T) {
	if !isManagementHost("tunnel.example.com", "example.com") {
		t.Error("tunnel.example.com should be management host of example.com")
	}
	if isManagementHost("foo.example.com", "example.com") {
		t.Error("foo.example.com should not be management host")
	}
	if isManagementHost("tunnel.example.com", "other.com") {
		t.Error("management host check should depend on public host")
	}
	if isManagementHost("example.com", "example.com") {
		t.Error("apex should not be management host")
	}
}

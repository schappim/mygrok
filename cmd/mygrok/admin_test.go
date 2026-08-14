package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestShortenID(t *testing.T) {
	if got := shortenID("short"); got != "short" {
		t.Errorf("short string: got %q", got)
	}
	long := strings.Repeat("a", 8) + strings.Repeat("x", 8) + strings.Repeat("b", 8)
	got := shortenID(long)
	if !strings.Contains(got, "…") {
		t.Errorf("missing ellipsis: %q", got)
	}
	if !strings.HasPrefix(got, "aaaaaaaa") {
		t.Errorf("expected aaaaaaaa prefix, got %q", got)
	}
	if !strings.HasSuffix(got, "bbbbbbbb") {
		t.Errorf("expected bbbbbbbb suffix, got %q", got)
	}
}

func TestKeysOfSorted(t *testing.T) {
	m := map[string]apiBucket{
		"zulu":  {},
		"alpha": {},
		"mike":  {},
	}
	got := keysOf(m)
	want := []string{"alpha", "mike", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestKeysOfEmpty(t *testing.T) {
	if got := keysOf(map[string]apiBucket{}); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestSortStrings(t *testing.T) {
	a := []string{"c", "a", "b", "a"}
	sortStrings(a)
	want := []string{"a", "a", "b", "c"}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("got %v want %v", a, want)
	}

	// Already-sorted is a noop.
	a = []string{"a", "b", "c"}
	sortStrings(a)
	if a[0] != "a" || a[1] != "b" || a[2] != "c" {
		t.Errorf("got %v", a)
	}

	// Single + empty slices.
	sortStrings([]string{})
	sortStrings([]string{"only"})
}

func TestNewAdminClientStripsPortFromHostInBaseURL(t *testing.T) {
	c := newAdminClient("tunnel.example.com:7000", "token")
	if c.baseURL != "https://tunnel.example.com" {
		t.Errorf("baseURL: got %q want %q", c.baseURL, "https://tunnel.example.com")
	}
	if c.auth != "token" {
		t.Errorf("auth: got %q", c.auth)
	}
	if c.httpc == nil || c.httpc.Timeout == 0 {
		t.Error("httpc must be configured with a timeout")
	}
}

func TestNewAdminClientNoPort(t *testing.T) {
	c := newAdminClient("tunnel.example.com", "token")
	if c.baseURL != "https://tunnel.example.com" {
		t.Errorf("baseURL: got %q", c.baseURL)
	}
}

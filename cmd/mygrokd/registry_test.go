package main

import (
	"net"
	"reflect"
	"testing"

	"github.com/hashicorp/yamux"
)

// newTestSession creates a real (but unused) yamux session backed by a
// net.Pipe. The registry holds *yamux.Session pointers and on takeover
// calls .Close() on the preempted session, so nil won't do.
func newTestSession(t *testing.T) (*yamux.Session, func()) {
	t.Helper()
	a, b := net.Pipe()
	server, err := yamux.Server(a, nil)
	if err != nil {
		t.Fatalf("yamux server: %v", err)
	}
	client, err := yamux.Client(b, nil)
	if err != nil {
		t.Fatalf("yamux client: %v", err)
	}
	cleanup := func() {
		_ = server.Close()
		_ = client.Close()
		_ = a.Close()
		_ = b.Close()
	}
	return server, cleanup
}

func TestRegistryClaimSimple(t *testing.T) {
	reg := newRegistry()
	sess, cleanup := newTestSession(t)
	defer cleanup()

	tu, preempted, msg := reg.claim("alice", "client-1", nil, "203.0.113.1", "", "", sess)
	if msg != "" {
		t.Fatalf("expected success, got error %q", msg)
	}
	if tu == nil || tu.subdomain != "alice" {
		t.Errorf("unexpected tunnel: %+v", tu)
	}
	if preempted != nil {
		t.Error("no prior tunnel — preempted should be nil")
	}
	if got := reg.get("alice"); got != tu {
		t.Error("get should return the claimed tunnel")
	}
}

func TestRegistryClaimRejectsConflictDifferentClientID(t *testing.T) {
	reg := newRegistry()
	s1, c1 := newTestSession(t)
	defer c1()
	if _, _, msg := reg.claim("alice", "client-1", nil, "", "", "", s1); msg != "" {
		t.Fatalf("first: %v", msg)
	}
	s2, c2 := newTestSession(t)
	defer c2()
	tu, preempted, msg := reg.claim("alice", "client-DIFFERENT", nil, "", "", "", s2)
	if msg == "" {
		t.Error("expected rejection for different clientID")
	}
	if tu != nil || preempted != nil {
		t.Errorf("expected nils on rejection, got tu=%v preempted=%v", tu, preempted)
	}
}

func TestRegistryClaimTakeoverSameClientID(t *testing.T) {
	reg := newRegistry()
	s1, c1 := newTestSession(t)
	defer c1()
	first, _, msg := reg.claim("alice", "client-1", []string{"a.example.com"}, "", "", "", s1)
	if msg != "" {
		t.Fatalf("first: %v", msg)
	}

	s2, c2 := newTestSession(t)
	defer c2()
	second, preempted, msg := reg.claim("alice", "client-1", []string{"b.example.com"}, "", "", "", s2)
	if msg != "" {
		t.Fatalf("takeover: %v", msg)
	}
	if preempted != first {
		t.Error("preempted should be the original tunnel")
	}
	if reg.get("alice") != second {
		t.Error("registry should now hold the new tunnel")
	}
	// Old hostname should be released; new one should resolve.
	if reg.lookup("a.example.com") != nil {
		t.Error("old hostname should no longer route")
	}
	if reg.lookup("b.example.com") != second {
		t.Error("new hostname should route to the new tunnel")
	}
}

func TestRegistryClaimRejectsHostnameOwnedByAnotherTunnel(t *testing.T) {
	reg := newRegistry()
	s1, c1 := newTestSession(t)
	defer c1()
	if _, _, msg := reg.claim("alice", "client-1", []string{"shared.example.com"}, "", "", "", s1); msg != "" {
		t.Fatalf("first: %v", msg)
	}
	s2, c2 := newTestSession(t)
	defer c2()
	if _, _, msg := reg.claim("bob", "client-2", []string{"shared.example.com"}, "", "", "", s2); msg == "" {
		t.Error("expected rejection for hostname collision")
	}
	// Alice's tunnel must still own the hostname.
	if reg.lookup("shared.example.com") == nil {
		t.Error("alice should still own shared.example.com")
	}
}

func TestRegistryReleaseIgnoresStaleSession(t *testing.T) {
	reg := newRegistry()
	s1, c1 := newTestSession(t)
	defer c1()
	first, _, _ := reg.claim("alice", "client-1", []string{"a.example.com"}, "", "", "", s1)

	// Takeover with same clientID.
	s2, c2 := newTestSession(t)
	defer c2()
	second, _, _ := reg.claim("alice", "client-1", []string{"b.example.com"}, "", "", "", s2)
	_ = second

	// Releasing under the *old* session must NOT evict the new owner.
	reg.release("alice", first.session)
	if reg.get("alice") != second {
		t.Error("stale release should not evict current tunnel")
	}
	if reg.lookup("b.example.com") != second {
		t.Error("stale release should not touch hostnames")
	}

	// Releasing under the current session must evict.
	reg.release("alice", second.session)
	if reg.get("alice") != nil {
		t.Error("current-session release should evict")
	}
	if reg.lookup("b.example.com") != nil {
		t.Error("current-session release should drop hostnames")
	}
}

func TestRegistryLookupAndHostKnown(t *testing.T) {
	reg := newRegistry()
	s, c := newTestSession(t)
	defer c()
	if _, _, msg := reg.claim("alice", "c1", []string{"foo.example.com", "bar.example.com"}, "", "", "", s); msg != "" {
		t.Fatal(msg)
	}
	if !reg.hostKnown("foo.example.com") {
		t.Error("hostKnown should be true for registered host")
	}
	if reg.hostKnown("never.example.com") {
		t.Error("hostKnown should be false for unregistered host")
	}
	if reg.lookup("foo.example.com") == nil {
		t.Error("lookup should find registered host")
	}
	if reg.lookup("nope.example.com") != nil {
		t.Error("lookup should be nil for unregistered host")
	}
}

func TestRegistryActiveSubdomainsSorted(t *testing.T) {
	reg := newRegistry()
	for _, sub := range []string{"zulu", "alpha", "mike"} {
		s, c := newTestSession(t)
		defer c()
		if _, _, msg := reg.claim(sub, "id-"+sub, nil, "", "", "", s); msg != "" {
			t.Fatal(msg)
		}
	}
	got := reg.activeSubdomains()
	want := []string{"alpha", "mike", "zulu"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

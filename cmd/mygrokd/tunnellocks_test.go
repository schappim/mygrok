package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func mustTunnelLocks(t *testing.T) *tunnelLocks {
	t.Helper()
	l, err := newTunnelLocks(filepath.Join(t.TempDir(), "locks.json"))
	if err != nil {
		t.Fatalf("newTunnelLocks: %v", err)
	}
	return l
}

func TestTunnelLocksStartOpen(t *testing.T) {
	l := mustTunnelLocks(t)
	if l.IsLocked("alice") {
		t.Error("fresh store should have no locked tunnels")
	}
	if l.AllowsUser("alice", "uid-1") {
		t.Error("AllowsUser on unlocked tunnel should be false")
	}
	if got := l.AllowedUsers("alice"); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
	if got := l.LockedSubdomains(); len(got) != 0 {
		t.Errorf("expected no locked subdomains, got %v", got)
	}
}

func TestTunnelLocksGrantLocksTunnel(t *testing.T) {
	l := mustTunnelLocks(t)
	if err := l.Grant("alice", "uid-1"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if !l.IsLocked("alice") {
		t.Error("tunnel should be locked after Grant")
	}
	if !l.AllowsUser("alice", "uid-1") {
		t.Error("uid-1 should be allowed")
	}
	if l.AllowsUser("alice", "uid-2") {
		t.Error("uid-2 should NOT be allowed")
	}
	if l.AllowsUser("bob", "uid-1") {
		t.Error("uid-1 must not bleed into other tunnels")
	}
}

func TestTunnelLocksGrantRejectsBadInputs(t *testing.T) {
	l := mustTunnelLocks(t)
	if err := l.Grant("BAD.SUB", "uid"); err == nil {
		t.Error("expected error for invalid subdomain")
	}
	if err := l.Grant("alice", ""); err == nil {
		t.Error("expected error for empty user_id")
	}
}

func TestTunnelLocksRevokeOneUser(t *testing.T) {
	l := mustTunnelLocks(t)
	if err := l.Grant("alice", "uid-1"); err != nil {
		t.Fatal(err)
	}
	if err := l.Grant("alice", "uid-2"); err != nil {
		t.Fatal(err)
	}
	if err := l.Revoke("alice", "uid-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !l.IsLocked("alice") {
		t.Error("tunnel must remain locked while uid-2 is on list")
	}
	if l.AllowsUser("alice", "uid-1") {
		t.Error("uid-1 should be revoked")
	}
	if !l.AllowsUser("alice", "uid-2") {
		t.Error("uid-2 should still be allowed")
	}
}

func TestTunnelLocksRevokeLastUserUnlocks(t *testing.T) {
	l := mustTunnelLocks(t)
	_ = l.Grant("alice", "uid-1")
	if err := l.Revoke("alice", "uid-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if l.IsLocked("alice") {
		t.Error("tunnel should be unlocked when last user revoked")
	}
}

func TestTunnelLocksRevokeUnknown(t *testing.T) {
	l := mustTunnelLocks(t)
	if err := l.Revoke("never-locked", "uid"); err == nil {
		t.Error("expected error revoking from unlocked tunnel")
	}
	_ = l.Grant("alice", "uid-1")
	if err := l.Revoke("alice", "uid-not-on-list"); err == nil {
		t.Error("expected error revoking unknown user")
	}
}

func TestTunnelLocksUnlock(t *testing.T) {
	l := mustTunnelLocks(t)
	_ = l.Grant("alice", "uid-1")
	_ = l.Grant("alice", "uid-2")
	if err := l.Unlock("alice"); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if l.IsLocked("alice") {
		t.Error("tunnel should be unlocked")
	}
	// Unlocking an already-unlocked tunnel is a noop, not an error.
	if err := l.Unlock("nobody"); err != nil {
		t.Errorf("Unlock on unlocked tunnel should be noop, got %v", err)
	}
}

func TestTunnelLocksAllowedUsersSorted(t *testing.T) {
	l := mustTunnelLocks(t)
	_ = l.Grant("alice", "uid-z")
	_ = l.Grant("alice", "uid-a")
	_ = l.Grant("alice", "uid-m")
	got := l.AllowedUsers("alice")
	want := []string{"uid-a", "uid-m", "uid-z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestTunnelLocksLockedSubdomainsSorted(t *testing.T) {
	l := mustTunnelLocks(t)
	_ = l.Grant("zebra", "uid-1")
	_ = l.Grant("apple", "uid-1")
	_ = l.Grant("middle", "uid-1")
	got := l.LockedSubdomains()
	want := []string{"apple", "middle", "zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestTunnelLocksRevokeUserAcrossTunnels(t *testing.T) {
	l := mustTunnelLocks(t)
	_ = l.Grant("alice", "uid-evicted")
	_ = l.Grant("alice", "uid-stays")
	_ = l.Grant("bob", "uid-evicted")
	_ = l.Grant("carol", "uid-other")

	touched := l.RevokeUserAcrossTunnels("uid-evicted")
	sort.Strings(touched)
	if !reflect.DeepEqual(touched, []string{"alice", "bob"}) {
		t.Errorf("touched: got %v", touched)
	}
	if l.AllowsUser("alice", "uid-evicted") {
		t.Error("uid-evicted should be gone from alice")
	}
	if !l.AllowsUser("alice", "uid-stays") {
		t.Error("uid-stays should remain on alice")
	}
	if l.IsLocked("bob") {
		t.Error("bob had only uid-evicted; should have unlocked")
	}
	if !l.IsLocked("carol") {
		t.Error("carol should be untouched")
	}

	// Re-revoking is a noop, returns empty.
	if got := l.RevokeUserAcrossTunnels("uid-evicted"); len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestTunnelLocksPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks.json")
	l, err := newTunnelLocks(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Grant("alice", "uid-1")
	_ = l.Grant("alice", "uid-2")
	_ = l.Grant("bob", "uid-3")

	l2, err := newTunnelLocks(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !l2.AllowsUser("alice", "uid-1") || !l2.AllowsUser("alice", "uid-2") {
		t.Error("alice's users didn't persist")
	}
	if !l2.AllowsUser("bob", "uid-3") {
		t.Error("bob's user didn't persist")
	}
}

func TestTunnelLocksLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locks.json")
	legacy := `{"locked": ["alice", "bob"]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := newTunnelLocks(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// After migration both tunnels should be locked but with empty
	// allow lists — admin must grant access manually.
	if !l.IsLocked("alice") || !l.IsLocked("bob") {
		t.Error("legacy tunnels should remain locked after migration")
	}
	if got := l.AllowedUsers("alice"); len(got) != 0 {
		t.Errorf("expected empty allowed users, got %v", got)
	}

	// File should have been rewritten — the "locked" key should be gone.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); contains(got, `"locked"`) {
		t.Errorf("legacy 'locked' key still in file: %s", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestErrInvalidSubdomainMessage(t *testing.T) {
	if errInvalidSubdomain.Error() != "invalid subdomain" {
		t.Errorf("got %q", errInvalidSubdomain.Error())
	}
}

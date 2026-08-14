package main

import (
	"path/filepath"
	"testing"
	"time"
)

func mustInviteStore(t *testing.T) *inviteStore {
	t.Helper()
	s, err := newInviteStore(filepath.Join(t.TempDir(), "invites.json"))
	if err != nil {
		t.Fatalf("newInviteStore: %v", err)
	}
	return s
}

func TestInviteIssueAndLookup(t *testing.T) {
	s := mustInviteStore(t)
	rec, err := s.Issue("Alice")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if rec.Token == "" {
		t.Error("token should be set")
	}
	if rec.Name != "Alice" {
		t.Errorf("name: got %q", rec.Name)
	}
	if rec.Consumed {
		t.Error("freshly issued invite should not be consumed")
	}
	if !rec.Expires.After(rec.Created) {
		t.Error("expires must be after created")
	}

	got := s.Lookup(rec.Token)
	if got == nil {
		t.Fatal("Lookup returned nil for fresh invite")
	}
	if got.Token != rec.Token || got.Name != "Alice" {
		t.Errorf("lookup mismatch: %+v", got)
	}

	// Mutating the returned copy must not affect the store.
	got.Name = "Mallory"
	if again := s.Lookup(rec.Token); again.Name != "Alice" {
		t.Errorf("Lookup must return a copy, got %q", again.Name)
	}
}

func TestInviteIssueRejectsEmptyName(t *testing.T) {
	s := mustInviteStore(t)
	if _, err := s.Issue("   "); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestInviteLookupExpired(t *testing.T) {
	s := mustInviteStore(t)
	rec, _ := s.Issue("Alice")
	// Force-expire it.
	s.mu.Lock()
	s.byToken[rec.Token].Expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if s.Lookup(rec.Token) != nil {
		t.Error("expired invite should look up as nil")
	}
}

func TestInviteLookupUnknown(t *testing.T) {
	s := mustInviteStore(t)
	if got := s.Lookup("definitely-not-a-token"); got != nil {
		t.Errorf("got %+v want nil", got)
	}
}

func TestInviteMarkConsumed(t *testing.T) {
	s := mustInviteStore(t)
	rec, _ := s.Issue("Alice")
	if err := s.MarkConsumed(rec.Token, "user-id-42"); err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}
	got := s.Lookup(rec.Token)
	if got == nil {
		t.Fatal("consumed invite should still be lookup-able")
	}
	if !got.Consumed || got.UserID != "user-id-42" {
		t.Errorf("expected consumed with userID: got %+v", got)
	}
	// Second consume must fail.
	if err := s.MarkConsumed(rec.Token, "other-user"); err == nil {
		t.Error("expected error on double-consume")
	}
}

func TestInviteMarkConsumedUnknown(t *testing.T) {
	s := mustInviteStore(t)
	if err := s.MarkConsumed("not-a-token", "uid"); err == nil {
		t.Error("expected error for unknown token")
	}
}

func TestInviteDelete(t *testing.T) {
	s := mustInviteStore(t)
	rec, _ := s.Issue("Alice")
	if err := s.Delete(rec.Token); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Lookup(rec.Token) != nil {
		t.Error("deleted invite should not be found")
	}
	if err := s.Delete(rec.Token); err == nil {
		t.Error("expected error deleting twice")
	}
}

func TestInviteListIncludesAndExcludesConsumed(t *testing.T) {
	s := mustInviteStore(t)
	a, _ := s.Issue("Alice")
	time.Sleep(2 * time.Millisecond) // ensure ordering
	b, _ := s.Issue("Bob")
	if err := s.MarkConsumed(a.Token, "uid-a"); err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}

	all := s.List(true)
	if len(all) != 2 {
		t.Errorf("expected 2 with includeConsumed, got %d", len(all))
	}
	if !all[0].Created.Before(all[1].Created) && !all[0].Created.Equal(all[1].Created) {
		t.Error("List should return creation order")
	}

	pending := s.List(false)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	if pending[0].Token != b.Token {
		t.Errorf("expected Bob, got %s", pending[0].Name)
	}
}

func TestInvitePendingForName(t *testing.T) {
	s := mustInviteStore(t)
	a, _ := s.Issue("Alice")
	if got := s.PendingForName("alice"); got == nil || got.Token != a.Token {
		t.Errorf("case-insensitive lookup failed: %+v", got)
	}
	if got := s.PendingForName("  ALICE  "); got == nil {
		t.Error("name should be trimmed and lowercased")
	}
	if got := s.PendingForName("nobody"); got != nil {
		t.Errorf("expected nil for unknown name, got %+v", got)
	}

	// Once consumed, PendingForName returns nil.
	_ = s.MarkConsumed(a.Token, "uid")
	if got := s.PendingForName("alice"); got != nil {
		t.Errorf("expected nil after consume, got %+v", got)
	}

	// Expired invites should not show as pending.
	b, _ := s.Issue("Bob")
	s.mu.Lock()
	s.byToken[b.Token].Expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got := s.PendingForName("bob"); got != nil {
		t.Error("expected nil for expired invite")
	}
}

func TestInvitePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invites.json")
	s, err := newInviteStore(path)
	if err != nil {
		t.Fatalf("newInviteStore: %v", err)
	}
	rec, err := s.Issue("Alice")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := s.MarkConsumed(rec.Token, "uid-1"); err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}

	// Reload from disk.
	s2, err := newInviteStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s2.Lookup(rec.Token)
	if got == nil {
		t.Fatal("reloaded store missing invite")
	}
	if !got.Consumed || got.UserID != "uid-1" {
		t.Errorf("reloaded record lost state: %+v", got)
	}
}

func TestInviteTokensAreUnique(t *testing.T) {
	s := mustInviteStore(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		rec, err := s.Issue("user")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if seen[rec.Token] {
			t.Fatalf("duplicate token: %s", rec.Token)
		}
		seen[rec.Token] = true
	}
}

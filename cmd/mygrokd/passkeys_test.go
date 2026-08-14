package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func mustPasskeyStore(t *testing.T) *passkeyStore {
	t.Helper()
	s, err := newPasskeyStore(filepath.Join(t.TempDir(), "pk.json"))
	if err != nil {
		t.Fatalf("newPasskeyStore: %v", err)
	}
	return s
}

func TestPasskeyCreateAndFindUser(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("  Alice  ")
	if u.ID == "" {
		t.Error("user ID must be set")
	}
	if u.Name != "Alice" {
		t.Errorf("name should be trimmed; got %q", u.Name)
	}
	if got := s.FindUser(u.ID); got == nil || got.Name != "Alice" {
		t.Errorf("FindUser: %+v", got)
	}
	if got := s.FindUser("nope"); got != nil {
		t.Errorf("FindUser unknown: %+v", got)
	}
	if got := s.FindUserByName("alice"); got == nil || got.ID != u.ID {
		t.Errorf("case-insensitive name lookup failed: %+v", got)
	}
	if got := s.FindUserByName("nobody"); got != nil {
		t.Errorf("FindUserByName unknown: %+v", got)
	}
}

func TestPasskeyListUsersSortedByCreation(t *testing.T) {
	s := mustPasskeyStore(t)
	a := s.CreateUser("Alice")
	time.Sleep(2 * time.Millisecond)
	b := s.CreateUser("Bob")
	time.Sleep(2 * time.Millisecond)
	c := s.CreateUser("Carol")

	users := s.ListUsers()
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if users[0].ID != a.ID || users[1].ID != b.ID || users[2].ID != c.ID {
		t.Errorf("creation order not preserved: %v", users)
	}
}

func TestPasskeyDeleteUserRemovesCredentials(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")
	credID := []byte{0x01, 0x02, 0x03}
	if err := s.AddCredential(u.ID, "yubikey", &webauthn.Credential{ID: credID}); err != nil {
		t.Fatalf("AddCredential: %v", err)
	}
	if !s.HasCredentials() {
		t.Error("HasCredentials should be true")
	}
	if err := s.DeleteUser(u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if s.FindUser(u.ID) != nil {
		t.Error("user should be gone")
	}
	if s.HasCredentials() {
		t.Error("credentials should be deleted with user")
	}
	if err := s.DeleteUser(u.ID); err == nil {
		t.Error("expected error deleting again")
	}
}

func TestPasskeyAddCredentialRejectsUnknownUserAndDuplicates(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")

	if err := s.AddCredential("not-a-user", "k", &webauthn.Credential{ID: []byte{1}}); err == nil {
		t.Error("expected error for unknown user")
	}
	if err := s.AddCredential(u.ID, "k", &webauthn.Credential{ID: []byte{1}}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := s.AddCredential(u.ID, "k2", &webauthn.Credential{ID: []byte{1}}); err == nil {
		t.Error("expected error for duplicate credential ID")
	}
}

func TestPasskeyListCredentialsFilter(t *testing.T) {
	s := mustPasskeyStore(t)
	a := s.CreateUser("Alice")
	b := s.CreateUser("Bob")
	_ = s.AddCredential(a.ID, "a1", &webauthn.Credential{ID: []byte{0x10}})
	_ = s.AddCredential(a.ID, "a2", &webauthn.Credential{ID: []byte{0x11}})
	_ = s.AddCredential(b.ID, "b1", &webauthn.Credential{ID: []byte{0x20}})

	if got := s.ListCredentials(""); len(got) != 3 {
		t.Errorf("expected 3 total, got %d", len(got))
	}
	if got := s.ListCredentials(a.ID); len(got) != 2 {
		t.Errorf("expected 2 for alice, got %d", len(got))
	}
	if got := s.ListCredentials(b.ID); len(got) != 1 {
		t.Errorf("expected 1 for bob, got %d", len(got))
	}
}

func TestPasskeyDeleteCredential(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")
	rawID := []byte{0xAA, 0xBB}
	if err := s.AddCredential(u.ID, "k", &webauthn.Credential{ID: rawID}); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(rawID)
	if err := s.DeleteCredential(id); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if s.HasCredentials() {
		t.Error("HasCredentials should be false after delete")
	}
	if err := s.DeleteCredential(id); err == nil {
		t.Error("expected error deleting absent credential")
	}
}

func TestPasskeyCredentialUserID(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")
	rawID := []byte{1, 2, 3, 4}
	_ = s.AddCredential(u.ID, "k", &webauthn.Credential{ID: rawID})

	if got := s.CredentialUserID(rawID); got != u.ID {
		t.Errorf("got %q want %q", got, u.ID)
	}
	if got := s.CredentialUserID([]byte{9, 9}); got != "" {
		t.Errorf("got %q want empty", got)
	}
}

func TestPasskeySessionLifecycle(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")
	if got := s.SessionUser(""); got != "" {
		t.Error("empty session id should return empty")
	}
	if got := s.SessionUser("never-issued"); got != "" {
		t.Error("unknown session id should return empty")
	}
	sid := s.issueAuthSession(u.ID)
	if sid == "" {
		t.Fatal("session id is empty")
	}
	if got := s.SessionUser(sid); got != u.ID {
		t.Errorf("SessionUser: got %q want %q", got, u.ID)
	}

	// Force-expire it.
	s.mu.Lock()
	e := s.authSessions[sid]
	e.Expires = time.Now().Add(-time.Second)
	s.authSessions[sid] = e
	s.mu.Unlock()
	if got := s.SessionUser(sid); got != "" {
		t.Error("expired session should return empty")
	}
}

func TestPasskeyRevokeSession(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")
	sid := s.issueAuthSession(u.ID)
	s.RevokeSession(sid)
	if got := s.SessionUser(sid); got != "" {
		t.Errorf("revoked session should be gone, got %q", got)
	}
}

func TestPasskeyRevokeUserSessions(t *testing.T) {
	s := mustPasskeyStore(t)
	a := s.CreateUser("Alice")
	b := s.CreateUser("Bob")
	asid := s.issueAuthSession(a.ID)
	bsid := s.issueAuthSession(b.ID)

	s.RevokeUserSessions(a.ID)
	if got := s.SessionUser(asid); got != "" {
		t.Error("alice's session should be revoked")
	}
	if got := s.SessionUser(bsid); got != b.ID {
		t.Errorf("bob's session must survive; got %q", got)
	}
}

func TestPasskeyRevokeAllSessions(t *testing.T) {
	s := mustPasskeyStore(t)
	u := s.CreateUser("Alice")
	sid1 := s.issueAuthSession(u.ID)
	sid2 := s.issueAuthSession(u.ID)
	s.RevokeAllSessions()
	if s.SessionUser(sid1) != "" || s.SessionUser(sid2) != "" {
		t.Error("RevokeAllSessions should clear everything")
	}
}

func TestPasskeyRegistrationPending(t *testing.T) {
	s := mustPasskeyStore(t)
	data := &webauthn.SessionData{Challenge: "abc"}
	id := s.issueRegistration(data, "invite-tok")
	if id == "" {
		t.Fatal("registration id is empty")
	}
	got, token := s.consumeRegistration(id)
	if got == nil || got.Challenge != "abc" || token != "invite-tok" {
		t.Errorf("consumeRegistration: %+v / %q", got, token)
	}
	// Second consume yields nothing.
	if got, _ := s.consumeRegistration(id); got != nil {
		t.Error("expected nil on second consume")
	}
}

func TestPasskeyRegistrationExpires(t *testing.T) {
	s := mustPasskeyStore(t)
	id := s.issueRegistration(&webauthn.SessionData{}, "")
	s.mu.Lock()
	s.regSessions[id].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if got, _ := s.consumeRegistration(id); got != nil {
		t.Error("expired registration should be nil")
	}
	// Should also be evicted.
	s.mu.RLock()
	_, stillThere := s.regSessions[id]
	s.mu.RUnlock()
	if stillThere {
		t.Error("expired registration should be evicted from map")
	}
}

func TestPasskeyLoginPending(t *testing.T) {
	s := mustPasskeyStore(t)
	data := &webauthn.SessionData{Challenge: "x"}
	id := s.issueLogin(data, "https://example.com/back")
	got, url := s.consumeLogin(id)
	if got == nil || got.Challenge != "x" || url != "https://example.com/back" {
		t.Errorf("consumeLogin: %+v / %q", got, url)
	}
}

func TestPasskeyReadCookieFromHeaders(t *testing.T) {
	hdrs := []byte("GET / HTTP/1.1\r\nCookie: foo=bar; " + pkSessionCookie + "=abc; baz=qux\r\n\r\n")
	if got := readPKSessionFromCookies(hdrs); got != "abc" {
		t.Errorf("got %q want %q", got, "abc")
	}
	if got := readCookieFromHeaders([]byte("GET / HTTP/1.1\r\n\r\n"), pkSessionCookie); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestPasskeyConstantTimeStringEq(t *testing.T) {
	if !constantTimeStringEq("abc", "abc") {
		t.Error("equal strings should match")
	}
	if constantTimeStringEq("abc", "abd") {
		t.Error("different strings should not match")
	}
	// Different lengths still resolve safely to false.
	if constantTimeStringEq("abc", "abcd") {
		t.Error("different-length strings should not match")
	}
}

func TestPasskeyWriteAuthSessionCookieHeader(t *testing.T) {
	got := writeAuthSessionCookieHeader("sid-1", "example.com")
	want := pkSessionCookie + "=sid-1; Domain=.example.com; Path=/; Max-Age="
	if len(got) <= len(want) || got[:len(want)] != want {
		t.Errorf("got %q want prefix %q", got, want)
	}
	for _, attr := range []string{"HttpOnly", "Secure", "SameSite=Lax"} {
		if !contains(got, attr) {
			t.Errorf("missing %q in %q", attr, got)
		}
	}
}

func TestPasskeyRandomBytesUnique(t *testing.T) {
	a := randomBytes(16)
	b := randomBytes(16)
	if len(a) != 16 || len(b) != 16 {
		t.Errorf("len(a)=%d len(b)=%d want 16", len(a), len(b))
	}
	same := true
	for i := range a {
		if a[i] != b[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("two randomBytes calls returned identical bytes — vanishingly unlikely")
	}
}

func TestPasskeyNewUserIDFormat(t *testing.T) {
	id := newUserID()
	if id == "" {
		t.Fatal("empty id")
	}
	// 32 bytes base64-raw-encoded → 43 chars.
	if len(id) != 43 {
		t.Errorf("len(id)=%d want 43 (32 bytes raw base64)", len(id))
	}
}

func TestPasskeyPersistenceAndLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pk.json")

	legacy := `{
  "user_id": "legacy-user-id",
  "credentials": [
    {"id": "deadbeef", "user_id": "", "label": "old-key", "credential": {"id":"3q2+7w=="}}
  ]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := newPasskeyStore(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := s.ListUsers(); len(got) != 1 || got[0].ID != "legacy-user-id" {
		t.Errorf("legacy user not promoted: %+v", got)
	}
	creds := s.ListCredentials("legacy-user-id")
	if len(creds) != 1 {
		t.Errorf("expected 1 credential attached to legacy user, got %d", len(creds))
	}
}

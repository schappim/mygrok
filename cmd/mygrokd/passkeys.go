package main

// Passkey (WebAuthn) integration with a multi-user model.
//
// Users (admin-created via invites) own one or more WebAuthn credentials.
// Tunnels can be gated by a per-tunnel `allowed_users` list — only those
// users' credentials can pass the assertion. Sessions are user-tagged so
// the gate knows which user is in front of it.
//
// Cookie scoping: mygrok_pk is set with Domain=.<publicHost> so it
// covers every <sub>.<publicHost> tunnel. Custom hostnames (CNAMEd
// third-party domains) are out of scope — different origin in the
// browser model.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// pkUserRecord is one user known to the system.
type pkUserRecord struct {
	ID      string    `json:"id"`   // base64-raw of the WebAuthn user handle
	Name    string    `json:"name"` // display name shown on invites / admin UI
	Created time.Time `json:"created"`
}

// storedCredential is the on-disk shape. UserID ties the credential to
// a pkUserRecord.
type storedCredential struct {
	ID         string              `json:"id"`
	UserID     string              `json:"user_id"`
	Label      string              `json:"label"`
	Created    time.Time           `json:"created"`
	LastUsedAt time.Time           `json:"last_used_at,omitempty"`
	Credential webauthn.Credential `json:"credential"`
}

type passkeyFile struct {
	Users       []pkUserRecord     `json:"users"`
	Credentials []storedCredential `json:"credentials"`

	// Legacy fields, only read on first load (pre-multi-user files).
	LegacyUserID string `json:"user_id,omitempty"`
}

type passkeyStore struct {
	mu          sync.RWMutex
	path        string
	users       []pkUserRecord
	credentials []storedCredential

	regSessions   map[string]*webauthnPending
	loginSessions map[string]*webauthnPending

	// authSessions: cookie-id → (user_id, expiry).
	authSessions map[string]authSessionEntry
}

type authSessionEntry struct {
	UserID  string
	Expires time.Time
}

type webauthnPending struct {
	data      *webauthn.SessionData
	expires   time.Time
	returnURL string
	// for invite flows: associates a pending registration with the
	// invite (and therefore the user being created).
	inviteToken string
}

const (
	pkSessionTTL    = 24 * time.Hour
	pkPendingTTL    = 5 * time.Minute
	pkSessionCookie = "mygrok_pk"
	pkLoginCookie   = "mygrok_pk_login"
	pkRegCookie     = "mygrok_pk_reg"

	// legacyUserName is the display name applied to credentials
	// migrated from the old single-user format. Visible in admin UI.
	legacyUserName = "owner"
)

func newPasskeyStore(path string) (*passkeyStore, error) {
	s := &passkeyStore{
		path:          path,
		regSessions:   map[string]*webauthnPending{},
		loginSessions: map[string]*webauthnPending{},
		authSessions:  map[string]authSessionEntry{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var f passkeyFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	s.users = f.Users
	s.credentials = f.Credentials

	// Migrate legacy single-user file into the multi-user shape.
	if f.LegacyUserID != "" {
		legacyID := f.LegacyUserID
		hasUser := false
		for _, u := range s.users {
			if u.ID == legacyID {
				hasUser = true
				break
			}
		}
		if !hasUser {
			s.users = append(s.users, pkUserRecord{
				ID: legacyID, Name: legacyUserName, Created: time.Now(),
			})
		}
		for i := range s.credentials {
			if s.credentials[i].UserID == "" {
				s.credentials[i].UserID = legacyID
			}
		}
		s.mu.Lock()
		_ = s.saveLocked()
		s.mu.Unlock()
	}
	return s, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func newUserID() string {
	return base64.RawStdEncoding.EncodeToString(randomBytes(32))
}

func (s *passkeyStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(dirOf(s.path), 0o755); err != nil {
		return err
	}
	out := passkeyFile{Users: s.users, Credentials: s.credentials}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// --- user accessors -------------------------------------------------------

// CreateUser adds a new user with a freshly-generated handle and the
// supplied display name. Caller must ensure name uniqueness if needed.
func (s *passkeyStore) CreateUser(name string) pkUserRecord {
	rec := pkUserRecord{
		ID:      newUserID(),
		Name:    strings.TrimSpace(name),
		Created: time.Now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = append(s.users, rec)
	_ = s.saveLocked()
	return rec
}

// DeleteUser removes a user and *all* of their credentials. Sessions
// owned by the user remain in memory until they expire — call
// RevokeUserSessions if you want immediate kick-out.
func (s *passkeyStore) DeleteUser(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	users := s.users[:0]
	for _, u := range s.users {
		if u.ID == userID {
			found = true
			continue
		}
		users = append(users, u)
	}
	if !found {
		return fmt.Errorf("no user with id %q", userID)
	}
	s.users = users
	creds := s.credentials[:0]
	for _, c := range s.credentials {
		if c.UserID == userID {
			continue
		}
		creds = append(creds, c)
	}
	s.credentials = creds
	return s.saveLocked()
}

// ListUsers returns all known users sorted by creation time.
func (s *passkeyStore) ListUsers() []pkUserRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]pkUserRecord, len(s.users))
	copy(out, s.users)
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// FindUser returns a user by ID or nil.
func (s *passkeyStore) FindUser(userID string) *pkUserRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.users {
		if s.users[i].ID == userID {
			u := s.users[i]
			return &u
		}
	}
	return nil
}

// FindUserByName returns the first user with matching name (case-
// insensitive) or nil.
func (s *passkeyStore) FindUserByName(name string) *pkUserRecord {
	target := strings.ToLower(strings.TrimSpace(name))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.users {
		if strings.ToLower(s.users[i].Name) == target {
			u := s.users[i]
			return &u
		}
	}
	return nil
}

// HasCredentials reports whether any credential is registered.
func (s *passkeyStore) HasCredentials() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.credentials) > 0
}

// ListCredentials (optionally filtered to one user) for display.
func (s *passkeyStore) ListCredentials(userID string) []storedCredential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]storedCredential, 0, len(s.credentials))
	for _, c := range s.credentials {
		if userID == "" || c.UserID == userID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// AddCredential persists a credential under the given user.
func (s *passkeyStore) AddCredential(userID, label string, c *webauthn.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := hex.EncodeToString(c.ID)
	for _, e := range s.credentials {
		if e.ID == id {
			return fmt.Errorf("credential already registered")
		}
	}
	hasUser := false
	for _, u := range s.users {
		if u.ID == userID {
			hasUser = true
			break
		}
	}
	if !hasUser {
		return fmt.Errorf("unknown user %q", userID)
	}
	s.credentials = append(s.credentials, storedCredential{
		ID:         id,
		UserID:     userID,
		Label:      label,
		Created:    time.Now(),
		Credential: *c,
	})
	return s.saveLocked()
}

// DeleteCredential removes a credential by hex ID.
func (s *passkeyStore) DeleteCredential(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.credentials[:0]
	found := false
	for _, c := range s.credentials {
		if c.ID == id {
			found = true
			continue
		}
		out = append(out, c)
	}
	if !found {
		return fmt.Errorf("no credential with id %q", id)
	}
	s.credentials = out
	return s.saveLocked()
}

// UpdateCredential is called after a successful assertion to advance
// the sign counter and bump LastUsedAt.
func (s *passkeyStore) UpdateCredential(c *webauthn.Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := hex.EncodeToString(c.ID)
	for i, e := range s.credentials {
		if e.ID == id {
			s.credentials[i].Credential = *c
			s.credentials[i].LastUsedAt = time.Now()
			_ = s.saveLocked()
			return
		}
	}
}

// CredentialUserID returns the user_id that owns a given credential,
// looked up by raw credential bytes (the WebAuthn rawID).
func (s *passkeyStore) CredentialUserID(rawID []byte) string {
	id := hex.EncodeToString(rawID)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.credentials {
		if c.ID == id {
			return c.UserID
		}
	}
	return ""
}

// --- webauthn.User implementation ----------------------------------------

type pkUser struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

func (u *pkUser) WebAuthnID() []byte                         { return u.id }
func (u *pkUser) WebAuthnName() string                       { return u.name }
func (u *pkUser) WebAuthnDisplayName() string                { return u.name }
func (u *pkUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// userByID returns a webauthn.User for the given user_id, populated
// with that user's credentials. Returns nil if no such user.
func (s *passkeyStore) userByID(userID string) *pkUser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var rec *pkUserRecord
	for i := range s.users {
		if s.users[i].ID == userID {
			rec = &s.users[i]
			break
		}
	}
	if rec == nil {
		return nil
	}
	idBytes, err := base64.RawStdEncoding.DecodeString(rec.ID)
	if err != nil {
		return nil
	}
	creds := []webauthn.Credential{}
	for _, c := range s.credentials {
		if c.UserID == userID {
			creds = append(creds, c.Credential)
		}
	}
	return &pkUser{id: idBytes, name: rec.Name, creds: creds}
}

// allCredentialsForLogin returns all stored credentials regardless of
// owner; used to seed BeginLogin's allowed-credentials list when we
// don't know upfront which user is at the keyboard.
func (s *passkeyStore) allCredentialsForLogin() []webauthn.Credential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]webauthn.Credential, 0, len(s.credentials))
	for _, c := range s.credentials {
		out = append(out, c.Credential)
	}
	return out
}

// --- pending session helpers --------------------------------------------

func (s *passkeyStore) issueRegistration(data *webauthn.SessionData, inviteToken string) string {
	id := hex.EncodeToString(randomBytes(16))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regSessions[id] = &webauthnPending{
		data: data, expires: time.Now().Add(pkPendingTTL),
		inviteToken: inviteToken,
	}
	return id
}

func (s *passkeyStore) consumeRegistration(id string) (*webauthn.SessionData, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.regSessions[id]
	if !ok || time.Now().After(p.expires) {
		delete(s.regSessions, id)
		return nil, ""
	}
	delete(s.regSessions, id)
	return p.data, p.inviteToken
}

func (s *passkeyStore) issueLogin(data *webauthn.SessionData, returnURL string) string {
	id := hex.EncodeToString(randomBytes(16))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginSessions[id] = &webauthnPending{data: data, expires: time.Now().Add(pkPendingTTL), returnURL: returnURL}
	return id
}

func (s *passkeyStore) consumeLogin(id string) (*webauthn.SessionData, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.loginSessions[id]
	if !ok || time.Now().After(p.expires) {
		delete(s.loginSessions, id)
		return nil, ""
	}
	delete(s.loginSessions, id)
	return p.data, p.returnURL
}

// --- tunnel-access sessions ---------------------------------------------

func (s *passkeyStore) issueAuthSession(userID string) string {
	id := hex.EncodeToString(randomBytes(32))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authSessions[id] = authSessionEntry{
		UserID:  userID,
		Expires: time.Now().Add(pkSessionTTL),
	}
	return id
}

// SessionUser returns the user_id that owns the given session cookie,
// or "" if the session is missing/expired.
func (s *passkeyStore) SessionUser(id string) string {
	if id == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.authSessions[id]
	if !ok || time.Now().After(e.Expires) {
		delete(s.authSessions, id)
		return ""
	}
	return e.UserID
}

func (s *passkeyStore) RevokeSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.authSessions, id)
}

func (s *passkeyStore) RevokeUserSessions(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.authSessions {
		if e.UserID == userID {
			delete(s.authSessions, id)
		}
	}
}

func (s *passkeyStore) RevokeAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authSessions = map[string]authSessionEntry{}
}

// --- WebAuthn factory ---------------------------------------------------

func buildWebAuthn(publicHost string) (*webauthn.WebAuthn, error) {
	cfg := &webauthn.Config{
		RPDisplayName: "mygrok",
		RPID:          publicHost,
		RPOrigins:     []string{"https://tunnel." + publicHost},
	}
	return webauthn.New(cfg)
}

// --- HTTP cookie helpers ------------------------------------------------

func writeAuthSessionCookieHeader(sid, publicHost string) string {
	return fmt.Sprintf("%s=%s; Domain=.%s; Path=/; Max-Age=%d; HttpOnly; Secure; SameSite=Lax",
		pkSessionCookie, sid, publicHost, int(pkSessionTTL.Seconds()))
}

func readPKSessionFromCookies(rawHeaders []byte) string {
	return readCookieFromHeaders(rawHeaders, pkSessionCookie)
}

func readCookieFromHeaders(rawHeaders []byte, name string) string {
	for _, line := range strings.Split(string(rawHeaders), "\r\n") {
		if !strings.HasPrefix(strings.ToLower(line), "cookie:") {
			continue
		}
		v := strings.TrimSpace(line[len("cookie:"):])
		for _, c := range strings.Split(v, ";") {
			c = strings.TrimSpace(c)
			eq := strings.IndexByte(c, '=')
			if eq < 0 {
				continue
			}
			if c[:eq] == name {
				return c[eq+1:]
			}
		}
	}
	return ""
}

func constantTimeStringEq(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func httpRequestFromBuffered(rawHeaders []byte, br *bufio.Reader) (*http.Request, error) {
	combined := io.MultiReader(bytes.NewReader(rawHeaders), br)
	return http.ReadRequest(bufio.NewReader(combined))
}

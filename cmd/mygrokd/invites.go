package main

// Invite store: admin pre-creates an invite tagged with a user name,
// shares the resulting URL out-of-band, and the invitee redeems it by
// registering a passkey at /invite/<token>. The invite is one-shot
// (consumed=true after the credential lands) and has a 7-day default
// expiry to limit blast radius if the link leaks.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const inviteDefaultTTL = 7 * 24 * time.Hour

type inviteRecord struct {
	Token      string    `json:"token"`
	Name       string    `json:"name"`    // display name shown on the invite page
	UserID     string    `json:"user_id"` // user_id created at issue time, populated when consumed
	Created    time.Time `json:"created"`
	Expires    time.Time `json:"expires"`
	Consumed   bool      `json:"consumed"`
	ConsumedAt time.Time `json:"consumed_at,omitempty"`
}

type inviteFile struct {
	Invites []inviteRecord `json:"invites"`
}

type inviteStore struct {
	mu      sync.RWMutex
	path    string
	byToken map[string]*inviteRecord
}

func newInviteStore(path string) (*inviteStore, error) {
	s := &inviteStore{path: path, byToken: map[string]*inviteRecord{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var f inviteFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i := range f.Invites {
		rec := f.Invites[i]
		s.byToken[rec.Token] = &rec
	}
	return s, nil
}

func (s *inviteStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(dirOf(s.path), 0o755); err != nil {
		return err
	}
	out := inviteFile{}
	for _, r := range s.byToken {
		out.Invites = append(out.Invites, *r)
	}
	sort.Slice(out.Invites, func(i, j int) bool {
		return out.Invites[i].Created.Before(out.Invites[j].Created)
	})
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

// Issue creates a new invite for the given display name. Returns the
// invite record (including the token for the URL).
func (s *inviteStore) Issue(name string) (inviteRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return inviteRecord{}, fmt.Errorf("name required")
	}
	rec := inviteRecord{
		Token:   hex.EncodeToString(randomBytes(24)),
		Name:    name,
		Created: time.Now(),
		Expires: time.Now().Add(inviteDefaultTTL),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byToken[rec.Token] = &rec
	if err := s.saveLocked(); err != nil {
		delete(s.byToken, rec.Token)
		return inviteRecord{}, err
	}
	return rec, nil
}

// Lookup returns a copy of the invite for the given token, or nil if
// it doesn't exist / is expired.
func (s *inviteStore) Lookup(token string) *inviteRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.byToken[token]
	if !ok {
		return nil
	}
	if time.Now().After(rec.Expires) {
		return nil
	}
	cp := *rec
	return &cp
}

// MarkConsumed flags an invite as redeemed and stores the user_id
// that was created.
func (s *inviteStore) MarkConsumed(token, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byToken[token]
	if !ok {
		return fmt.Errorf("unknown invite")
	}
	if rec.Consumed {
		return fmt.Errorf("invite already used")
	}
	rec.Consumed = true
	rec.ConsumedAt = time.Now()
	rec.UserID = userID
	return s.saveLocked()
}

// Delete removes an invite (admin-revoke).
func (s *inviteStore) Delete(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byToken[token]; !ok {
		return fmt.Errorf("no invite with token")
	}
	delete(s.byToken, token)
	return s.saveLocked()
}

// List returns invites in creation order. Set includeConsumed=false
// to skip already-redeemed ones.
func (s *inviteStore) List(includeConsumed bool) []inviteRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]inviteRecord, 0, len(s.byToken))
	for _, r := range s.byToken {
		if !includeConsumed && r.Consumed {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Pending reports whether an unconsumed, unexpired invite exists for
// a given name (used to avoid issuing duplicates without intent).
func (s *inviteStore) PendingForName(name string) *inviteRecord {
	target := strings.ToLower(strings.TrimSpace(name))
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.byToken {
		if r.Consumed || now.After(r.Expires) {
			continue
		}
		if strings.ToLower(r.Name) == target {
			cp := *r
			return &cp
		}
	}
	return nil
}

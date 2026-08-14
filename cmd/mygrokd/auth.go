package main

// HTTP handlers for passkey registration (admin + invite) and login.
// Mounted under tunnel.<publicHost> by serveAdmin's switch.
//
// Three flows:
//   1. Admin-driven registration via /admin/passkeys (admin auth) —
//      adds a credential to the legacy "owner" user. Useful for the
//      first credential on a fresh install.
//   2. Invite-driven registration via /invite/<token> (no auth, the
//      token is the auth) — admin pre-tags an invitee's name; redeem
//      creates a new user + their first credential atomically.
//   3. Visitor login via /auth (discoverable assertion). Browser shows
//      its registered passkeys; user picks one; server identifies the
//      user from the credential's userHandle and issues a user-tagged
//      session cookie.

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// --- /admin/passkeys (admin-driven first-credential registration) -------

func serveAdminPasskeys(conn net.Conn, method string, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	_ = method
	_ = br
	if wantsJSON(rawHeaders) || strings.Contains(string(rawHeaders), "format=json") {
		type credOut struct {
			ID         string `json:"id"`
			UserID     string `json:"user_id"`
			Label      string `json:"label"`
			Created    string `json:"created"`
			LastUsedAt string `json:"last_used_at,omitempty"`
		}
		list := []credOut{}
		for _, c := range pks.ListCredentials("") {
			lu := ""
			if !c.LastUsedAt.IsZero() {
				lu = c.LastUsedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			list = append(list, credOut{
				ID: c.ID, UserID: c.UserID, Label: c.Label,
				Created:    c.Created.UTC().Format("2006-01-02T15:04:05Z"),
				LastUsedAt: lu,
			})
		}
		body, _ := json.Marshal(list)
		writeHTTP(conn, 200, "application/json", body)
		return
	}
	page := renderAdminPasskeysPage(pks, "", "")
	writeHTTP(conn, 200, "text/html; charset=utf-8", page)
}

// adminPasskeyOwnerID returns (or creates) a legacy "owner" user for
// first-credential admin self-registration. We don't go through the
// invite flow for this — admin already has a higher-trust credential
// (the auth token).
func adminPasskeyOwnerID(pks *passkeyStore) string {
	if u := pks.FindUserByName(legacyUserName); u != nil {
		return u.ID
	}
	return pks.CreateUser(legacyUserName).ID
}

func serveAdminPasskeyBegin(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	uid := adminPasskeyOwnerID(pks)
	user := pks.userByID(uid)
	creation, sess, err := wa.BeginRegistration(user, registrationOpts()...)
	if err != nil {
		writeJSONErr(conn, 500, "begin registration: "+err.Error())
		return
	}
	sid := pks.issueRegistration(sess, "")
	body, _ := json.Marshal(creation)
	writeHTTPWithCookie(conn, 200, "application/json", body, fmt.Sprintf(
		"%s=%s; Path=/; Max-Age=%d; HttpOnly; Secure; SameSite=Lax",
		pkRegCookie, sid, int(pkPendingTTL.Seconds())))
}

func serveAdminPasskeyFinish(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	finishRegistration(conn, rawHeaders, br, pks, wa, adminPasskeyOwnerID(pks))
}

func serveAdminPasskeyDelete(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		writeHTTPRedirect(conn, "/admin/passkeys")
		return
	}
	id := form.Get("id")
	if id == "" {
		writeHTTPRedirect(conn, "/admin/passkeys")
		return
	}
	_ = pks.DeleteCredential(id)
	writeHTTPRedirect(conn, "/admin/passkeys")
}

// --- /admin/users -------------------------------------------------------

func serveAdminUsers(conn net.Conn, method string, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, locks *tunnelLocks, invites *inviteStore) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	if wantsJSON(rawHeaders) {
		type out struct {
			ID          string   `json:"id"`
			Name        string   `json:"name"`
			Created     string   `json:"created"`
			Credentials int      `json:"credentials"`
			Tunnels     []string `json:"tunnels"`
		}
		list := []out{}
		for _, u := range pks.ListUsers() {
			creds := len(pks.ListCredentials(u.ID))
			var subs []string
			for _, sub := range locks.LockedSubdomains() {
				if locks.AllowsUser(sub, u.ID) {
					subs = append(subs, sub)
				}
			}
			list = append(list, out{
				ID: u.ID, Name: u.Name,
				Created:     u.Created.UTC().Format("2006-01-02T15:04:05Z"),
				Credentials: creds, Tunnels: subs,
			})
		}
		body, _ := json.Marshal(list)
		writeHTTP(conn, 200, "application/json", body)
		return
	}
	page := renderAdminUsersPage(pks, locks, invites)
	writeHTTP(conn, 200, "text/html; charset=utf-8", page)
}

func serveAdminUsersDelete(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, locks *tunnelLocks) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		writeHTTPRedirect(conn, "/admin/users")
		return
	}
	uid := form.Get("user_id")
	if uid == "" {
		writeHTTPRedirect(conn, "/admin/users")
		return
	}
	_ = locks.RevokeUserAcrossTunnels(uid)
	pks.RevokeUserSessions(uid)
	_ = pks.DeleteUser(uid)
	if wantsJSON(rawHeaders) {
		writeHTTP(conn, 200, "application/json", []byte(`{"ok":true}`))
		return
	}
	writeHTTPRedirect(conn, "/admin/users")
}

// --- /admin/invites -----------------------------------------------------

func serveAdminInvites(conn net.Conn, method string, rawHeaders []byte, br *bufio.Reader, invites *inviteStore) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	asJSON := wantsJSON(rawHeaders)

	// POST: mutate first (revoke today; "create" too via the form UI).
	if method == "POST" {
		flash, class := applyInviteMutation(invites, rawHeaders, br)
		if asJSON {
			writeAdminMutateJSON(conn, flash, class)
			return
		}
		writeHTTP(conn, 200, "text/html; charset=utf-8",
			renderAdminInvitesPage(invites, flash, class))
		return
	}

	// GET: list.
	if asJSON {
		type out struct {
			Token      string `json:"token"`
			URL        string `json:"url"`
			Name       string `json:"name"`
			UserID     string `json:"user_id,omitempty"`
			Created    string `json:"created"`
			Expires    string `json:"expires"`
			Consumed   bool   `json:"consumed"`
			ConsumedAt string `json:"consumed_at,omitempty"`
		}
		list := []out{}
		for _, r := range invites.List(true) {
			cu := ""
			if !r.ConsumedAt.IsZero() {
				cu = r.ConsumedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			list = append(list, out{
				Token:      r.Token,
				URL:        inviteURL(r.Token),
				Name:       r.Name,
				UserID:     r.UserID,
				Created:    r.Created.UTC().Format("2006-01-02T15:04:05Z"),
				Expires:    r.Expires.UTC().Format("2006-01-02T15:04:05Z"),
				Consumed:   r.Consumed,
				ConsumedAt: cu,
			})
		}
		body, _ := json.Marshal(list)
		writeHTTP(conn, 200, "application/json", body)
		return
	}
	writeHTTP(conn, 200, "text/html; charset=utf-8",
		renderAdminInvitesPage(invites, "", ""))
}

func applyInviteMutation(invites *inviteStore, rawHeaders []byte, br *bufio.Reader) (string, string) {
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		return "could not parse form: " + err.Error(), "err"
	}
	op := form.Get("op")
	switch op {
	case "create":
		name := strings.TrimSpace(form.Get("name"))
		if name == "" {
			return "name required", "err"
		}
		rec, err := invites.Issue(name)
		if err != nil {
			return err.Error(), "err"
		}
		return "invite created: " + inviteURL(rec.Token), "ok"
	case "revoke":
		t := form.Get("token")
		if err := invites.Delete(t); err != nil {
			return err.Error(), "err"
		}
		return "invite revoked", "ok"
	}
	return "unknown op: " + op, "err"
}

// /admin/invites/create — JSON-friendly creation endpoint for CLI.
func serveAdminInvitesCreate(conn net.Conn, rawHeaders []byte, br *bufio.Reader, invites *inviteStore) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		writeJSONErr(conn, 400, "parse form: "+err.Error())
		return
	}
	name := strings.TrimSpace(form.Get("name"))
	if name == "" {
		writeJSONErr(conn, 400, "name required")
		return
	}
	rec, err := invites.Issue(name)
	if err != nil {
		writeJSONErr(conn, 400, err.Error())
		return
	}
	type out struct {
		OK      bool   `json:"ok"`
		Token   string `json:"token"`
		URL     string `json:"url"`
		Name    string `json:"name"`
		Expires string `json:"expires"`
	}
	body, _ := json.Marshal(out{
		OK: true, Token: rec.Token, URL: inviteURL(rec.Token),
		Name: rec.Name, Expires: rec.Expires.UTC().Format("2006-01-02T15:04:05Z"),
	})
	writeHTTP(conn, 200, "application/json", body)
}

// inviteURL builds the full https URL invitees should open.
func inviteURL(token string) string {
	return "https://tunnel." + *publicHost + "/invite/" + token
}

// --- /invite/<token> (public registration) -------------------------------

func serveInvite(conn net.Conn, path, method string, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn, invites *inviteStore) {
	// Accepts:
	//   GET  /invite/<token>            → registration HTML page
	//   POST /invite/<token>/begin      → JSON: registration challenge
	//   POST /invite/<token>/finish     → JSON: verify, create user+credential
	rest := strings.TrimPrefix(path, "/invite/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeHTTPError(conn, 404, "invalid invite URL")
		return
	}
	token := parts[0]
	stage := ""
	if len(parts) == 2 {
		stage = parts[1]
	}

	rec := invites.Lookup(token)
	if rec == nil {
		writeHTTP(conn, 410, "text/html; charset=utf-8", []byte(renderInviteErrorPage("This invite link is invalid or has expired. Ask the admin for a new one.")))
		return
	}
	if rec.Consumed {
		writeHTTP(conn, 410, "text/html; charset=utf-8", []byte(renderInviteErrorPage("This invite has already been used. Ask the admin to issue a new one if you need to register another device.")))
		return
	}

	switch stage {
	case "":
		if method != "GET" {
			writeHTTPError(conn, 405, "method not allowed")
			return
		}
		writeHTTP(conn, 200, "text/html; charset=utf-8", []byte(renderInvitePage(rec)))
	case "begin":
		serveInviteBegin(conn, rec, pks, wa)
	case "finish":
		serveInviteFinish(conn, rec, rawHeaders, br, pks, wa, invites)
	default:
		writeHTTPError(conn, 404, "not found")
	}
}

func serveInviteBegin(conn net.Conn, rec *inviteRecord, pks *passkeyStore, wa *webauthn.WebAuthn) {
	// Issue a *temporary* user record so BeginRegistration has stable
	// IDs to bind challenge data to. We commit (or not) when /finish
	// arrives; if the invitee bails out, the user is never persisted.
	tmpID := newUserID()
	tmpUser := &pkUser{
		id:    mustDecodeRawStdB64(tmpID),
		name:  rec.Name,
		creds: nil,
	}
	creation, sess, err := wa.BeginRegistration(tmpUser, registrationOpts()...)
	if err != nil {
		writeJSONErr(conn, 500, "begin registration: "+err.Error())
		return
	}
	// Stash the temp user_id in the pending session via the
	// inviteToken field's alternate use: we encode "<token>|<user_id>".
	stash := rec.Token + "|" + tmpID
	sid := pks.issueRegistration(sess, stash)
	body, _ := json.Marshal(creation)
	writeHTTPWithCookie(conn, 200, "application/json", body, fmt.Sprintf(
		"%s=%s; Path=/; Max-Age=%d; HttpOnly; Secure; SameSite=Lax",
		pkRegCookie, sid, int(pkPendingTTL.Seconds())))
}

func serveInviteFinish(conn net.Conn, rec *inviteRecord, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn, invites *inviteStore) {
	sid := readCookieFromHeaders(rawHeaders, pkRegCookie)
	sess, stash := pks.consumeRegistration(sid)
	if sess == nil {
		writeJSONErr(conn, 400, "no pending registration; reload the invite page")
		return
	}
	parts := strings.SplitN(stash, "|", 2)
	if len(parts) != 2 || parts[0] != rec.Token {
		writeJSONErr(conn, 400, "invite/session mismatch; reload the page")
		return
	}
	tmpUserID := parts[1]
	tmpUser := &pkUser{
		id:    mustDecodeRawStdB64(tmpUserID),
		name:  rec.Name,
		creds: nil,
	}
	req, err := httpRequestFromBuffered(rawHeaders, br)
	if err != nil {
		writeJSONErr(conn, 400, "parse request: "+err.Error())
		return
	}
	defer req.Body.Close()
	body, _ := io.ReadAll(req.Body)
	var wrap struct {
		Label      string          `json:"label"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		writeJSONErr(conn, 400, "parse body: "+err.Error())
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(strings.NewReader(string(wrap.Credential)))
	if err != nil {
		writeJSONErr(conn, 400, "parse credential: "+err.Error())
		return
	}
	cred, err := wa.CreateCredential(tmpUser, *sess, parsed)
	if err != nil {
		writeJSONErr(conn, 400, "verify: "+err.Error())
		return
	}
	// Commit: create the persistent user record, store the credential.
	user := pkUserRecord{ID: tmpUserID, Name: rec.Name}
	pks.mu.Lock()
	user.Created = sess.Expires // approx; we don't have a "now" field handy and Expires is set near issue time
	pks.users = append(pks.users, user)
	_ = pks.saveLocked()
	pks.mu.Unlock()

	label := strings.TrimSpace(wrap.Label)
	if label == "" {
		label = "passkey"
	}
	if err := pks.AddCredential(tmpUserID, label, cred); err != nil {
		// Roll back the user since the credential failed.
		_ = pks.DeleteUser(tmpUserID)
		writeJSONErr(conn, 400, err.Error())
		return
	}
	if err := invites.MarkConsumed(rec.Token, tmpUserID); err != nil {
		writeJSONErr(conn, 400, err.Error())
		return
	}
	// Issue a session cookie immediately so the invitee can hit any
	// tunnel they've been granted access to without re-authenticating.
	authSID := pks.issueAuthSession(tmpUserID)
	cookies := []string{
		fmt.Sprintf("%s=; Path=/; Max-Age=0", pkRegCookie),
		writeAuthSessionCookieHeader(authSID, *publicHost),
	}
	type out struct {
		OK     bool   `json:"ok"`
		UserID string `json:"user_id"`
	}
	out_, _ := json.Marshal(out{OK: true, UserID: tmpUserID})
	writeHTTPWithCookies(conn, 200, "application/json", out_, cookies)
}

// --- /admin/locks (legacy lock/unlock — now grant/revoke wrappers) -------

func serveAdminLocks(conn net.Conn, rawHeaders []byte, br *bufio.Reader, locks *tunnelLocks, pks *passkeyStore) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		writeHTTPRedirect(conn, "/admin/ips")
		return
	}
	sub := form.Get("sub")
	op := form.Get("op")
	if !looksLikeSubdomain(sub) {
		writeHTTPRedirect(conn, "/admin/ips")
		return
	}
	switch op {
	case "lock":
		// "Lock" without a specified user means "grant every existing
		// user." If no users yet, the lock is created empty (will be
		// auto-bypassed until a user shows up — see handlePublicConn).
		for _, u := range pks.ListUsers() {
			_ = locks.Grant(sub, u.ID)
		}
		if len(pks.ListUsers()) == 0 {
			// Force-create the entry so IsLocked reports true.
			_ = locks.Grant(sub, "")
			// Then immediately drop the empty user; entry stays.
			locks.mu.Lock()
			delete(locks.allowed[sub], "")
			locks.mu.Unlock()
		}
	case "unlock":
		_ = locks.Unlock(sub)
	}
	writeHTTPRedirect(conn, "/admin/ips/"+sub)
}

// --- /admin/grants ------------------------------------------------------

func serveAdminGrants(conn net.Conn, rawHeaders []byte, br *bufio.Reader, locks *tunnelLocks, pks *passkeyStore) {
	if !adminAuthOK(rawHeaders) {
		writeHTTPUnauthorized(conn)
		return
	}
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		writeJSONErr(conn, 400, "parse form: "+err.Error())
		return
	}
	sub := form.Get("sub")
	uid := form.Get("user_id")
	op := form.Get("op")
	if !looksLikeSubdomain(sub) {
		writeJSONErr(conn, 400, "invalid subdomain")
		return
	}
	if pks.FindUser(uid) == nil {
		writeJSONErr(conn, 400, "unknown user")
		return
	}
	var msg string
	switch op {
	case "grant":
		if err := locks.Grant(sub, uid); err != nil {
			writeJSONErr(conn, 400, err.Error())
			return
		}
		msg = fmt.Sprintf("granted %s access to %s", uid, sub)
	case "revoke":
		if err := locks.Revoke(sub, uid); err != nil {
			writeJSONErr(conn, 400, err.Error())
			return
		}
		msg = fmt.Sprintf("revoked %s from %s", uid, sub)
	default:
		writeJSONErr(conn, 400, "unknown op: "+op)
		return
	}
	if wantsJSON(rawHeaders) {
		body, _ := json.Marshal(struct {
			OK      bool   `json:"ok"`
			Message string `json:"message"`
		}{true, msg})
		writeHTTP(conn, 200, "application/json", body)
		return
	}
	writeHTTPRedirect(conn, "/admin/ips/"+sub)
}

// --- /auth (visitor login, discoverable assertion) ----------------------

// safeReturnURL sanitises the ?return= value that the login page redirects
// to after a successful assertion.
//
// The raw value is attacker-controlled — anyone can hand a victim a
// /auth?return=... link — and it ends up in a client-side redirect, so
// without this an attacker gets an open redirect off the admin origin, and
// a "javascript:" value would execute in that origin once the victim
// authenticates.
//
// Allowed: a root-relative path, or an absolute https:// URL under
// publicHost. Everything else collapses to "/", which lands on the
// management host's landing page.
func safeReturnURL(raw, publicHost string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// "//evil.com" is protocol-relative: it looks like a path but browsers
	// treat it as a host. Same for "/\evil.com", which some parsers fold to
	// the same thing.
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
		return "/"
	}
	if strings.HasPrefix(raw, "/") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return "/"
	}
	host := strings.ToLower(u.Hostname())
	base := strings.ToLower(publicHost)
	if host == base || strings.HasSuffix(host, "."+base) {
		return u.String()
	}
	return "/"
}

func serveAuth(conn net.Conn, rawQuery string, rawHeaders []byte, pks *passkeyStore) {
	returnURL := ""
	if q, err := url.ParseQuery(rawQuery); err == nil {
		returnURL = safeReturnURL(q.Get("return"), *publicHost)
	}
	if !pks.HasCredentials() {
		writeHTTP(conn, 503, "text/html; charset=utf-8",
			[]byte(renderAuthErrorPage("No passkeys registered yet. The admin needs to issue an invite or register the first credential at /admin/passkeys.")))
		return
	}
	page := renderAuthLoginPage(returnURL)
	writeHTTP(conn, 200, "text/html; charset=utf-8", []byte(page))
}

func serveAuthBegin(conn net.Conn, rawQuery string, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	if !pks.HasCredentials() {
		writeJSONErr(conn, 503, "no passkeys registered")
		return
	}
	options, sess, err := wa.BeginDiscoverableLogin()
	if err != nil {
		writeJSONErr(conn, 500, "begin login: "+err.Error())
		return
	}
	returnURL := ""
	if q, err := url.ParseQuery(rawQuery); err == nil {
		returnURL = safeReturnURL(q.Get("return"), *publicHost)
	}
	sid := pks.issueLogin(sess, returnURL)
	body, _ := json.Marshal(options)
	writeHTTPWithCookie(conn, 200, "application/json", body, fmt.Sprintf(
		"%s=%s; Path=/; Max-Age=%d; HttpOnly; Secure; SameSite=Lax",
		pkLoginCookie, sid, int(pkPendingTTL.Seconds())))
}

func serveAuthFinish(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	sid := readCookieFromHeaders(rawHeaders, pkLoginCookie)
	sess, storedReturn := pks.consumeLogin(sid)
	// Re-validate on the way out as well as on the way in: the value has
	// been sitting in server memory since /auth/begin, and this is the one
	// that actually reaches the browser's redirect.
	returnURL := safeReturnURL(storedReturn, *publicHost)
	if sess == nil {
		writeJSONErr(conn, 400, "no pending login; reload the page")
		return
	}
	req, err := httpRequestFromBuffered(rawHeaders, br)
	if err != nil {
		writeJSONErr(conn, 400, "parse request: "+err.Error())
		return
	}
	defer req.Body.Close()

	// Discoverable: handler decides which user is at the keyboard
	// based on the credential ID + userHandle the browser produced.
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		// Prefer matching by credential ID — userHandle is typically
		// echo of WebAuthnID() but we cross-check.
		uid := pks.CredentialUserID(rawID)
		if uid == "" {
			return nil, fmt.Errorf("unknown credential")
		}
		u := pks.userByID(uid)
		if u == nil {
			return nil, fmt.Errorf("user gone")
		}
		// userHandle should equal u.WebAuthnID(); the library asserts
		// that itself but we double-check for clarity.
		if len(userHandle) > 0 && !bytesEqual(userHandle, u.WebAuthnID()) {
			return nil, fmt.Errorf("userHandle mismatch")
		}
		return u, nil
	}

	cred, err := wa.FinishDiscoverableLogin(handler, *sess, req)
	if err != nil {
		writeJSONErr(conn, 400, "verify: "+err.Error())
		return
	}
	pks.UpdateCredential(cred)
	uid := pks.CredentialUserID(cred.ID)

	authSID := pks.issueAuthSession(uid)
	cookies := []string{
		fmt.Sprintf("%s=; Path=/; Max-Age=0", pkLoginCookie),
		writeAuthSessionCookieHeader(authSID, *publicHost),
	}
	type out struct {
		OK     bool   `json:"ok"`
		Return string `json:"return,omitempty"`
		User   string `json:"user,omitempty"`
	}
	uname := ""
	if u := pks.FindUser(uid); u != nil {
		uname = u.Name
	}
	body, _ := json.Marshal(out{OK: true, Return: returnURL, User: uname})
	writeHTTPWithCookies(conn, 200, "application/json", body, cookies)
}

func serveAuthLogout(conn net.Conn, rawQuery string, rawHeaders []byte, pks *passkeyStore) {
	if sid := readCookieFromHeaders(rawHeaders, pkSessionCookie); sid != "" {
		pks.RevokeSession(sid)
	}
	clear := fmt.Sprintf("%s=; Domain=.%s; Path=/; Max-Age=0", pkSessionCookie, *publicHost)
	body := []byte("logged out\n")
	hdr := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nSet-Cookie: %s\r\nCache-Control: no-store\r\nConnection: close\r\n\r\n",
		len(body), clear)
	_, _ = conn.Write([]byte(hdr))
	_, _ = conn.Write(body)
}

// --- /account (self-serve passkey management for signed-in users) ------

// currentSessionUser reads the mygrok_pk cookie and returns the user it
// belongs to, or nil if there's no live session.
func currentSessionUser(rawHeaders []byte, pks *passkeyStore) *pkUserRecord {
	sid := readCookieFromHeaders(rawHeaders, pkSessionCookie)
	if sid == "" {
		return nil
	}
	uid := pks.SessionUser(sid)
	if uid == "" {
		return nil
	}
	return pks.FindUser(uid)
}

// userOwnsCredential checks whether a given credential ID (hex form, as
// shown in /account) belongs to the given user.
func userOwnsCredential(pks *passkeyStore, userID, credID string) bool {
	for _, c := range pks.ListCredentials(userID) {
		if c.ID == credID {
			return true
		}
	}
	return false
}

func serveAccount(conn net.Conn, method, rawQuery string, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	user := currentSessionUser(rawHeaders, pks)
	if user == nil {
		writeHTTPRedirect(conn, "/auth?return="+url.QueryEscape("/account"))
		return
	}
	if wantsJSON(rawHeaders) {
		creds := pks.ListCredentials(user.ID)
		type credOut struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			Created  string `json:"created"`
			LastUsed string `json:"last_used,omitempty"`
		}
		out := struct {
			User struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"user"`
			Credentials []credOut `json:"credentials"`
		}{}
		out.User.ID = user.ID
		out.User.Name = user.Name
		out.Credentials = []credOut{}
		for _, c := range creds {
			co := credOut{ID: c.ID, Label: c.Label, Created: c.Created.UTC().Format("2006-01-02T15:04:05Z")}
			if !c.LastUsedAt.IsZero() {
				co.LastUsed = c.LastUsedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			out.Credentials = append(out.Credentials, co)
		}
		body, _ := json.Marshal(out)
		writeHTTP(conn, 200, "application/json", body)
		return
	}
	writeHTTP(conn, 200, "text/html; charset=utf-8", renderAccountPage(pks, user, "", ""))
}

func serveAccountRegisterBegin(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	user := currentSessionUser(rawHeaders, pks)
	if user == nil {
		writeJSONErr(conn, 401, "no session; sign in at /auth first")
		return
	}
	pku := pks.userByID(user.ID)
	if pku == nil {
		writeJSONErr(conn, 404, "user not found")
		return
	}
	creation, sess, err := wa.BeginRegistration(pku, registrationOpts()...)
	if err != nil {
		writeJSONErr(conn, 500, "begin registration: "+err.Error())
		return
	}
	sid := pks.issueRegistration(sess, "")
	body, _ := json.Marshal(creation)
	writeHTTPWithCookie(conn, 200, "application/json", body, fmt.Sprintf(
		"%s=%s; Path=/; Max-Age=%d; HttpOnly; Secure; SameSite=Lax",
		pkRegCookie, sid, int(pkPendingTTL.Seconds())))
}

func serveAccountRegisterFinish(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn) {
	user := currentSessionUser(rawHeaders, pks)
	if user == nil {
		writeJSONErr(conn, 401, "no session")
		return
	}
	finishRegistration(conn, rawHeaders, br, pks, wa, user.ID)
}

func serveAccountCredentialDelete(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore) {
	user := currentSessionUser(rawHeaders, pks)
	if user == nil {
		writeHTTPRedirect(conn, "/auth?return="+url.QueryEscape("/account"))
		return
	}
	form, err := readPostForm(rawHeaders, br)
	if err != nil {
		writeHTTPRedirect(conn, "/account")
		return
	}
	id := form.Get("id")
	if id == "" {
		writeHTTPRedirect(conn, "/account")
		return
	}
	if !userOwnsCredential(pks, user.ID, id) {
		// Either gone, or someone else's — refuse silently.
		writeHTTPRedirect(conn, "/account")
		return
	}
	if len(pks.ListCredentials(user.ID)) <= 1 {
		page := renderAccountPage(pks, user,
			"can't delete your last passkey — register another one first, or ask an admin to reset your account.",
			"err")
		writeHTTP(conn, 200, "text/html; charset=utf-8", page)
		return
	}
	_ = pks.DeleteCredential(id)
	writeHTTPRedirect(conn, "/account")
}

// --- shared helpers -----------------------------------------------------

func registrationOpts() []webauthn.RegistrationOption {
	return []webauthn.RegistrationOption{
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			RequireResidentKey: protocol.ResidentKeyRequired(),
			ResidentKey:        protocol.ResidentKeyRequirementRequired,
			UserVerification:   protocol.VerificationPreferred,
		}),
	}
}

// finishRegistration is the shared body used by /admin/passkeys/finish
// (the admin-driven flow). The invite flow has its own version because
// it also creates the user record and consumes the invite atomically.
func finishRegistration(conn net.Conn, rawHeaders []byte, br *bufio.Reader, pks *passkeyStore, wa *webauthn.WebAuthn, userID string) {
	sid := readCookieFromHeaders(rawHeaders, pkRegCookie)
	sess, _ := pks.consumeRegistration(sid)
	if sess == nil {
		writeJSONErr(conn, 400, "no pending registration; press the register button again")
		return
	}
	user := pks.userByID(userID)
	req, err := httpRequestFromBuffered(rawHeaders, br)
	if err != nil {
		writeJSONErr(conn, 400, "parse request: "+err.Error())
		return
	}
	defer req.Body.Close()
	body, _ := io.ReadAll(req.Body)
	var wrap struct {
		Label      string          `json:"label"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		writeJSONErr(conn, 400, "parse body: "+err.Error())
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(strings.NewReader(string(wrap.Credential)))
	if err != nil {
		writeJSONErr(conn, 400, "parse credential: "+err.Error())
		return
	}
	cred, err := wa.CreateCredential(user, *sess, parsed)
	if err != nil {
		writeJSONErr(conn, 400, "verify: "+err.Error())
		return
	}
	label := strings.TrimSpace(wrap.Label)
	if label == "" {
		label = "passkey"
	}
	if err := pks.AddCredential(userID, label, cred); err != nil {
		writeJSONErr(conn, 400, err.Error())
		return
	}
	writeHTTP(conn, 200, "application/json", []byte(`{"ok":true}`))
}

func mustDecodeRawStdB64(s string) []byte {
	b, _ := base64.RawStdEncoding.DecodeString(s)
	return b
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- HTTP write helpers -------------------------------------------------

func writeHTTPWithCookie(w io.Writer, code int, contentType string, body []byte, cookie string) {
	writeHTTPWithCookies(w, code, contentType, body, []string{cookie})
}

func writeHTTPWithCookies(w io.Writer, code int, contentType string, body []byte, cookies []string) {
	status := statusText(code)
	var sb strings.Builder
	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nCache-Control: no-store\r\nConnection: close\r\n",
		code, status, contentType, len(body))
	for _, c := range cookies {
		fmt.Fprintf(&sb, "Set-Cookie: %s\r\n", c)
	}
	sb.WriteString("\r\n")
	_, _ = w.Write([]byte(sb.String()))
	_, _ = w.Write(body)
}

func writeJSONErr(w io.Writer, code int, msg string) {
	body, _ := json.Marshal(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}{false, msg})
	writeHTTP(w, code, "application/json", body)
}

// --- HTML pages: invite + admin users + admin invites -------------------

func renderInvitePage(rec *inviteRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, adminPageHeader, "tunnel."+*publicHost, "Register passkey · "+rec.Name)
	fmt.Fprintf(&b, `<h1>Hi, %s</h1>`, html.EscapeString(rec.Name))
	b.WriteString(`<p class="muted">You've been invited to register a passkey for this tunnel server. Click the button below and follow your browser's passkey prompt.</p>`)
	b.WriteString(`<section>`)
	b.WriteString(`<form id="reg-form" onsubmit="return false;">`)
	b.WriteString(`<input id="reg-label" placeholder="device name (e.g. iPhone)" required>`)
	b.WriteString(`<button id="reg-btn">Create passkey</button>`)
	b.WriteString(`</form>`)
	b.WriteString(`<p id="reg-status" class="muted" style="margin-top:12px"></p>`)
	b.WriteString(`</section>`)
	b.WriteString(passkeyJSShim)
	fmt.Fprintf(&b, `<script>
    (function(){
      var btn = document.getElementById('reg-btn');
      var label = document.getElementById('reg-label');
      var status = document.getElementById('reg-status');
      var token = %q;
      btn.addEventListener('click', async function(){
        var name = label.value.trim();
        if (!name) { status.textContent = 'enter a device name first'; return; }
        status.textContent = 'starting…';
        try {
          var r = await fetch('/invite/' + token + '/begin', { method:'POST', credentials:'same-origin' });
          if (!r.ok) throw new Error('begin: HTTP ' + r.status);
          var opts = await r.json();
          opts.publicKey.challenge = b64uDecode(opts.publicKey.challenge);
          opts.publicKey.user.id = b64uDecode(opts.publicKey.user.id);
          if (opts.publicKey.excludeCredentials) {
            opts.publicKey.excludeCredentials.forEach(function(c){ c.id = b64uDecode(c.id); });
          }
          status.textContent = 'follow the browser prompt…';
          var cred = await navigator.credentials.create({ publicKey: opts.publicKey });
          var payload = {
            label: name,
            credential: {
              id: cred.id,
              rawId: b64uEncode(cred.rawId),
              type: cred.type,
              response: {
                clientDataJSON: b64uEncode(cred.response.clientDataJSON),
                attestationObject: b64uEncode(cred.response.attestationObject)
              },
              clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
            }
          };
          var r2 = await fetch('/invite/' + token + '/finish', {
            method:'POST', credentials:'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
          var body = await r2.json().catch(function(){return{};});
          if (!r2.ok || body.ok === false) {
            status.textContent = 'failed: ' + (body.error || ('HTTP ' + r2.status));
            return;
          }
          status.textContent = 'all set! you can close this page.';
          document.getElementById('reg-form').style.display = 'none';
        } catch (e) {
          status.textContent = 'failed: ' + e.message;
        }
      });
    })();
  </script>`, rec.Token)
	b.WriteString(adminPageFooter)
	return b.String()
}

func renderInviteErrorPage(msg string) string {
	var b strings.Builder
	fmt.Fprintf(&b, adminPageHeader, "tunnel."+*publicHost, "Invite")
	b.WriteString(`<h1>Invite unavailable</h1>`)
	fmt.Fprintf(&b, `<p>%s</p>`, html.EscapeString(msg))
	b.WriteString(adminPageFooter)
	return b.String()
}

func renderAdminUsersPage(pks *passkeyStore, locks *tunnelLocks, invites *inviteStore) []byte {
	users := pks.ListUsers()
	var b strings.Builder
	fmt.Fprintf(&b, adminPageHeader, "tunnel."+*publicHost, "Users")
	b.WriteString(`<p class="back"><a class="back-link" href="/admin/ips">overview</a> · <a href="/admin/passkeys">passkeys</a> · <a href="/admin/invites">invites</a></p>`)
	b.WriteString(`<h1>Users</h1>`)
	b.WriteString(`<p class="muted">A user is created the first time someone redeems an invite. Each user owns one or more passkeys (different devices) and can be granted access to specific tunnels.</p>`)

	b.WriteString(`<section>`)
	if len(users) == 0 {
		b.WriteString(`<p class="empty">No users yet. <a href="/admin/invites">Issue an invite →</a></p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>Name</th><th>Created</th><th>Passkeys</th><th>Tunnels</th><th></th></tr></thead><tbody>`)
		for _, u := range users {
			creds := pks.ListCredentials(u.ID)
			var tunnels []string
			for _, sub := range locks.LockedSubdomains() {
				if locks.AllowsUser(sub, u.ID) {
					tunnels = append(tunnels, sub)
				}
			}
			tn := strings.Join(tunnels, ", ")
			if tn == "" {
				tn = `<span class="muted">none</span>`
			} else {
				tn = html.EscapeString(tn)
			}
			fmt.Fprintf(&b,
				`<tr><td><strong>%s</strong><br><span class="muted">id: <code>%s</code></span></td><td>%s</td><td>%d</td><td>%s</td>`+
					`<td><form method="POST" action="/admin/users/delete" class="inline" onsubmit="return confirm('Delete %s and all their passkeys?');">`+
					`<input type="hidden" name="user_id" value="%s"><button class="danger">Delete</button></form></td></tr>`,
				html.EscapeString(u.Name), html.EscapeString(shortID(u.ID)),
				u.Created.UTC().Format("2006-01-02 15:04 UTC"),
				len(creds), tn,
				html.EscapeString(u.Name), html.EscapeString(u.ID))
		}
		b.WriteString(`</tbody></table>`)
	}
	b.WriteString(`</section>`)
	b.WriteString(adminPageFooter)
	return []byte(b.String())
}

func renderAdminInvitesPage(invites *inviteStore, flash, flashClass string) []byte {
	all := invites.List(true)
	var b strings.Builder
	fmt.Fprintf(&b, adminPageHeader, "tunnel."+*publicHost, "Invites")
	b.WriteString(`<p class="back"><a class="back-link" href="/admin/ips">overview</a> · <a href="/admin/users">users</a> · <a href="/admin/passkeys">passkeys</a></p>`)
	b.WriteString(`<h1>Passkey invites</h1>`)
	b.WriteString(`<p class="muted">Issue a one-shot invite link tagged with a name. Send the URL to that person; when they redeem it, a user with that name is created and their first passkey is registered.</p>`)
	if flash != "" {
		if flashClass == "ok" && strings.HasPrefix(flash, "invite created: ") {
			url := strings.TrimPrefix(flash, "invite created: ")
			fmt.Fprintf(&b,
				`<div class="flash ok"><div class="flash-title">invite created — send this URL to the invitee</div>`+
					`<div class="copy-row"><code>%s</code>`+
					`<button type="button" class="copy-btn" data-copy="%s">copy</button></div></div>`,
				html.EscapeString(url), html.EscapeString(url))
		} else {
			fmt.Fprintf(&b, `<div class="flash %s">%s</div>`, html.EscapeString(flashClass), html.EscapeString(flash))
		}
	}

	b.WriteString(`<section><h2>Issue invite</h2>`)
	b.WriteString(`<form method="POST" class="add"><input type="hidden" name="op" value="create"><input name="name" placeholder="name (e.g. Alice)" required><button>Create invite</button></form>`)
	b.WriteString(`<p class="muted" style="margin-top:8px">Invite links expire 7 days after issue and are single-use.</p>`)
	b.WriteString(`</section>`)

	b.WriteString(`<section><h2>Active</h2>`)
	pending := []inviteRecord{}
	used := []inviteRecord{}
	for _, r := range all {
		if r.Consumed {
			used = append(used, r)
		} else {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		b.WriteString(`<p class="empty">no pending invites</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>Name</th><th>URL</th><th>Created</th><th>Expires</th><th></th></tr></thead><tbody>`)
		for _, r := range pending {
			u := inviteURL(r.Token)
			fmt.Fprintf(&b,
				`<tr><td>%s</td><td><div class="copy-row"><a href="%s"><code>%s</code></a>`+
					`<button type="button" class="copy-btn" data-copy="%s">copy</button></div></td>`+
					`<td>%s</td><td>%s</td>`+
					`<td><form method="POST" class="inline"><input type="hidden" name="op" value="revoke"><input type="hidden" name="token" value="%s"><button class="danger">Revoke</button></form></td></tr>`,
				html.EscapeString(r.Name), html.EscapeString(u), html.EscapeString(shortID(r.Token)),
				html.EscapeString(u),
				r.Created.UTC().Format("2006-01-02 15:04 UTC"),
				r.Expires.UTC().Format("2006-01-02 15:04 UTC"),
				html.EscapeString(r.Token))
		}
		b.WriteString(`</tbody></table>`)
	}
	b.WriteString(`</section>`)

	if len(used) > 0 {
		b.WriteString(`<section><h2>Redeemed</h2><table><thead><tr><th>Name</th><th>Created</th><th>Used</th></tr></thead><tbody>`)
		for _, r := range used {
			fmt.Fprintf(&b, `<tr><td>%s</td><td>%s</td><td>%s</td></tr>`,
				html.EscapeString(r.Name),
				r.Created.UTC().Format("2006-01-02 15:04 UTC"),
				r.ConsumedAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		b.WriteString(`</tbody></table></section>`)
	}

	b.WriteString(adminPageFooter)
	return []byte(b.String())
}

// renderAccountPage shows the signed-in user's own passkeys and a form
// to register an additional device. No admin auth required — gated only
// by the mygrok_pk session cookie.
func renderAccountPage(pks *passkeyStore, user *pkUserRecord, flash, flashClass string) []byte {
	creds := pks.ListCredentials(user.ID)
	var b strings.Builder
	fmt.Fprintf(&b, adminPageHeader, "tunnel."+*publicHost, "My passkeys")
	b.WriteString(`<h1>My passkeys</h1>`)
	fmt.Fprintf(&b, `<p class="muted">Signed in as <strong>%s</strong>. Add another device, or remove an old one. <a href="/auth/logout">sign out</a></p>`,
		html.EscapeString(user.Name))
	if flash != "" {
		fmt.Fprintf(&b, `<div class="flash %s">%s</div>`, html.EscapeString(flashClass), html.EscapeString(flash))
	}

	b.WriteString(`<section><h2>Registered <span class="muted">(`)
	fmt.Fprintf(&b, "%d", len(creds))
	b.WriteString(`)</span></h2>`)
	if len(creds) == 0 {
		b.WriteString(`<p class="empty">No passkeys yet. Use the form below to register one.</p>`)
	} else {
		b.WriteString(`<table><thead><tr><th>Label</th><th>ID</th><th>Created</th><th>Last used</th><th></th></tr></thead><tbody>`)
		canDelete := len(creds) > 1
		for _, c := range creds {
			lastUsed := "never"
			if !c.LastUsedAt.IsZero() {
				lastUsed = c.LastUsedAt.UTC().Format("2006-01-02 15:04 UTC")
			}
			delCell := `<td class="muted" title="register another passkey before removing your last one">last passkey</td>`
			if canDelete {
				delCell = fmt.Sprintf(
					`<td><form method="POST" action="/account/credentials/delete" class="inline" onsubmit="return confirm('Delete this passkey? You won&apos;t be able to use it to sign in anymore.');">`+
						`<input type="hidden" name="id" value="%s"><button class="danger">Delete</button></form></td>`,
					html.EscapeString(c.ID))
			}
			fmt.Fprintf(&b,
				`<tr><td>%s</td><td><code>%s</code></td><td>%s</td><td>%s</td>%s</tr>`,
				html.EscapeString(c.Label), html.EscapeString(shortID(c.ID)),
				c.Created.UTC().Format("2006-01-02 15:04 UTC"),
				html.EscapeString(lastUsed),
				delCell)
		}
		b.WriteString(`</tbody></table>`)
	}
	b.WriteString(`</section>`)

	b.WriteString(`<section><h2>Add another device</h2>`)
	b.WriteString(`<p class="blurb">Pick a label that helps you recognise this device later (e.g. "iPhone 15", "office MacBook"), then click Register and follow your browser's passkey prompt.</p>`)
	b.WriteString(`<form id="reg-form" class="add" onsubmit="return false;">`)
	b.WriteString(`<input id="reg-label" placeholder="e.g. MacBook Pro" required>`)
	b.WriteString(`<button id="reg-btn">Register passkey</button>`)
	b.WriteString(`</form>`)
	b.WriteString(`<p id="reg-status" class="muted" style="margin-top:8px"></p>`)
	b.WriteString(`</section>`)

	b.WriteString(passkeyJSShim)
	b.WriteString(`<script>
    (function(){
      var btn = document.getElementById('reg-btn');
      var label = document.getElementById('reg-label');
      var status = document.getElementById('reg-status');
      btn.addEventListener('click', async function(){
        var name = label.value.trim();
        if (!name) { status.textContent = 'enter a label first'; return; }
        status.textContent = 'starting…';
        try {
          var r = await fetch('/account/register/begin', { method:'POST', credentials:'same-origin' });
          if (!r.ok) throw new Error('begin: HTTP ' + r.status);
          var opts = await r.json();
          opts.publicKey.challenge = b64uDecode(opts.publicKey.challenge);
          opts.publicKey.user.id = b64uDecode(opts.publicKey.user.id);
          if (opts.publicKey.excludeCredentials) {
            opts.publicKey.excludeCredentials.forEach(function(c){ c.id = b64uDecode(c.id); });
          }
          status.textContent = 'follow the browser prompt…';
          var cred = await navigator.credentials.create({ publicKey: opts.publicKey });
          var payload = {
            label: name,
            credential: {
              id: cred.id,
              rawId: b64uEncode(cred.rawId),
              type: cred.type,
              response: {
                clientDataJSON: b64uEncode(cred.response.clientDataJSON),
                attestationObject: b64uEncode(cred.response.attestationObject)
              },
              clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
            }
          };
          var r2 = await fetch('/account/register/finish', {
            method:'POST', credentials:'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
          var body = await r2.json().catch(function(){return{};});
          if (!r2.ok || body.ok === false) {
            status.textContent = 'failed: ' + (body.error || ('HTTP ' + r2.status));
            return;
          }
          status.textContent = 'registered. reloading…';
          location.reload();
        } catch (e) {
          status.textContent = 'failed: ' + e.message;
        }
      });
    })();
  </script>`)
	b.WriteString(adminPageFooter)
	return []byte(b.String())
}

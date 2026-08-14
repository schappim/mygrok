// mygrokd is the public-facing tunnel server.
//
// It listens on:
//
//	:7000 — control/data plane: clients connect, handshake, then yamux-mux
//	:80   — public HTTP: requests are routed by Host header subdomain to the
//	        matching tunnel client over a new yamux stream.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/hashicorp/yamux"
	"github.com/schappim/mygrok/internal/buildinfo"
	"github.com/schappim/mygrok/internal/proto"
)

// yamuxConfig is shared by client and server. Tighter than the library
// defaults (30s/10s) so we evict dead peers in ~15s instead of ~40s. Keeps
// home-router NAT entries warm too.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.ConnectionWriteTimeout = 5 * time.Second
	return cfg
}

// tuneTCPKeepalive enables OS-level TCP keepalive as a backstop in case
// yamux's application-level pings get stuck behind a blocked write.
func tuneTCPKeepalive(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
}

var (
	publicHost      = flag.String("public-host", "", "base public host, e.g. example.com; tunnel URLs become <subdomain>.<public-host> (required)")
	httpAddr        = flag.String("http", ":80", "public HTTP listen address (\"\" disables)")
	httpsAddr       = flag.String("https", "", "public HTTPS listen address, e.g. :443 (empty disables)")
	tunnelAddr      = flag.String("tunnel", ":7000", "tunnel control listen address")
	authToken       = flag.String("auth", "", "shared auth token (or env MYGROK_AUTHTOKEN)")
	dnsProviderName = flag.String("dns-provider", "none",
		"DNS provider for wildcard certs (DNS-01) and LAN-direct records: none, route53, cloudflare, digitalocean")
	certDomains = flag.String("cert-domains", "",
		"comma-separated names to pre-issue certs for (default: *.<public-host> with a DNS provider, tunnel.<public-host> without)")
	certEmail       = flag.String("cert-email", "", "ACME contact email")
	certStaging     = flag.Bool("cert-staging", false, "use Let's Encrypt staging endpoint")
	certDir         = flag.String("cert-dir", "/var/lib/mygrokd/certs", "where to cache certs and ACME state")
	ipListPath      = flag.String("ip-list", "/var/lib/mygrokd/iplist.json", "path to JSON file storing the IP allow/block lists")
	passkeysPath    = flag.String("passkeys", "/var/lib/mygrokd/passkeys.json", "path to JSON file storing users + their registered passkeys")
	tunnelLocksPath = flag.String("tunnel-locks", "/var/lib/mygrokd/tunnellocks.json", "path to JSON file recording per-tunnel passkey access lists")
	invitesPath     = flag.String("invites", "/var/lib/mygrokd/invites.json", "path to JSON file storing pending passkey invites")
	lanEnabled      = flag.Bool("lan", false, "enable LAN-direct (same-NAT 307 to <sub>-lan.<publicHost>; needs --dns-provider and a wildcard cert)")
	showVersion     = flag.Bool("version", false, "print version and exit")
)

// Reserved subdomains that the server refuses to register. These names
// typically already have specific A/MX records on a real zone, and a
// specific record always beats the wildcard — so a tunnel claiming one
// would register fine and then never receive traffic. Refusing up front
// turns a silent mystery into an error at connect time.
//
// The management host (tunnel.<publicHost>) is excluded separately, by
// isManagementHost.
var reserved = map[string]bool{
	"www": true, "api": true, "admin": true, "mail": true,
}

type tunnel struct {
	subdomain string
	session   *yamux.Session
	clientID  string
	hostnames []string // additional public hostnames (CNAMEs) routed to this tunnel
	created   time.Time
	// publicIP is the IP the server saw on the control connection's
	// RemoteAddr at claim time. Used for same-NAT detection: when a
	// public visitor's IP matches this, we 307 them to the tunnel's
	// LAN sister hostname so the request bypasses us entirely.
	publicIP string
	// lanIP is the RFC1918 address the client published in its Hello
	// (or "" if the client didn't enable LAN-direct). lanHostname is
	// the matching public DNS name (<sub>-lan.<publicHost>) we wrote
	// a Route53 A record for.
	lanIP       string
	lanHostname string
}

type registry struct {
	mu          sync.RWMutex
	tunnels     map[string]*tunnel
	customHosts map[string]string // host → subdomain
}

func newRegistry() *registry {
	return &registry{
		tunnels:     map[string]*tunnel{},
		customHosts: map[string]string{},
	}
}

// claim registers a tunnel for sub plus any extra hostnames (CNAMEs).
// If sub is already taken by the same clientID, the existing session is
// closed and replaced (this is how a reconnecting client recovers from a
// stale dead session on the server). If clientID differs, the claim fails.
// If any of the requested hostnames is already owned by a different
// subdomain, the claim fails too — same-clientID takeover does not extend
// to hostnames owned by another tunnel.
//
// preempted is non-nil when we kicked out an old session — the caller
// should log this for diagnostics.
func (r *registry) claim(sub, clientID string, hostnames []string, publicIP, lanIP, lanHostname string, sess *yamux.Session) (t *tunnel, preempted *tunnel, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range hostnames {
		if owner, exists := r.customHosts[h]; exists && owner != sub {
			return nil, nil, fmt.Sprintf("hostname %q is already in use by another tunnel", h)
		}
	}
	if existing, taken := r.tunnels[sub]; taken {
		if clientID == "" || existing.clientID != clientID {
			return nil, nil, "subdomain in use"
		}
		preempted = existing
		// Drop the old tunnel's hostname mappings before re-registering;
		// the new claim brings its own (possibly different) set.
		for _, h := range existing.hostnames {
			delete(r.customHosts, h)
		}
	}
	t = &tunnel{
		subdomain: sub, session: sess, clientID: clientID,
		hostnames: hostnames, created: time.Now(),
		publicIP: publicIP, lanIP: lanIP, lanHostname: lanHostname,
	}
	r.tunnels[sub] = t
	for _, h := range hostnames {
		r.customHosts[h] = sub
	}
	if preempted != nil {
		// Close after swapping so any in-flight stream.Open() on the old
		// session fails fast rather than silently using a dead pipe.
		go preempted.session.Close()
	}
	return t, preempted, ""
}

// release removes sub from the registry only if the currently-registered
// session matches sess. This protects against a goroutine whose session was
// pre-empted from clobbering the new owner's registration when its defer
// runs.
func (r *registry) release(sub string, sess *yamux.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.tunnels[sub]; ok && t.session == sess {
		for _, h := range t.hostnames {
			delete(r.customHosts, h)
		}
		delete(r.tunnels, sub)
	}
}

// lookup finds the tunnel that should serve a given host. Tries the
// custom-hostnames map first; the caller is responsible for the
// `*.publicHost` flow.
func (r *registry) lookup(host string) *tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if sub, ok := r.customHosts[host]; ok {
		return r.tunnels[sub]
	}
	return nil
}

// activeSubdomains returns the subdomains of all currently-registered
// tunnels, sorted lexically. Used by the admin overview page to list
// live tunnels alongside any that have rules pre-configured.
func (r *registry) activeSubdomains() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tunnels))
	for sub := range r.tunnels {
		out = append(out, sub)
	}
	sort.Strings(out)
	return out
}

// hostKnown is consulted by certmagic's OnDemand decision callback —
// only registered custom hostnames are eligible for lazy cert issuance.
// Anything else (including random scanners hitting :443 with arbitrary
// SNI) is rejected before we waste an ACME round-trip.
func (r *registry) hostKnown(host string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.customHosts[host]
	return ok
}

func (r *registry) get(sub string) *tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[sub]
}

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Printf("mygrokd %s\n", buildinfo.Version)
		return
	}
	// Logged on every start so a bug report can say which build it is
	// without anyone having to stop the service to ask.
	log.Printf("mygrokd %s", buildinfo.Version)
	if *authToken == "" {
		*authToken = os.Getenv("MYGROK_AUTHTOKEN")
	}
	if *authToken == "" {
		log.Fatal("auth token required: --auth or MYGROK_AUTHTOKEN")
	}
	if *publicHost == "" {
		*publicHost = os.Getenv("MYGROK_PUBLIC_HOST")
	}
	*publicHost = strings.Trim(strings.ToLower(strings.TrimSpace(*publicHost)), ".")
	if *publicHost == "" {
		log.Fatal("public host required: --public-host=example.com (or MYGROK_PUBLIC_HOST). " +
			"Tunnel URLs become <subdomain>.<public-host>, so it must be a domain whose " +
			"wildcard DNS points at this server.")
	}
	// A server with neither public listener has nowhere to send visitors:
	// tunnels would register successfully and then be unreachable at any URL.
	if *httpAddr == "" && *httpsAddr == "" {
		log.Fatal("at least one public listener is required: set --http (e.g. :80) or --https (e.g. :443)")
	}

	reg := newRegistry()

	acl, err := newIPACL(*ipListPath)
	if err != nil {
		log.Fatalf("ip list: %v", err)
	}

	pks, err := newPasskeyStore(*passkeysPath)
	if err != nil {
		log.Fatalf("passkeys: %v", err)
	}
	wa, err := buildWebAuthn(*publicHost)
	if err != nil {
		log.Fatalf("webauthn: %v", err)
	}
	locks, err := newTunnelLocks(*tunnelLocksPath)
	if err != nil {
		log.Fatalf("tunnel locks: %v", err)
	}
	invites, err := newInviteStore(*invitesPath)
	if err != nil {
		log.Fatalf("invites: %v", err)
	}

	dns, err := buildDNSProvider(*dnsProviderName)
	if err != nil {
		log.Fatalf("dns provider: %v", err)
	}
	if dns == nil {
		log.Printf("no DNS provider: certificates will be issued per-hostname on demand " +
			"(set --dns-provider for a wildcard)")
	} else {
		log.Printf("dns provider: %s", *dnsProviderName)
	}

	var lan *lanManager
	if *lanEnabled {
		if dns == nil {
			log.Fatal("--lan needs --dns-provider: LAN-direct publishes an A record " +
				"for <sub>-lan.<public-host> and serves it with the wildcard cert")
		}
		lan = newLANManager(*publicHost, *certDir, dns)
	}

	go serveTunnel(reg, lan)

	// HTTPS listener (optional). certmagic gets a wildcard via DNS-01 when a
	// provider is configured, and falls back to on-demand TLS-ALPN-01 certs
	// for everything else.
	if *httpsAddr != "" {
		tlsCfg, err := setupTLS(reg, dns)
		if err != nil {
			log.Fatalf("tls setup: %v", err)
		}
		go servePublicTLS(reg, acl, pks, wa, locks, invites, lan, *httpsAddr, tlsCfg)
	}

	if *httpAddr != "" {
		servePublic(reg, acl, pks, wa, locks, invites, lan, *httpAddr)
	} else {
		select {}
	}
}

func setupTLS(reg *registry, dns dnsProvider) (*tls.Config, error) {
	var domains []string
	if *certDomains == "" {
		domains = defaultCertDomains(*publicHost, dns != nil)
	} else {
		for _, d := range strings.Split(*certDomains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				domains = append(domains, d)
			}
		}
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("--cert-domains resolved to nothing")
	}
	for _, d := range domains {
		if strings.HasPrefix(d, "*.") && dns == nil {
			return nil, fmt.Errorf("cert domain %q is a wildcard, which requires "+
				"DNS-01 — set --dns-provider (one of: %s)",
				d, strings.Join(dnsProviderNames[1:], ", "))
		}
	}

	if err := os.MkdirAll(*certDir, 0o700); err != nil {
		return nil, fmt.Errorf("cert dir: %w", err)
	}

	// Declared up-front so the GetConfigForCert closure can capture the
	// pointer; certmagic.New(nil, ...) panics, so the callback must hand
	// it a real cache. Without the closure trick we'd hit "a certificate
	// cache is required" the first time the renewal goroutine ran.
	var cache *certmagic.Cache
	cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(cache, certmagic.Config{Storage: &certmagic.FileStorage{Path: *certDir}}), nil
		},
	})
	magic := certmagic.New(cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: *certDir},
	})
	ca := certmagic.LetsEncryptProductionCA
	if *certStaging {
		ca = certmagic.LetsEncryptStagingCA
	}
	// The plain issuer handles everything that resolves to us — every
	// <sub>.<publicHost> (thanks to wildcard DNS) and every client-registered
	// CNAME. No DNS01Solver, so it falls back to HTTP-01 / TLS-ALPN-01;
	// TLS-ALPN-01 piggybacks on our own :443 listener, so there's nothing
	// extra to wire up.
	onDemandIssuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:     ca,
		Email:  *certEmail,
		Agreed: true,
		// HTTP-01 is switched off because we cannot solve it: our :80
		// listener does its own Host-header routing and never hands
		// /.well-known/acme-challenge to certmagic. Leaving it enabled
		// would just burn an attempt (and a failed-validation against the
		// rate limit) before falling through to TLS-ALPN-01, which does
		// work — it rides the :443 handshake we already own.
		DisableHTTPChallenge: true,
	})
	// With a DNS provider we prepend a DNS-01 issuer, the only way to get a
	// wildcard. One *.<publicHost> cert then covers every tunnel, so new
	// subdomains serve TLS instantly and we stay well clear of Let's
	// Encrypt's per-domain rate limits.
	if dns != nil {
		wildIssuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
			CA:     ca,
			Email:  *certEmail,
			Agreed: true,
			DNS01Solver: &certmagic.DNS01Solver{
				DNSManager: certmagic.DNSManager{DNSProvider: dns},
			},
		})
		magic.Issuers = []certmagic.Issuer{wildIssuer, onDemandIssuer}
	} else {
		magic.Issuers = []certmagic.Issuer{onDemandIssuer}
	}

	// OnDemand: lazy cert issuance for SNI names we didn't pre-issue for.
	// This is the only thing standing between a scanner and the operator's
	// Let's Encrypt quota, so it has to actually decide something.
	//
	// With a wildcard (DNS provider configured), everything under publicHost
	// is already covered by one cached cert. We allow those unconditionally:
	// certmagic normally serves them from cache without consulting us at
	// all, and if a cache lookup ever falls through, blocking here would
	// break working tunnels.
	//
	// Without a wildcard, every <sub>.publicHost that arrives is a real ACME
	// order. Allowing the whole zone would let anyone with a TCP connection
	// walk it — a few hundred SNIs and the operator can't get certificates
	// for a week. So we require a live registration: a tunnel that is
	// actually connected, a client-registered custom hostname, or the
	// management host itself (which must work before any tunnel exists).
	hasWildcard := dns != nil
	mgmtHost := "tunnel." + *publicHost
	magic.OnDemand = &certmagic.OnDemandConfig{
		DecisionFunc: func(_ context.Context, name string) error {
			n := strings.ToLower(name)
			if hasWildcard && (n == *publicHost || strings.HasSuffix(n, "."+*publicHost)) {
				return nil
			}
			if n == mgmtHost || n == *publicHost {
				return nil
			}
			if sub := subdomainOf(n, *publicHost); sub != "" && reg.get(sub) != nil {
				return nil
			}
			if reg.hostKnown(n) {
				return nil
			}
			return fmt.Errorf("no tunnel is registered for %q", name)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	log.Printf("acquiring certs for %v", domains)
	if err := magic.ManageSync(ctx, domains); err != nil {
		// Deliberately not fatal. The overwhelmingly common cause on a first
		// boot is "DNS doesn't point here yet", and dying would put us in a
		// Restart=always loop that hammers Let's Encrypt's failed-validation
		// limit — locking the operator out of certificates for an hour for
		// the crime of running the installer before updating DNS.
		//
		// Coming up without a certificate is strictly better: :80 works, the
		// landing page explains itself, certmagic's background maintenance
		// keeps retrying, and OnDemand issues per-hostname as requests
		// arrive.
		log.Printf("WARNING: could not obtain certs for %v: %v", domains, err)
		log.Printf("WARNING: serving without them for now — check that DNS for %v resolves to this server, "+
			"and that :80 and :443 are reachable from the internet", domains)
	} else {
		log.Printf("certs ready")
	}
	cfg := magic.TLSConfig()
	// We tunnel raw bytes; only advertise HTTP/1.1 so clients don't try h2.
	// (Keep "acme-tls/1" if certmagic added it for TLS-ALPN-01, though we use DNS-01.)
	out := []string{"http/1.1"}
	for _, p := range cfg.NextProtos {
		if p == "acme-tls/1" {
			out = append(out, p)
		}
	}
	cfg.NextProtos = out
	return cfg, nil
}

// serveTunnel accepts client tunnel connections, handshakes, then runs yamux
// for the lifetime of the tunnel.
func serveTunnel(reg *registry, lan *lanManager) {
	ln, err := net.Listen("tcp", *tunnelAddr)
	if err != nil {
		log.Fatalf("tunnel listen: %v", err)
	}
	log.Printf("tunnel listener on %s", *tunnelAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("tunnel accept: %v", err)
			continue
		}
		go handleTunnelConn(reg, lan, conn)
	}
}

func handleTunnelConn(reg *registry, lan *lanManager, conn net.Conn) {
	defer conn.Close()
	tuneTCPKeepalive(conn)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	var hello proto.Hello
	if err := proto.ReadJSONLine(br, &hello); err != nil {
		log.Printf("hello read: %v", err)
		return
	}
	conn.SetDeadline(time.Time{})

	// Constant-time: :7000 is reachable from the internet, and a byte-wise
	// early return would make this a timing oracle for the shared token.
	if !constantTimeStringEq(hello.Auth, *authToken) {
		_ = proto.WriteJSONLine(conn, proto.HelloResp{Error: "bad auth"})
		return
	}
	sub := strings.ToLower(strings.TrimSpace(hello.Subdomain))
	if !validSubdomain(sub) || reserved[sub] {
		_ = proto.WriteJSONLine(conn, proto.HelloResp{Error: "invalid or reserved subdomain"})
		return
	}

	// yamux must see all bytes on the wire. The handshake JSON line has
	// already been consumed via bufio, but bufio may have buffered nothing
	// further (we write one line and immediately switch). Pass the raw conn
	// to yamux — it will not read headers we've consumed because we only
	// read up to the newline.
	if br.Buffered() > 0 {
		log.Printf("unexpected buffered bytes after hello: %d", br.Buffered())
		return
	}

	sess, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		log.Printf("yamux server: %v", err)
		return
	}

	hostnames, herr := normalizeHostnames(hello.Hostnames, *publicHost)
	if herr != nil {
		_ = proto.WriteJSONLine(conn, proto.HelloResp{Error: herr.Error()})
		sess.Close()
		return
	}

	// LAN-direct opt-in: client sends a private IPv4 in Hello.LANIP.
	// Validate it; refuse anything that's not RFC1918 so we can't be
	// tricked into pointing public DNS at a non-private address.
	publicIP := remoteIPFrom(conn.RemoteAddr())
	var lanIP, lanHostname string
	if lan != nil && hello.LANIP != "" {
		if !IsRFC1918(hello.LANIP) {
			_ = proto.WriteJSONLine(conn, proto.HelloResp{Error: "lan_ip must be a private IPv4 (10/8, 172.16/12, 192.168/16)"})
			sess.Close()
			return
		}
		// Refuse subdomains ending in "-lan": their public URL would
		// collide with another tunnel's LAN sister hostname.
		if strings.HasSuffix(sub, "-lan") {
			_ = proto.WriteJSONLine(conn, proto.HelloResp{Error: "subdomain cannot end with '-lan' when --lan is enabled (collides with sister-hostname pattern)"})
			sess.Close()
			return
		}
		lanIP = hello.LANIP
		lanHostname = lan.LANHostname(sub)
	}

	t, preempted, claimErr := reg.claim(sub, hello.ClientID, hostnames, publicIP, lanIP, lanHostname, sess)
	if claimErr != "" {
		_ = proto.WriteJSONLine(conn, proto.HelloResp{Error: claimErr})
		sess.Close()
		return
	}
	if preempted != nil {
		log.Printf("tunnel takeover: %s (same client_id reconnected, evicted prior session)", sub)
		// Best-effort: drop the LAN A record from the prior session if
		// the new client isn't bringing its own (or if the IP changed).
		if preempted.lanHostname != "" && lan != nil &&
			(lanHostname == "" || preempted.lanIP != lanIP) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = lan.DeleteA(ctx, preempted.lanHostname)
			cancel()
		}
	}
	defer reg.release(sub, sess)
	defer sess.Close()
	defer func() {
		// Best-effort: pull the LAN A record on tunnel teardown so
		// stale records don't strand visitors at a dead LAN IP. Skip
		// if a takeover already replaced this tunnel (the new owner's
		// release will clean up its own record).
		if lanHostname == "" || lan == nil {
			return
		}
		if cur := reg.get(sub); cur != nil && cur.session != sess {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = lan.DeleteA(ctx, lanHostname)
		cancel()
	}()

	// LAN handshake response bits: write the Route53 record and fetch
	// the wildcard cert PEM. Both are best-effort — if either fails,
	// we still bring the tunnel up without LAN-direct (the response
	// just omits the LAN fields).
	var lanCertPEM, lanKeyPEM string
	if lan != nil && lanHostname != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := lan.UpsertA(ctx, lanHostname, lanIP); err != nil {
			log.Printf("lan: route53 upsert %s -> %s: %v (tunnel up without LAN-direct)", lanHostname, lanIP, err)
			lanHostname = "" // signal "no LAN" downstream
		}
		cancel()
		if lanHostname != "" {
			cert, key, err := lan.WildcardCertPEM()
			if err != nil {
				log.Printf("lan: read wildcard cert: %v (tunnel up without LAN-direct)", err)
				lanHostname = ""
			} else {
				lanCertPEM, lanKeyPEM = cert, key
			}
		}
		// If the cert read failed but we already wrote the A record,
		// drop it again — no point pointing public DNS at a LAN IP
		// when the client can't actually serve TLS.
		if lanHostname == "" && lan != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = lan.DeleteA(ctx, lan.LANHostname(sub))
			cancel()
		}
	}

	urls := []string{}
	if *httpsAddr != "" {
		urls = append(urls, fmt.Sprintf("https://%s.%s", sub, *publicHost))
	}
	if *httpAddr != "" {
		urls = append(urls, fmt.Sprintf("http://%s.%s", sub, *publicHost))
	}
	for _, h := range hostnames {
		if *httpsAddr != "" {
			urls = append(urls, "https://"+h)
		}
		if *httpAddr != "" {
			urls = append(urls, "http://"+h)
		}
	}
	// main() refuses to start with both public listeners disabled, so urls
	// is never empty here. Belt and braces: a panic in the handshake would
	// take down every other tunnel on the server.
	url := ""
	if len(urls) > 0 {
		url = urls[0]
	}
	resp := proto.HelloResp{OK: true, URL: url, URLs: urls}
	if lanHostname != "" {
		resp.LANHostname = lanHostname
		resp.LANCertPEM = lanCertPEM
		resp.LANKeyPEM = lanKeyPEM
	}
	if err := proto.WriteJSONLine(conn, resp); err != nil {
		log.Printf("hello resp: %v", err)
		return
	}
	switch {
	case len(hostnames) > 0 && lanHostname != "":
		log.Printf("tunnel up: %s -> %s (+hostnames %v +lan %s -> %s) (peer=%s)", sub, url, hostnames, lanHostname, lanIP, conn.RemoteAddr())
	case len(hostnames) > 0:
		log.Printf("tunnel up: %s -> %s (+hostnames %v) (peer=%s)", sub, url, hostnames, conn.RemoteAddr())
	case lanHostname != "":
		log.Printf("tunnel up: %s -> %s (+lan %s -> %s) (peer=%s)", sub, url, lanHostname, lanIP, conn.RemoteAddr())
	default:
		log.Printf("tunnel up: %s -> %s (peer=%s)", sub, url, conn.RemoteAddr())
	}

	// Block until the session dies.
	<-sess.CloseChan()
	log.Printf("tunnel down: %s", sub)
	_ = t
}

// normalizeHostnames lowercases, trims, and de-duplicates the client's
// requested custom hostnames, validating each. Hostnames under publicHost
// (e.g., "foo.<publicHost>") are rejected — those go through --subdomain
// and use the wildcard cert. All other DNS-shaped names are accepted; the
// server doesn't pre-check resolution.
func normalizeHostnames(hosts []string, publicHost string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	publicHost = strings.ToLower(publicHost)
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || seen[h] {
			continue
		}
		if h == publicHost || strings.HasSuffix(h, "."+publicHost) {
			return nil, fmt.Errorf("hostname %q is under %s; use --subdomain= for that, --hostname= is for external CNAMEs", h, publicHost)
		}
		if err := validateCustomHost(h); err != nil {
			return nil, err
		}
		seen[h] = true
		out = append(out, h)
	}
	return out, nil
}

func validateCustomHost(h string) error {
	if len(h) == 0 || len(h) > 253 {
		return fmt.Errorf("invalid hostname length: %q", h)
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 {
			return fmt.Errorf("invalid hostname %q: empty or too-long label", h)
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '-' && i != 0 && i != len(label)-1:
			default:
				return fmt.Errorf("invalid character in hostname %q", h)
			}
		}
	}
	if !strings.Contains(h, ".") {
		return fmt.Errorf("hostname %q must be fully qualified (contain a dot)", h)
	}
	return nil
}

func validSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(s)-1:
		default:
			return false
		}
	}
	return true
}

// servePublic accepts public HTTP connections, peeks the Host header, then
// pipes the connection to the matching tunnel via a new yamux stream.
func servePublic(reg *registry, acl *ipACL, pks *passkeyStore, wa *webauthn.WebAuthn, locks *tunnelLocks, invites *inviteStore, lan *lanManager, addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("public listen: %v", err)
	}
	log.Printf("public HTTP listener on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("public accept: %v", err)
			continue
		}
		go handlePublicConn(reg, acl, pks, wa, locks, invites, lan, conn)
	}
}

func servePublicTLS(reg *registry, acl *ipACL, pks *passkeyStore, wa *webauthn.WebAuthn, locks *tunnelLocks, invites *inviteStore, lan *lanManager, addr string, cfg *tls.Config) {
	ln, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		log.Fatalf("public tls listen: %v", err)
	}
	log.Printf("public HTTPS listener on %s", addr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("public tls accept: %v", err)
			continue
		}
		go handlePublicConn(reg, acl, pks, wa, locks, invites, lan, conn)
	}
}

func handlePublicConn(reg *registry, acl *ipACL, pks *passkeyStore, wa *webauthn.WebAuthn, locks *tunnelLocks, invites *inviteStore, lan *lanManager, conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	rawHeaders, host, err := readHeadersAndHost(br)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		writeHTTPError(conn, 400, "bad request")
		conn.Close()
		return
	}

	hostNoPort := host
	if i := strings.LastIndex(hostNoPort, ":"); i >= 0 {
		hostNoPort = hostNoPort[:i]
	}
	hostNoPort = strings.ToLower(hostNoPort)

	if isManagementHost(hostNoPort, *publicHost) {
		// The management host carries the admin magic-link token, invite
		// links, session cookies, and the script people pipe into bash.
		// None of that belongs on plaintext HTTP, so when a TLS listener
		// exists, send cleartext callers there first.
		if *httpsAddr != "" && !isTLSConn(conn) {
			_, uri := parseRequestLine(rawHeaders)
			// 308 rather than 302: it preserves the method and body, so a
			// mid-flight admin POST isn't silently downgraded to a GET.
			writeHTTPRedirectStatus(conn, 308, "https://"+hostNoPort+uri)
			conn.Close()
			return
		}
		// Admin host is exempt from the IP ACL — otherwise you could
		// lock yourself out, and /install must remain reachable from
		// arbitrary networks.
		serveAdmin(conn, rawHeaders, br, acl, reg, pks, wa, locks, invites)
		return
	}

	clientIP := remoteIPFrom(conn.RemoteAddr())

	// Two routing paths: subdomains under our public host go via the
	// wildcard-cert path; anything else may be a CNAME'd custom host
	// that some client has registered.
	sub := subdomainOf(host, *publicHost)
	var t *tunnel
	if sub != "" {
		t = reg.get(sub)
	} else {
		t = reg.lookup(hostNoPort)
		if t != nil {
			sub = t.subdomain
		}
	}

	// Same-NAT detection: when the visitor's public IP matches the
	// client's, the visitor is almost certainly on the same LAN. 307
	// to the per-tunnel sister hostname (<sub>-lan.<publicHost>),
	// whose public DNS A record points at the client's RFC1918 IP, so
	// the request bypasses us entirely. Skipped on:
	//   - HTTP (we 307 to https only — wildcard cert is HTTPS-only)
	//   - custom hostnames (different origin, cookie/cert mismatch)
	//   - explicit ?nolan=1 escape hatch (manual fallback)
	//   - tunnels without LAN-direct enabled
	if t != nil && t.lanHostname != "" && t.publicIP != "" && clientIP == t.publicIP {
		_, isTLS := conn.(*tls.Conn)
		_, reqURI := parseRequestLine(rawHeaders)
		isPublicHost := sub != "" && hostNoPort == sub+"."+*publicHost
		if isTLS && isPublicHost && !strings.Contains(reqURI, "nolan=1") {
			redirect := "https://" + t.lanHostname + reqURI
			writeHTTPRedirectStatus(conn, 307, redirect)
			conn.Close()
			return
		}
	}

	// Per-tunnel IP allow/block. The check needs the resolved subdomain
	// so it can consult per-tunnel rules. Global blocklist always fires;
	// per-tunnel allow/block only fires when sub != "". Reject before
	// the request crosses the tunnel so blocked clients don't burn
	// bandwidth.
	if !acl.Check(sub, net.ParseIP(clientIP)) {
		writeHTTPError(conn, 403, "your IP is not permitted: "+clientIP)
		conn.Close()
		return
	}

	// Per-tunnel passkey gate. If this tunnel is locked AND no
	// matching credential authenticated the visitor as a user the
	// tunnel allows, redirect to the login page on the admin host.
	// The gate is auto-bypassed when no credentials exist anywhere —
	// so a misclicked grant can't strand you before any passkey is
	// registered.
	if sub != "" && locks.IsLocked(sub) && pks.HasCredentials() {
		sid := readPKSessionFromCookies(rawHeaders)
		userID := pks.SessionUser(sid)
		if userID == "" || !locks.AllowsUser(sub, userID) {
			scheme := "https"
			if _, isTLS := conn.(*tls.Conn); !isTLS {
				scheme = "http"
			}
			_, reqURI := parseRequestLine(rawHeaders)
			returnURL := scheme + "://" + host + reqURI
			loginURL := "https://tunnel." + *publicHost + "/auth?return=" + url.QueryEscape(returnURL)
			writeHTTPRedirect(conn, loginURL)
			conn.Close()
			return
		}
	}
	if t == nil {
		// Special-case favicons before falling through to the offline /
		// not-found page so the offline page's tab icon resolves.
		if _, p := parseRequestLine(rawHeaders); p == "/favicon.ico" || p == "/favicon.svg" {
			writeHTTP(conn, 200, "image/svg+xml", []byte(faviconSVG))
			conn.Close()
			return
		}
		if sub != "" {
			fullHost := sub + "." + *publicHost
			body := []byte(fmt.Sprintf(offlinePage, fullHost, sub, "tunnel."+*publicHost))
			writeHTTP(conn, 502, "text/html; charset=utf-8", body)
		} else {
			// Unknown host (not under publicHost and no matching custom
			// hostname registration) — most likely a stale/wrong CNAME or
			// a probe.
			writeHTTPError(conn, 404, "no tunnel for host: "+hostNoPort)
		}
		conn.Close()
		return
	}

	// Inject the requester's IP so the local app (and the mygrok client's
	// request log) can see it. We strip any inbound X-Forwarded-For /
	// X-Real-IP first — the public internet can claim anything, and we
	// are the trust boundary. (clientIP was computed above for the ACL.)
	rawHeaders = injectClientIP(rawHeaders, clientIP)

	stream, err := t.session.Open()
	if err != nil {
		// Session is shutting down or already dead. Evict it so the next
		// reconnecting client (or the next request) doesn't keep hitting
		// the same corpse — without this, a stale session can sit in the
		// registry until yamux's own keepalive notices.
		log.Printf("open stream for %s: %v (evicting stale session)", sub, err)
		t.session.Close()
		reg.release(sub, t.session)
		fullHost := sub + "." + *publicHost
		body := []byte(fmt.Sprintf(offlinePage, fullHost, sub, "tunnel."+*publicHost))
		writeHTTP(conn, 502, "text/html; charset=utf-8", body)
		conn.Close()
		return
	}

	// Write the captured request headers to the tunnel, then full-duplex copy.
	if _, err := stream.Write(rawHeaders); err != nil {
		stream.Close()
		conn.Close()
		return
	}

	go func() {
		// Anything still buffered in br plus continued reads from conn → stream.
		_, _ = io.Copy(stream, br)
		stream.Close()
	}()
	_, _ = io.Copy(conn, stream)
	conn.Close()
}

// readHeadersAndHost reads bytes until the end of the HTTP request headers
// (\r\n\r\n) and returns them along with the parsed Host header value.
func readHeadersAndHost(br *bufio.Reader) ([]byte, string, error) {
	var raw []byte
	var host string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return raw, host, err
		}
		raw = append(raw, line...)
		if line == "\r\n" || line == "\n" {
			return raw, host, nil
		}
		if len(raw) > 64*1024 {
			return raw, host, fmt.Errorf("headers too large")
		}
		// Capture Host: ...
		if l := strings.ToLower(line); strings.HasPrefix(l, "host:") {
			host = strings.TrimSpace(line[5:])
			host = strings.TrimRight(host, "\r\n")
		}
	}
}

func subdomainOf(host, base string) string {
	// Drop port if present.
	if i := strings.LastIndex(host, ":"); i >= 0 {
		// IPv6 literal not handled (we don't expect it on Host header).
		host = host[:i]
	}
	host = strings.ToLower(host)
	if host == base {
		return ""
	}
	if !strings.HasSuffix(host, "."+base) {
		return ""
	}
	return host[:len(host)-len(base)-1]
}

// remoteIPFrom extracts the host portion of a net.Addr, dropping the port.
func remoteIPFrom(addr net.Addr) string {
	s := addr.String()
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}

// injectClientIP rewrites a raw HTTP request header block to carry the
// real requester IP. It strips any inbound X-Forwarded-For / X-Real-IP
// (the public internet is untrusted) and appends our own values so the
// local app and the mygrok client log can rely on them.
func injectClientIP(rawHeaders []byte, clientIP string) []byte {
	crlf := []byte("\r\n")
	blank := []byte("\r\n\r\n")
	if !bytes.Contains(rawHeaders, crlf) {
		crlf = []byte("\n")
		blank = []byte("\n\n")
	}
	idx := bytes.Index(rawHeaders, blank)
	if idx < 0 {
		return rawHeaders
	}
	headerBlock := rawHeaders[:idx]

	var keep [][]byte
	for _, line := range bytes.Split(headerBlock, crlf) {
		lower := bytes.ToLower(bytes.TrimLeft(line, " \t"))
		if bytes.HasPrefix(lower, []byte("x-forwarded-for:")) ||
			bytes.HasPrefix(lower, []byte("x-real-ip:")) {
			continue
		}
		// mygrok's own session cookies are strictly between the visitor and
		// this server. They are scoped to the whole zone, so forwarding one
		// into a tunnel would hand that tunnel's operator a credential
		// that also works against every other tunnel the visitor can reach.
		if bytes.HasPrefix(lower, []byte("cookie:")) {
			line = stripMygrokCookies(line)
			if line == nil {
				continue
			}
		}
		keep = append(keep, line)
	}
	keep = append(keep,
		[]byte("X-Forwarded-For: "+clientIP),
		[]byte("X-Real-IP: "+clientIP),
	)

	out := bytes.Join(keep, crlf)
	out = append(out, blank...)
	return out
}

// isTLSConn reports whether this connection arrived on the HTTPS listener.
// servePublicTLS hands us a *tls.Conn; servePublic hands us a bare one.
func isTLSConn(c net.Conn) bool {
	_, ok := c.(*tls.Conn)
	return ok
}

// stripMygrokCookies removes mygrok's own session cookies from a raw
// `Cookie:` header line, preserving every other pair and the original
// header name. Returns nil when nothing is left to send.
func stripMygrokCookies(line []byte) []byte {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return line
	}
	name := line[:colon]
	var kept []string
	for _, pair := range strings.Split(string(line[colon+1:]), ";") {
		trimmed := strings.TrimSpace(pair)
		if trimmed == "" {
			continue
		}
		key := trimmed
		if eq := strings.IndexByte(trimmed, '='); eq >= 0 {
			key = trimmed[:eq]
		}
		switch strings.TrimSpace(key) {
		case pkSessionCookie, adminCookieName, pkLoginCookie, pkRegCookie:
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return nil
	}
	return append(append(append([]byte{}, name...), ": "...), []byte(strings.Join(kept, "; "))...)
}

func writeHTTPError(w io.Writer, code int, msg string) {
	body := fmt.Sprintf("%d %s\nmygrok: %s\n", code, http.StatusText(code), msg)
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}

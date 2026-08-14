package main

// `mygrok serve [dir]` — quick static file server, optionally tunneled.
//
// Like `python -m http.server` or `serve` (https://formulae.brew.sh/formula/serve),
// but with the option to expose the result through your mygrok tunnel by
// passing --subdomain=<name>. With a subdomain, mygrok starts the static
// server on a local port and immediately registers the tunnel pointing at it,
// so a single command gets you a public URL for any folder.

import (
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/schappim/mygrok/internal/branding"
)

func cmdServe(args []string) {
	cfg, cfgFrom, err := loadConfig(scanFlag(args, "--config"))
	if err != nil {
		exitf("%v", err)
	}

	defServer := resolveServer(cfg)
	defAuth := ""
	if cfg != nil && cfg.Auth != "" {
		defAuth = cfg.Auth
	}
	if v := os.Getenv("MYGROK_AUTHTOKEN"); v != "" {
		defAuth = v
	}
	if defAuth == "" {
		defAuth = readTokenFile()
	}
	defSub := ""
	if cfg != nil {
		defSub = cfg.Subdomain
	}

	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 8080, "local port to listen on (0 = pick a free port)")
	host := fs.String("host", "127.0.0.1", "interface to bind locally (use 0.0.0.0 to expose on LAN)")
	subdomain := fs.String("subdomain", defSub, "if set, also expose via mygrok at <subdomain>.<server>")
	server := fs.String("server", defServer, "tunnel server")
	auth := fs.String("auth", defAuth, "auth token (only needed with --subdomain)")
	indexFile := fs.String("index", "index.html", "filename used as the directory index when present")
	basicAuth := fs.String("basic-auth", "", `protect with HTTP basic auth, e.g. "alice:s3cret"`)
	hostnames := fs.String("hostname", "", "additional public hostname (CNAME) to claim, e.g. app.example.com (comma-separate for multiple)")
	lanFlag := fs.String("lan", "", `enable LAN-direct: "auto" or an explicit RFC1918 IPv4 (only used in tunnel mode)`)
	lanPort := fs.Int("lan-port", 8443, "port for the LAN-direct TLS listener (only used with --lan)")
	configFlag := fs.String("config", "", "explicit path to a .mygrok.toml file")
	_ = configFlag

	// Split flags from positionals so we can parse flags first (honouring
	// an explicit --subdomain) and then interpret positionals against the
	// resulting state.
	var positional, flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)

	// Positional grammar:
	//   mygrok serve                       → standalone, dir="."
	//   mygrok serve <sub>                 → tunnel <sub>, dir="."
	//   mygrok serve <sub> <dir>           → tunnel <sub>, dir=<dir>
	//   mygrok serve <dir>                 → standalone, dir=<dir>      (when arg looks like a path)
	//   mygrok serve --subdomain=<sub> <dir>  → tunnel <sub>, dir=<dir> (backwards-compat form)
	dir := "."
	randomSub := false
	switch {
	case *subdomain != "":
		// Explicit flag: first positional, if any, is the directory.
		if len(positional) >= 1 {
			dir = positional[0]
		}
	case len(positional) == 0:
		// `mygrok serve` with no args → tunnel the cwd under a random
		// subdomain. The user gets a public URL with one keystroke.
		*subdomain = randomSubdomain()
		randomSub = true
	case looksLikeSubdomain(positional[0]):
		*subdomain = positional[0]
		if len(positional) >= 2 {
			dir = positional[1]
		}
	default:
		// First positional is a path → standalone mode.
		dir = positional[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		exitf("resolve dir: %v", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		exitf("stat %s: %v", abs, err)
	}
	if !fi.IsDir() {
		exitf("not a directory: %s", abs)
	}

	if cfgFrom != "" {
		fmt.Fprintf(os.Stderr, "(config: %s)\n", cfgFrom)
	}

	authExpected, err := parseBasicAuthFlag(resolveBasicAuth(*basicAuth))
	if err != nil {
		exitf("%v", err)
	}

	// Track whether the user explicitly chose a port. If they did, a
	// collision is fatal (they wanted *that* port). If they didn't, a
	// busy default (8080) silently falls back to a kernel-picked free
	// one — handy when running multiple `mygrok serve` from the same
	// box for different folders.
	portExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portExplicit = true
		}
	})

	// Bind first so we can report the actual port (matters when --port=0).
	bind := net.JoinHostPort(*host, fmt.Sprintf("%d", *port))
	ln, err := net.Listen("tcp", bind)
	if err != nil && !portExplicit && strings.Contains(err.Error(), "address already in use") {
		fallback := net.JoinHostPort(*host, "0")
		ln, err = net.Listen("tcp", fallback)
		if err == nil {
			fmt.Fprintf(os.Stderr, "(port %d busy; using free port %d instead)\n",
				*port, ln.Addr().(*net.TCPAddr).Port)
		}
	}
	if err != nil {
		exitf("listen %s: %v", bind, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	log.SetFlags(log.Ltime)

	// Standalone mode: just run the file server in the foreground with
	// per-request logging (matches the tunnel-mode log style).
	if *subdomain == "" {
		fmt.Println()
		fmt.Println("  mygrok serve")
		fmt.Println()
		fmt.Printf("  %-12s %s\n", "Directory", abs)
		fmt.Printf("  %-12s http://%s:%d/\n", "Local", *host, actualPort)
		if authExpected != "" {
			fmt.Printf("  %-12s HTTP basic auth required\n", "Auth")
		}
		fmt.Println()
		fmt.Println("  HTTP Requests")
		fmt.Println("  -------------")
		fmt.Println()
		handler := withRequestLog(requireBasicAuth(staticHandler(abs, *indexFile), authExpected))
		if err := http.Serve(ln, handler); err != nil {
			log.Fatalf("serve: %v", err)
		}
		return
	}

	// Tunnel mode. The tunnel agent (runReconnectLoop -> proxyToLocal)
	// already logs each request, so we don't double-log here. The static
	// server runs in a goroutine; the reconnect loop owns the foreground.
	if *auth == "" {
		exitf("auth token required for --subdomain (MYGROK_AUTHTOKEN, --auth, ~/.mygrok/authtoken, or auth = ... in .mygrok.toml)")
	}
	// Resolve before printing a banner that promises a public URL: without a
	// server there isn't going to be one, and failing after the banner reads
	// like the tunnel came up and then died.
	tunnelServer := requireServer(*server)

	fmt.Println()
	fmt.Println("  mygrok serve")
	fmt.Println()
	fmt.Printf("  %-12s %s\n", "Directory", abs)
	fmt.Printf("  %-12s http://%s:%d/\n", "Local", *host, actualPort)
	if randomSub {
		fmt.Printf("  %-12s %s (auto-generated; pass <name> to choose your own)\n", "Subdomain", *subdomain)
	}
	if authExpected != "" {
		fmt.Printf("  %-12s HTTP basic auth required\n", "Auth")
	}

	go func() {
		if err := http.Serve(ln, requireBasicAuth(staticHandler(abs, *indexFile), authExpected)); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	lanIP, err := resolveLANFlag(*lanFlag)
	if err != nil {
		exitf("%v", err)
	}
	if err := checkLANCompatibility(lanIP, authExpected, mcpSecret); err != nil {
		exitf("%v", err)
	}

	// Tunnel always points at loopback regardless of --host (the tunnel
	// agent runs on this machine, so 127.0.0.1 is always the right target).
	target := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", actualPort))
	runReconnectLoop(tunnelServer, *auth, *subdomain, target, splitCSV(*hostnames), lanIP, *lanPort)
}

// staticHandler wraps http.FileServer with a configurable index file and a
// default-favicon fallback (so a folder without favicon.ico doesn't render
// a broken tab icon). http.FileServer handles directory listings,
// content-type sniffing, range requests, and 304s on its own.
func staticHandler(root, indexName string) http.Handler {
	fs := http.FileServer(http.Dir(root))
	customIndex := indexName != "" && indexName != "index.html"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Custom index for directory requests.
		if customIndex && strings.HasSuffix(r.URL.Path, "/") {
			candidate := filepath.Join(root, filepath.FromSlash(r.URL.Path), indexName)
			if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
				http.ServeFile(w, r, candidate)
				return
			}
		}
		// Favicon fallback: only if the served tree has no favicon.ico
		// and no favicon.svg of its own. http.FileServer will gladly
		// serve either if they exist (right content-type and all).
		if r.URL.Path == "/favicon.ico" || r.URL.Path == "/favicon.svg" {
			if !diskHasFavicon(root) {
				w.Header().Set("Content-Type", "image/svg+xml")
				w.Header().Set("Cache-Control", "public, max-age=86400")
				_, _ = w.Write([]byte(branding.FaviconSVG))
				return
			}
		}
		fs.ServeHTTP(w, r)
	})
}

func diskHasFavicon(root string) bool {
	for _, name := range []string{"favicon.ico", "favicon.svg"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// withRequestLog records the response status and emits the same per-request
// log line the tunnel agent uses, so standalone-mode output looks consistent
// with `mygrok http`. Skipped in tunnel mode (proxyToLocal already logs).
func withRequestLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		ip := r.Header.Get("X-Real-IP")
		if ip == "" {
			ip = remoteIPHost(r.RemoteAddr)
		}
		logRequest(ip, r.Method, r.URL.RequestURI(), rec.status, time.Since(start), nil)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func remoteIPHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// randomSubdomain returns a 6-character lowercase-letter slug for use as
// a throwaway subdomain when the user invokes `mygrok serve` with no
// args. ~309M combinations — collisions are essentially never an issue
// for ad-hoc share links, but the user can always pass an explicit name
// if they want something memorable.
func randomSubdomain() string {
	const chars = "abcdefghijklmnopqrstuvwxyz"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback that still satisfies validSubdomain rules.
		return fmt.Sprintf("s%x", time.Now().UnixNano())
	}
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b[:])
}

// looksLikeSubdomain decides whether a positional argument should be
// treated as a subdomain (the new ergonomic form `mygrok serve foo ./`)
// or as a directory (the legacy form `mygrok serve ./photos`). Anything
// containing a path separator, `.`, or `~` is clearly a path; otherwise
// it must match the subdomain charset to qualify.
func looksLikeSubdomain(s string) bool {
	if s == "" || strings.ContainsAny(s, "/.\\~") {
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

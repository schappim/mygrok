// mygrok is the local tunnel agent. Usage matches ngrok's basic CLI:
//
//	mygrok http <port> [--subdomain=<name>] [--server=<host:port>]
//
// It connects to the mygrok server, registers the subdomain, then forwards
// every yamux stream the server pushes to it to localhost:<port>.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/schappim/mygrok/internal/buildinfo"
	"github.com/schappim/mygrok/internal/proto"
)

// clientID is a per-process random ID. The server uses it to recognise a
// reconnecting client and pre-empt its own stale session — so a network
// blip heals in <1s instead of waiting ~15s for the dead session's
// keepalive to expire on the server.
var clientID = generateClientID()

func generateClientID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// yamuxConfig matches the server's: 10s keepalive, 5s write timeout. Faster
// dead-peer detection on both sides, and well under any home-router NAT
// idle threshold.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.ConnectionWriteTimeout = 5 * time.Second
	return cfg
}

func tuneTCPKeepalive(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
}

func main() {
	// No subcommand → default to `http`. Picks up .mygrok.toml if present;
	// otherwise emits the same "missing <port>" / "--subdomain is required"
	// errors as `mygrok http` with empty args.
	//
	// A leading flag counts as "no subcommand" too, so `mygrok
	// --subdomain=preview` works next to a config file — otherwise it would
	// fall through to the switch's default and print usage, which is not
	// what a flag on a command with a default subcommand should do.
	if len(os.Args) < 2 {
		cmdHTTP(nil)
		return
	}
	if strings.HasPrefix(os.Args[1], "-") && !isHelpFlag(os.Args[1]) {
		cmdHTTP(os.Args[1:])
		return
	}
	switch os.Args[1] {
	case "http":
		cmdHTTP(os.Args[2:])
	case "mcp":
		cmdMCP(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "admin":
		cmdAdmin(os.Args[2:])
	case "version":
		printVersion()
	case "-h", "--help", "help":
		usage()
	default:
		usage()
	}
}

// isHelpFlag reports whether an argument is one of the help spellings, which
// must reach usage() rather than being parsed as an `http` flag.
func isHelpFlag(a string) bool {
	switch a {
	case "-h", "--help", "-help":
		return true
	}
	return false
}

func usage() {
	fmt.Fprintln(os.Stderr, `mygrok — DIY tunnel client

Usage:
  mygrok http  <port> [--subdomain=<name>] [--hostname=<host>] [--basic-auth=<u>:<p>] [--lan=auto] [--server=<host:port>] [--auth=<token>]
  mygrok mcp   <port> [--subdomain=<name>] [--secret=<token>] [--no-strip] [--path=/mcp]
  mygrok serve [<subdomain>] [dir]  [--port=<n>] [--hostname=<host>] [--basic-auth=<u>:<p>] [--lan=auto]
  mygrok service install   <port> --subdomain=<name> [--name=<svc>] [--mcp]
  mygrok service uninstall <name>
  mygrok service list
  mygrok service status <name>
  mygrok service logs   <name>
  mygrok admin
  mygrok update
  mygrok version

Examples:
  mygrok http  3000 --subdomain=app              # https://app.<your-domain> -> localhost:3000
  mygrok mcp   8790 --subdomain=tools            # expose a local MCP server as a claude.ai connector
  mygrok serve gallery ./photos                  # static file server + tunnel at gallery.<your-domain>
  mygrok serve gallery                           # tunnel cwd at gallery.<your-domain>
  mygrok serve                                   # tunnel cwd at a random subdomain — this is public
  mygrok serve ./photos                          # local only, http://127.0.0.1:8080, no tunnel
  mygrok service install 3000 --subdomain=app    # auto-start on login/boot
  mygrok service install 8790 --subdomain=tools --mcp   # persistent MCP connector

Environment:
  MYGROK_AUTHTOKEN   shared auth token
  MYGROK_SERVER      default server address (host:port)
  MYGROK_BASIC_AUTH  user:pass for --basic-auth, kept out of argv

Docs: https://github.com/schappim/mygrok`)
	os.Exit(2)
}

// printVersion reports the link-time version plus, when one was stamped in,
// the server this binary defaults to — the fastest way to tell a generic
// build apart from one downloaded off a specific mygrokd.
func printVersion() {
	fmt.Printf("mygrok %s\n", buildinfo.Version)
	if buildinfo.DefaultServer != "" {
		fmt.Printf("default server: %s\n", buildinfo.DefaultServer)
	}
}

func cmdHTTP(args []string) {
	// Resolve config before flag parsing so config values can populate the
	// flag-package defaults. CLI flags then naturally override config; env
	// is layered between (env > config > built-in default).
	cfg, cfgFrom, err := loadConfig(scanFlag(args, "--config"))
	if err != nil {
		exitf("%v", err)
	}

	defSub := ""
	if cfg != nil {
		defSub = cfg.Subdomain
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
	defHost := "127.0.0.1"
	if cfg != nil && cfg.Host != "" {
		defHost = cfg.Host
	}

	fs := flag.NewFlagSet("http", flag.ExitOnError)
	subdomain := fs.String("subdomain", defSub, "requested public subdomain (required)")
	server := fs.String("server", defServer, "server address")
	auth := fs.String("auth", defAuth, "auth token")
	host := fs.String("host", defHost, "local host to forward to")
	basicAuth := fs.String("basic-auth", "", `protect the tunnel with HTTP basic auth, e.g. "alice:s3cret"`)
	hostnames := fs.String("hostname", "", "additional public hostname (CNAME) to claim, e.g. app.example.com (comma-separate for multiple)")
	lanFlag := fs.String("lan", "", `enable LAN-direct: visitors on the same NAT 307 to <sub>-lan.<publicHost> and hit this box directly. Pass "auto" to auto-detect, or a specific RFC1918 IPv4 to pin it.`)
	lanPort := fs.Int("lan-port", 8443, "port for the LAN-direct TLS listener (only used with --lan)")
	configFlag := fs.String("config", "", "explicit path to a .mygrok.toml file")
	_ = configFlag // value consumed via scanFlag above

	// `mygrok http 3000` — port as positional. Split positional from flag args.
	var port string
	var rest []string
	for _, a := range args {
		if port == "" && !strings.HasPrefix(a, "-") {
			port = a
		} else {
			rest = append(rest, a)
		}
	}
	fs.Parse(rest)

	if port == "" && cfg != nil && cfg.Port != 0 {
		port = fmt.Sprintf("%d", cfg.Port)
	}
	if port == "" {
		fmt.Fprintln(os.Stderr, "missing <port> (pass on CLI or set port = ... in .mygrok.toml)")
		usage()
	}
	if *subdomain == "" {
		fmt.Fprintln(os.Stderr, "--subdomain is required (pass on CLI or set subdomain = ... in .mygrok.toml)")
		os.Exit(2)
	}
	if *auth == "" {
		fmt.Fprintln(os.Stderr, "auth token required: set MYGROK_AUTHTOKEN, pass --auth, write the token to ~/.mygrok/authtoken, or set auth = ... in .mygrok.toml")
		os.Exit(2)
	}
	if cfgFrom != "" {
		fmt.Fprintf(os.Stderr, "(config: %s)\n", cfgFrom)
	}

	expected, err := parseBasicAuthFlag(resolveBasicAuth(*basicAuth))
	if err != nil {
		exitf("%v", err)
	}
	basicAuthExpected = expected

	lanIP, err := resolveLANFlag(*lanFlag)
	if err != nil {
		exitf("%v", err)
	}
	if err := checkLANCompatibility(lanIP, basicAuthExpected, mcpSecret); err != nil {
		exitf("%v", err)
	}

	// Time-only log prefix keeps the per-request lines compact.
	log.SetFlags(log.Ltime)

	target := net.JoinHostPort(*host, port)
	runReconnectLoop(requireServer(*server), *auth, *subdomain, target, splitCSV(*hostnames), lanIP, *lanPort)
}

// checkLANCompatibility refuses --lan alongside gates the LAN listener
// cannot enforce.
//
// LAN-direct works by 307ing same-NAT visitors to a listener that proxies
// straight to the local backend. That listener is not the tunnel: it never
// sees the server's IP rules or passkey gate, and it does not run the
// basic-auth or MCP capability checks that forwardOne applies. Quietly
// combining them would leave a protected tunnel with an unprotected door
// beside it, open to anyone sharing the visitor's public IP.
func checkLANCompatibility(lanIP, basicAuth, mcp string) error {
	if lanIP == "" {
		return nil
	}
	switch {
	case mcp != "":
		return fmt.Errorf("--lan cannot be combined with MCP mode: the LAN-direct listener does not enforce the capability-URL gate")
	case basicAuth != "":
		return fmt.Errorf("--lan cannot be combined with --basic-auth: the LAN-direct listener does not enforce it")
	}
	return nil
}

// resolveLANFlag turns the --lan value into a concrete RFC1918 IPv4
// address ("" means off). "auto" picks the first private IP on the
// box. Anything else is parsed as a literal IPv4.
func resolveLANFlag(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if v == "auto" {
		ip := pickLANIP()
		if ip == "" {
			return "", fmt.Errorf("--lan=auto: no RFC1918 IPv4 found on this box; pin one explicitly with --lan=192.168.x.y")
		}
		return ip, nil
	}
	parsed := net.ParseIP(v)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("--lan=%q is not a valid IPv4", v)
	}
	if !parsed.IsPrivate() {
		return "", fmt.Errorf("--lan=%q must be RFC1918 (10/8, 172.16/12, 192.168/16)", v)
	}
	return parsed.String(), nil
}

// splitCSV trims and de-duplicates a comma-separated string into a slice.
// Used by --hostname=a.com,b.com.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// basicAuthExpected is the precomputed Authorization header value that
// authorized requests must carry, or "" when --basic-auth is not set.
// Set once at startup by cmdHTTP; read by forwardOne. Package-level
// because mygrok runs one tunnel per process.
var basicAuthExpected string

// runReconnectLoop is the long-lived tunnel client loop. It calls runOnce,
// classifies the disconnect cause, sleeps an appropriate backoff, and
// repeats forever.
func runReconnectLoop(server, auth, subdomain, target string, hostnames []string, lanIP string, lanPort int) {
	attempt := 1
	for {
		start := time.Now()
		err := runOnce(server, auth, subdomain, target, hostnames, lanIP, lanPort)
		log.Printf("disconnected: %v", err)
		// Reset backoff after a session that ran healthily for a while —
		// otherwise long-running tunnels would keep climbing the backoff
		// after their first reconnect.
		if time.Since(start) > 60*time.Second {
			attempt = 1
		}
		var backoff time.Duration
		if isSubdomainInUse(err) {
			// Server says the slot is taken. With same-clientID takeover
			// in place this should be rare (a different client owns it,
			// or our previous session hasn't been evicted yet). Retry
			// quickly with jitter — the heavy backoff is meant for "server
			// is down", not "server says wait."
			backoff = 1*time.Second + jitter(500*time.Millisecond)
		} else {
			backoff = time.Duration(attempt)*2*time.Second + jitter(1*time.Second)
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		log.Printf("reconnecting in %s...", backoff.Round(100*time.Millisecond))
		time.Sleep(backoff)
		attempt++
	}
}

func isSubdomainInUse(err error) bool {
	return err != nil && strings.Contains(err.Error(), "subdomain in use")
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(mathrand.Int63n(int64(d)))
}

func runOnce(server, auth, subdomain, target string, hostnames []string, lanIP string, lanPort int) error {
	conn, err := net.DialTimeout("tcp", server, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial server: %w", err)
	}
	defer conn.Close()
	tuneTCPKeepalive(conn)

	if err := proto.WriteJSONLine(conn, proto.Hello{
		Version:   proto.Version,
		Auth:      auth,
		Subdomain: subdomain,
		Proto:     "http",
		ClientID:  clientID,
		Hostnames: hostnames,
		LANIP:     lanIP,
		LANPort:   lanPort,
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	br := bufio.NewReader(conn)
	var resp proto.HelloResp
	if err := proto.ReadJSONLine(br, &resp); err != nil {
		return fmt.Errorf("read hello resp: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("server rejected: %s", resp.Error)
	}
	if br.Buffered() > 0 {
		return fmt.Errorf("unexpected bytes after hello: %d", br.Buffered())
	}

	// Spin up the LAN-direct TLS listener if the server confirmed the
	// handshake and shipped us a cert. Same-LAN visitors will be 307'd
	// here from the tunnel server. Tear it down when this runOnce
	// returns so reconnects don't leak listeners.
	var lanStop func()
	if resp.LANHostname != "" && resp.LANCertPEM != "" && resp.LANKeyPEM != "" && lanIP != "" {
		stop, err := serveLAN(lanIP, lanPort, resp.LANHostname, resp.LANCertPEM, resp.LANKeyPEM, target)
		if err != nil {
			log.Printf("lan: %v (continuing without LAN-direct)", err)
		} else {
			lanStop = stop
			defer lanStop()
		}
	}

	urls := resp.URLs
	if len(urls) == 0 && resp.URL != "" {
		urls = []string{resp.URL}
	}
	fmt.Println()
	fmt.Println("  mygrok tunnel active")
	fmt.Println()
	for i, u := range urls {
		label := "Forwarding  "
		if i > 0 {
			label = "            "
		}
		fmt.Printf("  %s %-40s -> %s\n", label, u, target)
	}
	if lanStop != nil {
		fmt.Printf("  %s %-40s -> %s (same-LAN bypass)\n", "LAN-direct  ",
			fmt.Sprintf("https://%s:%d", resp.LANHostname, lanPort), target)
	}
	if mcpConnectorPath != "" {
		httpsURL := ""
		for _, u := range urls {
			if strings.HasPrefix(u, "https://") {
				httpsURL = u
				break
			}
		}
		if httpsURL == "" && len(urls) > 0 {
			httpsURL = urls[0]
		}
		fmt.Println()
		fmt.Println("  MCP connector URL (the URL is the credential — treat it as a secret):")
		fmt.Printf("    %s%s\n", httpsURL, mcpConnectorPath)
	}
	fmt.Println()
	fmt.Println("  HTTP Requests")
	fmt.Println("  -------------")
	fmt.Println()

	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		return fmt.Errorf("yamux client: %w", err)
	}
	defer sess.Close()

	for {
		stream, err := sess.Accept()
		if err != nil {
			return fmt.Errorf("accept stream: %w", err)
		}
		go proxyToLocal(stream, target)
	}
}

// proxyToLocal handles the lifetime of one yamux stream. A stream may carry
// several pipelined HTTP/1.1 requests (browser keep-alive); each request is
// served on its own freshly-dialled local TCP connection. We deliberately do
// not reuse one local conn across requests on the same stream: the local
// server (e.g. Puma's persistent_timeout, default 20s) can close an idle
// keep-alive socket between requests, and the next request would race the
// FIN and surface as a spurious "unexpected EOF" or "read: connection reset"
// to the public client.
//
// On a WebSocket-style Upgrade we forward the 101 and switch to raw byte
// pumping for the rest of the connection — there is no more HTTP framing
// past that point.
func proxyToLocal(stream net.Conn, target string) {
	defer stream.Close()
	srcBR := bufio.NewReader(stream)

	first := true
	for {
		if first {
			// 30s deadline only on the FIRST read of a fresh stream — if
			// nothing arrives, the stream was likely opened in error.
			// Subsequent requests on the same stream may arrive much later
			// (idle browser tab) so we do not deadline those.
			_ = stream.SetReadDeadline(time.Now().Add(30 * time.Second))
		}
		req, err := http.ReadRequest(srcBR)
		if first {
			_ = stream.SetReadDeadline(time.Time{})
			first = false
		}
		if err != nil {
			return
		}
		if !forwardOne(req, srcBR, stream, target) {
			return
		}
	}
}

// forwardOne dials a fresh local TCP connection, writes one HTTP request,
// reads the response, and writes it back to stream. Returns true if more
// requests can still be served on the same yamux stream; false on error or
// after a protocol upgrade (after which the connection is no longer HTTP).
func forwardOne(req *http.Request, srcBR *bufio.Reader, stream net.Conn, target string) bool {
	method := req.Method
	path := req.URL.RequestURI()
	clientIP := req.Header.Get("X-Real-IP")
	start := time.Now()
	upgrade := isUpgradeRequest(req)

	// In MCP mode the first path segment is the capability token, so it must
	// never reach the log — the log is a file on disk, and the token is the
	// entire credential.
	if mcpSecret != "" {
		path = mcpRedactPath(path)
	}

	// MCP capability-URL gate (when `mygrok mcp` is active). Checked before
	// basic auth so unauthorized probes learn nothing but a bare 404.
	if mcpSecret != "" {
		restPath, ok := mcpGate(req.URL.Path, mcpSecret)
		if !ok {
			body := []byte("Not Found\n")
			fmt.Fprintf(stream,
				"HTTP/1.1 404 Not Found\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
				len(body), body)
			logRequest(clientIP, method, path, 404, time.Since(start), nil)
			return false
		}
		if mcpStrip {
			// Forward the path with the token segment removed. RawPath and
			// RequestURI are derived views of the same URI; clear them so
			// req.Write rebuilds from the rewritten Path.
			req.URL.Path = restPath
			req.URL.RawPath = ""
			req.RequestURI = ""
		}
	}

	// Basic-auth gate (when --basic-auth is set). Reject before we dial
	// the backend; tear down the stream to avoid the complexity of
	// draining the (possibly large) request body of an unauth'd POST.
	// Browsers will reconnect with credentials and the second request
	// flows normally.
	if !basicAuthOK(req.Header.Get("Authorization"), basicAuthExpected) {
		body := []byte("Unauthorized\n")
		fmt.Fprintf(stream,
			"HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Basic realm=\"mygrok\"\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			len(body), body)
		logRequest(clientIP, method, path, 401, time.Since(start), nil)
		return false
	}

	local, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		body := fmt.Sprintf("mygrok: cannot connect to %s: %v\n", target, err)
		fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			len(body), body)
		logRequest(clientIP, method, path, 502, time.Since(start), err)
		return false
	}
	defer local.Close()

	if err := req.Write(local); err != nil {
		logRequest(clientIP, method, path, 0, time.Since(start), err)
		return false
	}

	dstBR := bufio.NewReader(local)
	resp, err := http.ReadResponse(dstBR, &http.Request{Method: method})
	if err != nil {
		logRequest(clientIP, method, path, 0, time.Since(start), err)
		return false
	}

	werr := resp.Write(stream)
	logRequest(clientIP, method, path, resp.StatusCode, time.Since(start), werr)
	if werr != nil {
		return false
	}

	if upgrade && resp.StatusCode == 101 {
		// After a 101 the connection is no longer HTTP — pump raw bytes
		// in both directions until either side closes its write end.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(local, srcBR)
			closeWrite(local)
		}()
		go func() {
			defer wg.Done()
			_, _ = io.Copy(stream, dstBR)
			closeWrite(stream)
		}()
		wg.Wait()
		return false
	}

	return true
}

// closeWrite half-closes the write side of c if the underlying type supports
// it (yamux streams and *net.TCPConn both do). Lets the peer see EOF without
// us tearing down the read side.
func closeWrite(c net.Conn) {
	if w, ok := c.(interface{ CloseWrite() error }); ok {
		_ = w.CloseWrite()
	}
}

func isUpgradeRequest(r *http.Request) bool {
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return r.Header.Get("Upgrade") != ""
}

// --- per-request logging ---------------------------------------------------

var (
	colorOnce    sync.Once
	colorEnabled bool
)

func useColor() bool {
	colorOnce.Do(func() {
		if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
			return
		}
		fi, err := os.Stderr.Stat()
		if err != nil {
			return
		}
		colorEnabled = fi.Mode()&os.ModeCharDevice != 0
	})
	return colorEnabled
}

func colorize(code int, s string) string {
	if !useColor() || code == 0 {
		return s
	}
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", code, s)
}

func statusColor(s int) int {
	switch {
	case s >= 500:
		return 31 // red
	case s >= 400:
		return 33 // yellow
	case s >= 300:
		return 36 // cyan
	case s >= 100:
		return 32 // green
	default:
		return 0
	}
}

func logRequest(clientIP, method, path string, status int, dur time.Duration, err error) {
	var statusStr string
	var col int
	if status == 0 {
		statusStr = "ERR"
		col = 31
		if err != nil {
			msg := strings.ReplaceAll(err.Error(), "\n", " ")
			msg = strings.ReplaceAll(msg, "\r", " ")
			statusStr += " " + truncate(msg, 60)
		}
	} else {
		text := http.StatusText(status)
		if text == "" {
			statusStr = strconv.Itoa(status)
		} else {
			statusStr = fmt.Sprintf("%d %s", status, text)
		}
		col = statusColor(status)
	}
	statusPadded := fmt.Sprintf("%-25s", statusStr)
	if clientIP == "" {
		clientIP = "-"
	}
	log.Printf("%-15s %-6s %s  %7s  %s",
		clientIP,
		method,
		colorize(col, statusPadded),
		formatDuration(dur),
		path,
	)
}

func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func readTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(home + "/.mygrok/authtoken")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

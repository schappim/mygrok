package main

// `mygrok admin` — manage server-side IP access control from the CLI.
//
// With no subcommand (or `login`), opens (or prints) a magic-link URL
// that signs into the admin web UI. With a subcommand, talks to the
// admin HTTP API directly using HTTP basic auth (password = the shared
// MYGROK_AUTHTOKEN). All admin web functions are mirrored as
// subcommands so an LLM (or a script) can drive the server without
// rendering HTML — see `mygrok admin help` for the full surface.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

func cmdAdmin(args []string) {
	// `mygrok admin` (bare) opens the login URL — backward compat.
	// `mygrok admin --open=false` (flag-only) → also login.
	// Anything else → subcommand dispatch.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		cmdAdminLogin(args)
		return
	}
	switch args[0] {
	case "login":
		cmdAdminLogin(args[1:])
	case "tunnels":
		cmdAdminTunnels(args[1:])
	case "rules":
		cmdAdminRules(args[1:])
	case "allow":
		cmdAdminMutate("allow", "add", args[1:])
	case "block":
		cmdAdminMutate("block", "add", args[1:])
	case "unallow":
		cmdAdminMutate("allow", "remove", args[1:])
	case "unblock":
		cmdAdminMutate("block", "remove", args[1:])
	case "lock":
		cmdAdminLockToggle("lock", args[1:])
	case "unlock":
		cmdAdminLockToggle("unlock", args[1:])
	case "grant":
		cmdAdminGrantRevoke("grant", args[1:])
	case "revoke":
		cmdAdminGrantRevoke("revoke", args[1:])
	case "passkeys":
		cmdAdminPasskeys(args[1:])
	case "users":
		cmdAdminUsers(args[1:])
	case "invite":
		cmdAdminInviteCreate(args[1:])
	case "invites":
		cmdAdminInvites(args[1:])
	case "help", "-h", "--help":
		fmt.Print(adminHelpText)
	default:
		fmt.Fprintf(os.Stderr, "unknown admin subcommand: %s\n\n", args[0])
		fmt.Fprint(os.Stderr, adminHelpText)
		os.Exit(2)
	}
}

// --- shared flag/env resolution -------------------------------------------

type adminEnv struct {
	server string
	auth   string
}

// resolveAdminEnv reads server + auth from CLI flags (parsed via fs),
// falling back to env vars / config / token file using the same
// precedence as `mygrok http`. Caller passes the parsed *flag.FlagSet
// values; we just centralise the default-resolution logic.
func defaultAdminServer(args []string) string {
	cfg, _, _ := loadConfig(scanFlag(args, "--config"))
	return resolveServer(cfg)
}

func defaultAdminAuth(args []string) string {
	cfg, _, _ := loadConfig(scanFlag(args, "--config"))
	def := ""
	if cfg != nil && cfg.Auth != "" {
		def = cfg.Auth
	}
	if v := os.Getenv("MYGROK_AUTHTOKEN"); v != "" {
		def = v
	}
	if def == "" {
		def = readTokenFile()
	}
	return def
}

// addCommonFlags wires --server / --auth / --config onto an existing
// FlagSet. The pointers are returned so the subcommand can inspect them
// after Parse.
func addCommonFlags(fs *flag.FlagSet, args []string) (*string, *string) {
	server := fs.String("server", defaultAdminServer(args), "tunnel server (host:port)")
	auth := fs.String("auth", defaultAdminAuth(args), "auth token")
	configFlag := fs.String("config", "", "explicit path to a .mygrok.toml file")
	_ = configFlag
	return server, auth
}

// --- login (browser) -------------------------------------------------------

func cmdAdminLogin(args []string) {
	fs := flag.NewFlagSet("admin login", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	openBrowser := fs.Bool("open", true, "auto-open the URL in a browser (set --open=false to just print)")
	fs.Parse(args)

	if *auth == "" {
		exitf("auth token required (MYGROK_AUTHTOKEN, --auth=, or ~/.mygrok/authtoken)")
	}
	host := requireServer(*server)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	u := fmt.Sprintf("https://%s/admin/ips?key=%s", host, url.QueryEscape(*auth))

	fmt.Println()
	fmt.Println("  mygrok admin")
	fmt.Println()
	fmt.Println("  Open this URL to log in (key is single-use; server swaps it for a session cookie):")
	fmt.Println()
	fmt.Printf("    %s\n\n", u)
	if *openBrowser {
		if err := openURL(u); err != nil {
			fmt.Printf("  (couldn't auto-open: %v)\n", err)
		}
	}
}

func openURL(u string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{u}
	case "linux":
		cmd, args = "xdg-open", []string{u}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", u}
	default:
		return fmt.Errorf("unsupported OS for auto-open: %s", runtime.GOOS)
	}
	return exec.Command(cmd, args...).Start()
}

// --- HTTP client used by the API-driven subcommands ------------------------

type adminClient struct {
	baseURL string
	auth    string
	httpc   *http.Client
}

// newAdminClient is the single choke point for every admin subcommand that
// talks to the server, so the "which server?" check lives here rather than
// being repeated at each of them.
func newAdminClient(server, auth string) *adminClient {
	if auth == "" {
		exitf("auth token required (MYGROK_AUTHTOKEN, --auth=, or ~/.mygrok/authtoken)")
	}
	host := requireServer(server)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return &adminClient{
		baseURL: "https://" + host,
		auth:    auth,
		httpc:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *adminClient) do(req *http.Request) (int, []byte, error) {
	req.SetBasicAuth("admin", c.auth)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

func (c *adminClient) getJSON(path string, into any) error {
	req, _ := http.NewRequest("GET", c.baseURL+path, nil)
	code, body, err := c.do(req)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("HTTP %d: %s", code, strings.TrimSpace(string(body)))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(body, into)
}

type apiMutateResp struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (c *adminClient) postForm(path string, form url.Values) (apiMutateResp, error) {
	req, _ := http.NewRequest("POST", c.baseURL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, body, err := c.do(req)
	if err != nil {
		return apiMutateResp{}, err
	}
	var r apiMutateResp
	if err := json.Unmarshal(body, &r); err != nil {
		return apiMutateResp{}, fmt.Errorf("parse server response: %v (raw: %s)", err, string(body))
	}
	return r, nil
}

// --- subcommands -----------------------------------------------------------

type apiState struct {
	PublicHost        string               `json:"public_host"`
	Global            apiBucket            `json:"global"`
	TunnelsConfigured map[string]apiBucket `json:"tunnels_configured"`
	TunnelsLive       []string             `json:"tunnels_live"`
}

type apiBucket struct {
	Allowed []apiEntry `json:"allowed"`
	Blocked []apiEntry `json:"blocked"`
}

type apiEntry struct {
	Raw  string `json:"raw"`
	Note string `json:"note,omitempty"`
}

type apiTunnelState struct {
	Scope   string     `json:"scope"`
	Live    bool       `json:"live"`
	Allowed []apiEntry `json:"allowed"`
	Blocked []apiEntry `json:"blocked"`
}

func cmdAdminTunnels(args []string) {
	fs := flag.NewFlagSet("admin tunnels", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	fs.Parse(args)

	client := newAdminClient(*server, *auth)
	var state apiState
	if err := client.getJSON("/admin/ips", &state); err != nil {
		exitf("admin tunnels: %v", err)
	}
	if *asJSON {
		out := struct {
			PublicHost string   `json:"public_host"`
			Live       []string `json:"live"`
			Configured []string `json:"configured"`
		}{
			PublicHost: state.PublicHost,
			Live:       state.TunnelsLive,
			Configured: keysOf(state.TunnelsConfigured),
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	urlFor := func(sub string) string {
		if state.PublicHost == "" {
			return sub
		}
		return "https://" + sub + "." + state.PublicHost
	}
	fmt.Println("LIVE:")
	if len(state.TunnelsLive) == 0 {
		fmt.Println("  (none)")
	}
	for _, s := range state.TunnelsLive {
		fmt.Printf("  %s\n", urlFor(s))
	}
	fmt.Println()
	fmt.Println("CONFIGURED (with rules):")
	configured := keysOf(state.TunnelsConfigured)
	if len(configured) == 0 {
		fmt.Println("  (none)")
	}
	for _, s := range configured {
		bk := state.TunnelsConfigured[s]
		fmt.Printf("  %-40s %d allow / %d block\n", urlFor(s), len(bk.Allowed), len(bk.Blocked))
	}
}

func cmdAdminRules(args []string) {
	fs := flag.NewFlagSet("admin rules", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional []string
	var flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)

	client := newAdminClient(*server, *auth)

	if len(positional) == 0 {
		var state apiState
		if err := client.getJSON("/admin/ips", &state); err != nil {
			exitf("admin rules: %v", err)
		}
		if *asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(state)
			return
		}
		printBucket("GLOBAL", state.Global)
		fmt.Println()
		for _, sub := range keysOf(state.TunnelsConfigured) {
			printBucket(sub, state.TunnelsConfigured[sub])
			fmt.Println()
		}
		if len(state.TunnelsConfigured) == 0 {
			fmt.Println("(no per-tunnel rules)")
		}
		return
	}
	scope := positional[0]
	if scope == "global" {
		var state apiState
		if err := client.getJSON("/admin/ips", &state); err != nil {
			exitf("admin rules: %v", err)
		}
		if *asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(state.Global)
			return
		}
		printBucket("GLOBAL", state.Global)
		return
	}
	if !looksLikeSubdomain(scope) {
		fmt.Fprintf(os.Stderr, "invalid scope %q (use \"global\" or a subdomain)\n", scope)
		os.Exit(2)
	}
	var t apiTunnelState
	if err := client.getJSON("/admin/ips/"+scope, &t); err != nil {
		exitf("admin rules %s: %v", scope, err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(t)
		return
	}
	tag := scope
	if t.Live {
		tag += "  (live)"
	} else {
		tag += "  (offline)"
	}
	printBucket(tag, apiBucket{Allowed: t.Allowed, Blocked: t.Blocked})
}

func cmdAdminMutate(list, op string, args []string) {
	verb := list
	if op == "remove" {
		verb = "un" + list
	}
	fs := flag.NewFlagSet("admin "+verb, flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	note := fs.String("note", "", "optional note (only used on add)")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional []string
	var flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)

	if len(positional) != 2 {
		fmt.Fprintf(os.Stderr, "usage: mygrok admin %s <scope> <ip-or-cidr> [--note=<text>]\n  scope = \"global\" or a subdomain\n  ip-or-cidr = single IP (1.2.3.4) or CIDR (10.0.0.0/8)\n", verb)
		os.Exit(2)
	}
	scope := positional[0]
	value := positional[1]
	if scope != "global" && !looksLikeSubdomain(scope) {
		fmt.Fprintf(os.Stderr, "invalid scope %q (use \"global\" or a subdomain)\n", scope)
		os.Exit(2)
	}

	path := "/admin/ips"
	if scope != "global" {
		path = "/admin/ips/" + scope
	}
	form := url.Values{}
	form.Set("list", list)
	form.Set("op", op)
	form.Set("value", value)
	if op == "add" && *note != "" {
		form.Set("note", *note)
	}

	client := newAdminClient(*server, *auth)
	resp, err := client.postForm(path, form)
	if err != nil {
		exitf("admin %s: %v", verb, err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		if !resp.OK {
			os.Exit(3)
		}
		return
	}
	if resp.OK {
		if resp.Message != "" {
			fmt.Println(resp.Message)
		}
		return
	}
	fmt.Fprintln(os.Stderr, "error:", resp.Error)
	os.Exit(3)
}

// --- lock / unlock --------------------------------------------------------

func cmdAdminLockToggle(op string, args []string) {
	fs := flag.NewFlagSet("admin "+op, flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional []string
	var flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)

	if len(positional) != 1 {
		fmt.Fprintf(os.Stderr, "usage: mygrok admin %s <subdomain>\n", op)
		os.Exit(2)
	}
	sub := positional[0]
	if !looksLikeSubdomain(sub) {
		fmt.Fprintf(os.Stderr, "invalid subdomain %q\n", sub)
		os.Exit(2)
	}

	form := url.Values{}
	form.Set("sub", sub)
	form.Set("op", op)
	client := newAdminClient(*server, *auth)
	// /admin/locks redirects on success; treat 302 as ok.
	req, _ := http.NewRequest("POST", client.baseURL+"/admin/locks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", client.auth)
	// Don't auto-follow — we just want the status.
	noRedirect := &http.Client{
		Timeout: client.httpc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		exitf("admin %s: %v", op, err)
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	msg := fmt.Sprintf("%sed %s", op, sub)
	if !ok {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			OK      bool   `json:"ok"`
			Message string `json:"message,omitempty"`
			Error   string `json:"error,omitempty"`
		}{ok, msg, ""})
		if !ok {
			os.Exit(3)
		}
		return
	}
	if ok {
		fmt.Println(msg)
	} else {
		fmt.Fprintln(os.Stderr, "error:", msg)
		os.Exit(3)
	}
}

// --- passkeys -------------------------------------------------------------

type apiPasskey struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Created    string `json:"created"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

func cmdAdminPasskeys(args []string) {
	if len(args) == 0 {
		cmdAdminPasskeysList(nil)
		return
	}
	switch args[0] {
	case "list":
		cmdAdminPasskeysList(args[1:])
	case "delete", "remove", "rm":
		cmdAdminPasskeysDelete(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "usage: mygrok admin passkeys [list|delete <id>]\n")
		os.Exit(2)
	}
}

func cmdAdminPasskeysList(args []string) {
	fs := flag.NewFlagSet("admin passkeys list", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	fs.Parse(args)

	client := newAdminClient(*server, *auth)
	// Reuse /admin/passkeys with Accept: application/json — we don't
	// have a dedicated JSON read endpoint yet, so for now expose only
	// what we can synthesize from the passkey-store admin file via a
	// fallback: parse the rendered HTML for credential rows. To keep
	// things clean, add a simple JSON read endpoint server-side. (See
	// passkeys list endpoint.)
	var creds []apiPasskey
	if err := client.getJSON("/admin/passkeys?format=json", &creds); err != nil {
		exitf("admin passkeys list: %v", err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(creds)
		return
	}
	if len(creds) == 0 {
		fmt.Println("(no passkeys registered — open https://tunnel.<host>/admin/passkeys in a browser to add one)")
		return
	}
	fmt.Printf("%-20s %-22s %-22s %s\n", "LABEL", "ID", "CREATED", "LAST USED")
	for _, c := range creds {
		last := c.LastUsedAt
		if last == "" {
			last = "(never)"
		}
		fmt.Printf("%-20s %-22s %-22s %s\n", c.Label, shortenID(c.ID), c.Created, last)
	}
}

func cmdAdminPasskeysDelete(args []string) {
	fs := flag.NewFlagSet("admin passkeys delete", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional, flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)

	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mygrok admin passkeys delete <id>")
		os.Exit(2)
	}
	id := positional[0]

	form := url.Values{}
	form.Set("id", id)
	client := newAdminClient(*server, *auth)
	req, _ := http.NewRequest("POST", client.baseURL+"/admin/passkeys/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("admin", client.auth)
	noRedirect := &http.Client{
		Timeout: client.httpc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		exitf("admin passkeys delete: %v", err)
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	msg := "deleted " + id
	if !ok {
		msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			OK      bool   `json:"ok"`
			Message string `json:"message,omitempty"`
		}{ok, msg})
		if !ok {
			os.Exit(3)
		}
		return
	}
	if ok {
		fmt.Println(msg)
	} else {
		fmt.Fprintln(os.Stderr, "error:", msg)
		os.Exit(3)
	}
}

func shortenID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return id[:8] + "…" + id[len(id)-8:]
}

// --- invite create / list / revoke ----------------------------------------

type apiInvite struct {
	Token      string `json:"token"`
	URL        string `json:"url"`
	Name       string `json:"name"`
	UserID     string `json:"user_id,omitempty"`
	Created    string `json:"created"`
	Expires    string `json:"expires"`
	Consumed   bool   `json:"consumed"`
	ConsumedAt string `json:"consumed_at,omitempty"`
}

func cmdAdminInviteCreate(args []string) {
	fs := flag.NewFlagSet("admin invite", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional, flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)

	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mygrok admin invite <name>")
		os.Exit(2)
	}
	name := positional[0]

	form := url.Values{}
	form.Set("name", name)
	client := newAdminClient(*server, *auth)
	req, _ := http.NewRequest("POST", client.baseURL+"/admin/invites/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth("admin", client.auth)
	resp, err := client.httpc.Do(req)
	if err != nil {
		exitf("admin invite: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		OK      bool   `json:"ok"`
		Token   string `json:"token"`
		URL     string `json:"url"`
		Name    string `json:"name"`
		Expires string `json:"expires"`
		Error   string `json:"error,omitempty"`
	}
	_ = json.Unmarshal(body, &r)
	if resp.StatusCode != 200 || !r.OK {
		if r.Error == "" {
			r.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		if *asJSON {
			fmt.Println(string(body))
		} else {
			fmt.Fprintln(os.Stderr, "error:", r.Error)
		}
		os.Exit(3)
	}
	if *asJSON {
		fmt.Println(string(body))
		return
	}
	fmt.Println()
	fmt.Println("  invite created")
	fmt.Println()
	fmt.Printf("  for:     %s\n", r.Name)
	fmt.Printf("  expires: %s\n", r.Expires)
	fmt.Printf("  url:     %s\n", r.URL)
	fmt.Println()
	fmt.Println("  Send this URL to the invitee. The link is one-shot — once they")
	fmt.Println("  register a passkey, the user is created and the link stops working.")
}

func cmdAdminInvites(args []string) {
	if len(args) > 0 && args[0] == "revoke" {
		cmdAdminInviteRevoke(args[1:])
		return
	}
	cmdAdminInvitesList(args)
}

func cmdAdminInvitesList(args []string) {
	fs := flag.NewFlagSet("admin invites", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	fs.Parse(args)

	client := newAdminClient(*server, *auth)
	var list []apiInvite
	if err := client.getJSON("/admin/invites", &list); err != nil {
		exitf("admin invites: %v", err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(list)
		return
	}
	if len(list) == 0 {
		fmt.Println("(no invites — issue one with: mygrok admin invite <name>)")
		return
	}
	fmt.Printf("%-18s %-8s %-22s %s\n", "NAME", "STATUS", "EXPIRES", "URL")
	for _, r := range list {
		status := "pending"
		if r.Consumed {
			status = "used"
		}
		fmt.Printf("%-18s %-8s %-22s %s\n", r.Name, status, r.Expires, r.URL)
	}
}

func cmdAdminInviteRevoke(args []string) {
	fs := flag.NewFlagSet("admin invites revoke", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional, flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mygrok admin invites revoke <token>")
		os.Exit(2)
	}
	token := positional[0]

	form := url.Values{}
	form.Set("op", "revoke")
	form.Set("token", token)
	client := newAdminClient(*server, *auth)
	resp, err := client.postForm("/admin/invites", form)
	if err != nil {
		exitf("admin invites revoke: %v", err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		if !resp.OK {
			os.Exit(3)
		}
		return
	}
	if resp.OK {
		fmt.Println(resp.Message)
		return
	}
	fmt.Fprintln(os.Stderr, "error:", resp.Error)
	os.Exit(3)
}

// --- users -----------------------------------------------------------------

type apiUser struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Created     string   `json:"created"`
	Credentials int      `json:"credentials"`
	Tunnels     []string `json:"tunnels"`
}

func cmdAdminUsers(args []string) {
	if len(args) > 0 && (args[0] == "delete" || args[0] == "remove" || args[0] == "rm") {
		cmdAdminUsersDelete(args[1:])
		return
	}
	cmdAdminUsersList(args)
}

func cmdAdminUsersList(args []string) {
	fs := flag.NewFlagSet("admin users", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	fs.Parse(args)

	client := newAdminClient(*server, *auth)
	var list []apiUser
	if err := client.getJSON("/admin/users", &list); err != nil {
		exitf("admin users: %v", err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(list)
		return
	}
	if len(list) == 0 {
		fmt.Println("(no users — issue an invite with: mygrok admin invite <name>)")
		return
	}
	fmt.Printf("%-18s %-22s %-7s %s\n", "NAME", "ID", "PASSKEYS", "TUNNELS")
	for _, u := range list {
		t := strings.Join(u.Tunnels, ",")
		if t == "" {
			t = "-"
		}
		fmt.Printf("%-18s %-22s %-7d %s\n", u.Name, shortenID(u.ID), u.Credentials, t)
	}
}

func cmdAdminUsersDelete(args []string) {
	fs := flag.NewFlagSet("admin users delete", flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional, flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)
	if len(positional) != 1 {
		fmt.Fprintln(os.Stderr, "usage: mygrok admin users delete <user-id-or-name>")
		os.Exit(2)
	}
	uid := positional[0]
	uid = resolveUserID(*server, *auth, uid)

	form := url.Values{}
	form.Set("user_id", uid)
	client := newAdminClient(*server, *auth)
	req, _ := http.NewRequest("POST", client.baseURL+"/admin/users/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth("admin", client.auth)
	resp, err := client.httpc.Do(req)
	if err != nil {
		exitf("admin users delete: %v", err)
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 400
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			OK      bool   `json:"ok"`
			Message string `json:"message,omitempty"`
		}{ok, "deleted " + uid})
		if !ok {
			os.Exit(3)
		}
		return
	}
	if ok {
		fmt.Println("deleted", uid)
		return
	}
	fmt.Fprintf(os.Stderr, "error: HTTP %d\n", resp.StatusCode)
	os.Exit(3)
}

// resolveUserID accepts either a raw user-id (base64) or a name; in
// the second case it queries /admin/users to find the matching user.
func resolveUserID(server, auth, ident string) string {
	if strings.Contains(ident, "/") || len(ident) > 32 {
		return ident
	}
	client := newAdminClient(server, auth)
	var list []apiUser
	if err := client.getJSON("/admin/users", &list); err == nil {
		for _, u := range list {
			if strings.EqualFold(u.Name, ident) || u.ID == ident {
				return u.ID
			}
		}
	}
	return ident
}

// --- grant / revoke -------------------------------------------------------

func cmdAdminGrantRevoke(op string, args []string) {
	fs := flag.NewFlagSet("admin "+op, flag.ExitOnError)
	server, auth := addCommonFlags(fs, args)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")

	var positional, flagArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
		} else {
			positional = append(positional, a)
		}
	}
	fs.Parse(flagArgs)
	if len(positional) != 2 {
		fmt.Fprintf(os.Stderr, "usage: mygrok admin %s <subdomain> <user-id-or-name>\n", op)
		os.Exit(2)
	}
	sub := positional[0]
	uid := resolveUserID(*server, *auth, positional[1])

	form := url.Values{}
	form.Set("sub", sub)
	form.Set("user_id", uid)
	form.Set("op", op)
	client := newAdminClient(*server, *auth)
	resp, err := client.postForm("/admin/grants", form)
	if err != nil {
		exitf("admin %s: %v", op, err)
	}
	if *asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(resp)
		if !resp.OK {
			os.Exit(3)
		}
		return
	}
	if resp.OK {
		fmt.Println(resp.Message)
		return
	}
	fmt.Fprintln(os.Stderr, "error:", resp.Error)
	os.Exit(3)
}

// --- helpers ---------------------------------------------------------------

func keysOf(m map[string]apiBucket) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(a []string) {
	// Tiny insertion sort, no dep on sort. Lists are short.
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

func printBucket(label string, b apiBucket) {
	fmt.Printf("== %s ==\n", label)
	fmt.Printf("  allow:\n")
	if len(b.Allowed) == 0 {
		fmt.Println("    (empty)")
	}
	for _, e := range b.Allowed {
		printEntry("    ", e)
	}
	fmt.Printf("  block:\n")
	if len(b.Blocked) == 0 {
		fmt.Println("    (empty)")
	}
	for _, e := range b.Blocked {
		printEntry("    ", e)
	}
}

func printEntry(indent string, e apiEntry) {
	if e.Note != "" {
		fmt.Printf("%s%-20s %s\n", indent, e.Raw, e.Note)
		return
	}
	fmt.Printf("%s%s\n", indent, e.Raw)
}

// --- help text -------------------------------------------------------------

const adminHelpText = `mygrok admin — manage server-side IP access control.

Without a subcommand (or with "login"), opens / prints a magic-link URL
that signs into the admin web UI. With a subcommand, talks to the admin
HTTP API directly so scripts and LLMs can drive the server without
rendering HTML.

USAGE
  mygrok admin                          open the admin web UI
  mygrok admin login [--open=false]     print + (optionally) open the URL
  mygrok admin tunnels [--json]         list registered + configured tunnels
  mygrok admin rules [<scope>] [--json] show rules (global + all tunnels, or one)
  mygrok admin allow   <scope> <ip|cidr> [--note=<t>] add to allow list
  mygrok admin block   <scope> <ip|cidr> [--note=<t>] add to block list
  mygrok admin unallow <scope> <ip|cidr>              remove from allow list
  mygrok admin unblock <scope> <ip|cidr>              remove from block list
  mygrok admin invite  <name>                          create a single-use passkey-registration URL
  mygrok admin invites [revoke <token>] [--json]      list pending/used invites (or revoke one)
  mygrok admin users   [delete <id-or-name>] [--json] list users (or delete one)
  mygrok admin grant   <subdomain> <id-or-name>       allow this user on a tunnel (locks it if needed)
  mygrok admin revoke  <subdomain> <id-or-name>       remove a user from a tunnel
  mygrok admin lock    <subdomain>                    grant every existing user (legacy convenience)
  mygrok admin unlock  <subdomain>                    drop all grants on a tunnel
  mygrok admin passkeys [list] [--json]               list registered credentials (across all users)
  mygrok admin passkeys delete <id>                   revoke a single credential
  mygrok admin help                     this help

  <scope>     = "global" or a subdomain (lowercase letters, digits, hyphens).
  <ip|cidr>   = single IP (1.2.3.4 or 2001:db8::1) or CIDR (10.0.0.0/8).
  --note=<t>  = optional human label, only saved on "add".

GLOBAL FLAGS  (accepted by every subcommand)
  --server=<host:port>   tunnel server (default: MYGROK_SERVER, the "server" key
                         in .mygrok.toml, or whatever this build was stamped with)
  --auth=<token>         auth token (also resolves from MYGROK_AUTHTOKEN
                         env var or ~/.mygrok/authtoken file)
  --config=<path>        explicit .mygrok.toml file
  --json                 machine-readable JSON output (where supported)

SEMANTICS
  Default with empty lists is OPEN — every tunnel accepts every visitor.
  The check on each public request fires in this order:
    1. Global blocklist hit              → 403
    2. Per-tunnel blocklist hit          → 403
    3. Per-tunnel allowlist non-empty
       AND IP not in it                  → 403
    4. Per-tunnel allowlist empty AND
       global allowlist non-empty AND
       IP not in it                      → 403
    5. otherwise                         → allow

EXAMPLES
  # See what's live and what has rules
  mygrok admin tunnels

  # Show every rule (global + all per-tunnel)
  mygrok admin rules

  # Just one tunnel
  mygrok admin rules jarvis

  # Lock down a tunnel to a single IP
  mygrok admin allow jarvis 203.0.113.7 --note=office

  # Lock down a tunnel to a whole CIDR
  mygrok admin allow jarvis 10.0.0.0/8

  # Globally block a scanner range
  mygrok admin block global 198.51.100.0/24 --note=scanner

  # Remove a rule
  mygrok admin unblock global 198.51.100.0/24
  mygrok admin unallow jarvis 203.0.113.7

  # Pipe to jq
  mygrok admin rules --json | jq '.tunnels_configured'

  # End-to-end: invite Alice, grant her access to jarvis
  mygrok admin invite alice
  # → prints https://tunnel.<host>/invite/<token>; send to Alice.
  # Alice opens it, registers a passkey in her browser. A "alice" user
  # is created and her credential is bound to it. Then:
  mygrok admin grant jarvis alice

  # See who exists / who can reach what
  mygrok admin users
  mygrok admin invites

  # Take Alice off jarvis (or remove her entirely)
  mygrok admin revoke jarvis alice
  mygrok admin users delete alice     # purges all her credentials too

PASSKEY GATE
  Each tunnel can have an "allowed_users" list. When non-empty, only
  those users' credentials open the tunnel. Visitors without a valid
  mygrok_pk session cookie (or with one tagged for a non-listed user)
  are redirected to https://tunnel.<host>/auth?return=<original-url>,
  where they complete a WebAuthn discoverable-credential assertion. On
  success the server issues a 24 h user-tagged session cookie scoped
  to .<public_host>, then redirects back to the return URL. The cookie
  is HttpOnly + Secure + SameSite=Lax. The gate is auto-bypassed when
  zero credentials are registered anywhere, so a misclicked "lock" or
  early "grant" can't strand you before any passkey exists.

ONBOARDING
  Registration itself can't be driven from the CLI — WebAuthn needs a
  browser + authenticator. The supported pattern is:
    1. Admin issues an invite tagged with the user's name:
         mygrok admin invite alice
    2. Admin sends the printed URL to the invitee.
    3. Invitee opens it; their browser prompts for a passkey.
    4. The user is created, their first credential is registered, and
       a session cookie is issued so they can immediately reach any
       tunnel they've been granted access to.

EXIT CODES
  0    success
  2    bad arguments / unknown subcommand
  3    server returned a non-OK response (validation or auth failure)
  >0   network / unexpected error

JSON OUTPUT SCHEMAS
  rules (no scope):
    {
      "public_host":        "<host>",
      "global":             { "allowed": [Entry], "blocked": [Entry] },
      "tunnels_configured": { "<sub>": { "allowed": [Entry], "blocked": [Entry] } },
      "tunnels_live":       ["<sub>", ...]
    }
  rules <scope=global>: same as the "global" object above.
  rules <scope=subdomain>:
    { "scope": "<sub>", "live": <bool>,
      "allowed": [Entry], "blocked": [Entry] }
  tunnels:
    { "public_host": "<host>", "live": ["<sub>", ...], "configured": ["<sub>", ...] }
    Each <sub> reaches the tunnel at https://<sub>.<public_host>.
  rules <scope=subdomain> (extended fields):
    The per-tunnel object also includes:
      "require_passkey": <bool>,
      "allowed_users":   [{ "id": "<user-id>", "name": "<name>" }, ...]
  passkeys list:
    [ { "id": "<hex>", "user_id": "<user-id>", "label": "<text>",
        "created": "<RFC3339>", "last_used_at": "<RFC3339>" }, ... ]
  users:
    [ { "id": "<user-id>", "name": "<text>", "created": "<RFC3339>",
        "credentials": <int>, "tunnels": ["<sub>", ...] }, ... ]
  invites:
    [ { "token": "<hex>", "url": "https://tunnel.<host>/invite/<token>",
        "name": "<text>", "user_id": "<user-id-after-redeem>",
        "created": "<RFC3339>", "expires": "<RFC3339>",
        "consumed": <bool>, "consumed_at": "<RFC3339>" }, ... ]
  invite (create) success:
    { "ok": true, "token": "<hex>", "url": "...",
      "name": "<text>", "expires": "<RFC3339>" }
  grant / revoke / lock / unlock / users delete / passkeys delete:
    { "ok": true,  "message": "..." } / { "ok": false, "message": "HTTP NNN" }
  allow / block / unallow / unblock:
    { "ok": true,  "message": "added 1.2.3.4 to jarvis/allow" }     on success
    { "ok": false, "error":   "invalid IP \"foo\"" }                 on failure

  Entry:
    { "raw": "<ip-or-cidr>", "note": "<optional text>" }

ENVIRONMENT
  MYGROK_AUTHTOKEN   shared auth token (also doubles as admin password)
  MYGROK_SERVER      default server host:port

NOTES
  - Admin requests authenticate via HTTP basic auth (any username, password
    = MYGROK_AUTHTOKEN). The web UI also supports magic-link login that
    swaps the token for a session cookie; the CLI uses basic auth directly.
  - The admin host (tunnel.<publicHost>) itself is exempt from the IP ACL,
    so you cannot lock yourself out with a misconfigured rule.
  - Per-tunnel rules apply to a tunnel's subdomain, regardless of whether
    requests arrive at <sub>.<publicHost> or via a registered custom
    hostname (CNAME).
`

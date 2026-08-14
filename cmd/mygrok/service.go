package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Service install/uninstall/list. Wraps `mygrok http <port> --subdomain=<sub>`
// in a launchd LaunchAgent (macOS) or systemd unit (Linux) so tunnels survive
// reboots.

func cmdService(args []string) {
	if len(args) == 0 {
		serviceUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "install":
		serviceInstall(args[1:])
	case "uninstall", "remove":
		serviceUninstall(args[1:])
	case "list", "ls":
		serviceList()
	case "logs":
		serviceLogs(args[1:])
	case "status":
		serviceStatus(args[1:])
	default:
		serviceUsage()
		os.Exit(2)
	}
}

func serviceUsage() {
	fmt.Fprintln(os.Stderr, `mygrok service — install tunnels as auto-starting OS services

Usage:
  mygrok service install <port> --subdomain=<name> [--name=<svc-name>] [--mcp [--secret=<token>] [--no-strip] [--path=/mcp]]
  mygrok service uninstall <name>
  mygrok service list
  mygrok service status <name>
  mygrok service logs <name>

Examples:
  mygrok service install 3000 --subdomain=jarvis
  mygrok service install 8080 --subdomain=app --name=app-prod
  mygrok service install 8790 --subdomain=tools --mcp   # persistent claude.ai MCP connector
  mygrok service uninstall jarvis

Notes:
  • macOS: writes a LaunchAgent to ~/Library/LaunchAgents (per-user, runs at login).
  • Linux: writes a system unit to /etc/systemd/system (uses sudo).
  • Secrets stay out of the unit file and out of argv: on Linux the auth token
    and any --basic-auth go in /etc/mygrok/<name>.env (0600, root-only); on
    macOS they go in the 0600 plist's environment.`)
}

type svcSpec struct {
	Name       string // service name (default = subdomain)
	Subdomain  string
	Port       string
	Server     string
	Token      string
	Binary     string // absolute path to the mygrok binary
	LocalHost  string
	BasicAuth  string // optional --basic-auth=user:pass passthrough
	Hostnames  string // optional --hostname=a.com,b.com passthrough (comma-separated)
	LAN        string // optional --lan=auto / --lan=192.168.x.y passthrough
	LANPort    int    // optional --lan-port=<n> passthrough (only if LAN != "")
	MCP        bool   // run `mygrok mcp` instead of `mygrok http`
	MCPSecret  string // resolved capability token (printed, not baked into the unit)
	MCPNoStrip bool   // --no-strip passthrough
	MCPPath    string // --path passthrough (connector URL suffix)
}

// subcommand returns the mygrok subcommand the service unit should run.
func (s svcSpec) subcommand() string {
	if s.MCP {
		return "mcp"
	}
	return "http"
}

// launchdLabelPrefix namespaces the LaunchAgents this tool writes.
//
// legacyLaunchdLabelPrefix is the pre-1.0 prefix. Nothing writes it any
// more, but list/status/uninstall still recognise it so an existing install
// doesn't become invisible to the CLI that manages it.
const (
	launchdLabelPrefix       = "dev.mygrok.agent."
	legacyLaunchdLabelPrefix = "cloud.schappi.mygrok."
)

// launchdLabelPrefixes lists every prefix a service on disk might carry,
// current one first.
var launchdLabelPrefixes = []string{launchdLabelPrefix, legacyLaunchdLabelPrefix}

func serviceInstall(args []string) {
	// Same precedence as `mygrok http`: CLI > env > config > default.
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

	fs := flag.NewFlagSet("service install", flag.ExitOnError)
	subdomain := fs.String("subdomain", defSub, "public subdomain (required)")
	name := fs.String("name", "", "service name (default: same as --subdomain)")
	server := fs.String("server", defServer, "tunnel server")
	auth := fs.String("auth", defAuth, "auth token")
	host := fs.String("host", defHost, "local host to forward to")
	basicAuth := fs.String("basic-auth", "", `protect the tunnel with HTTP basic auth, e.g. "alice:s3cret"`)
	hostnames := fs.String("hostname", "", "additional public hostname (CNAME) to claim, e.g. app.example.com (comma-separate for multiple)")
	lanFlag := fs.String("lan", "", `enable LAN-direct: "auto" or an explicit RFC1918 IPv4`)
	lanPort := fs.Int("lan-port", 8443, "port for the LAN-direct TLS listener (only used with --lan)")
	mcpFlag := fs.Bool("mcp", false, "run in MCP connector mode (capability-URL gate, see `mygrok mcp`)")
	mcpSecretFlag := fs.String("secret", "", "MCP capability token (default: load or generate ~/.mygrok/mcp/<subdomain>.secret)")
	mcpNoStrip := fs.Bool("no-strip", false, "MCP mode: forward the path without removing the token segment")
	mcpPath := fs.String("path", "/mcp", "MCP mode: path suffix shown in the connector URL")
	configFlag := fs.String("config", "", "explicit path to a .mygrok.toml file")
	_ = configFlag

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
		serviceUsage()
		os.Exit(2)
	}
	if *subdomain == "" {
		exitf("--subdomain is required (pass on CLI or set subdomain = ... in .mygrok.toml)")
	}
	if *auth == "" {
		exitf("auth token required (MYGROK_AUTHTOKEN, --auth, ~/.mygrok/authtoken, or auth = ... in .mygrok.toml)")
	}
	if cfgFrom != "" {
		fmt.Fprintf(os.Stderr, "(config: %s)\n", cfgFrom)
	}
	svcName := *name
	if svcName == "" {
		svcName = *subdomain
	}
	if !validServiceName(svcName) {
		exitf("invalid name %q (use lowercase letters, digits, dashes)", svcName)
	}

	exe, err := os.Executable()
	if err != nil {
		exitf("cannot locate mygrok binary: %v", err)
	}
	exe, _ = filepath.Abs(exe)

	if *basicAuth != "" {
		// Validate now so the user finds out before we write the unit
		// file. The agent will re-parse at runtime.
		if _, err := parseBasicAuthFlag(*basicAuth); err != nil {
			exitf("%v", err)
		}
		if strings.ContainsAny(*basicAuth, " \t\n\r") {
			exitf(`--basic-auth must not contain whitespace`)
		}
	}

	resolvedLAN, err := resolveLANFlag(*lanFlag)
	if err != nil {
		exitf("%v", err)
	}

	// MCP mode: resolve (and persist) the capability token now, so the
	// connector URL is printable here and stable across restarts. The
	// running agent re-loads it from ~/.mygrok/mcp/<subdomain>.secret —
	// the token is never baked into the unit file.
	resolvedSecret := ""
	if *mcpFlag {
		resolvedSecret, err = resolveMCPSecret(mcpSecretDir(), *subdomain, *mcpSecretFlag)
		if err != nil {
			exitf("%v", err)
		}
	} else if *mcpSecretFlag != "" || *mcpNoStrip {
		exitf("--secret/--no-strip only make sense with --mcp")
	}

	// Check this before writing any unit file. A service installed with no
	// server address would enable cleanly and then crash-loop forever, which
	// is a much worse way to find out than an error here.
	resolvedServer := requireServer(*server)

	spec := svcSpec{
		Name:       svcName,
		Subdomain:  *subdomain,
		Port:       port,
		Server:     resolvedServer,
		Token:      *auth,
		Binary:     exe,
		LocalHost:  *host,
		BasicAuth:  *basicAuth,
		Hostnames:  *hostnames,
		LAN:        resolvedLAN,
		LANPort:    *lanPort,
		MCP:        *mcpFlag,
		MCPSecret:  resolvedSecret,
		MCPNoStrip: *mcpNoStrip,
		MCPPath:    *mcpPath,
	}

	switch runtime.GOOS {
	case "darwin":
		installLaunchd(spec)
	case "linux":
		installSystemd(spec)
	default:
		exitf("service install not supported on %s", runtime.GOOS)
	}
}

func serviceUninstall(args []string) {
	if len(args) < 1 {
		exitf("usage: mygrok service uninstall <name>")
	}
	name := args[0]
	switch runtime.GOOS {
	case "darwin":
		uninstallLaunchd(name)
	case "linux":
		uninstallSystemd(name)
	default:
		exitf("service uninstall not supported on %s", runtime.GOOS)
	}
}

func serviceList() {
	switch runtime.GOOS {
	case "darwin":
		listLaunchd()
	case "linux":
		listSystemd()
	default:
		exitf("service list not supported on %s", runtime.GOOS)
	}
}

func serviceStatus(args []string) {
	if len(args) < 1 {
		exitf("usage: mygrok service status <name>")
	}
	name := args[0]
	switch runtime.GOOS {
	case "darwin":
		_, label, ok := findLaunchdPlist(name)
		if !ok {
			exitf("no such service: %s", name)
		}
		run("launchctl", "list", label)
	case "linux":
		run("systemctl", "status", "mygrok-"+name+".service", "--no-pager")
	}
}

func serviceLogs(args []string) {
	if len(args) < 1 {
		exitf("usage: mygrok service logs <name>")
	}
	name := args[0]
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(homeDir(), "Library/Logs/mygrok-"+name+".log")
		fmt.Println("==>", path)
		run("tail", "-n", "200", "-f", path)
	case "linux":
		run("journalctl", "-u", "mygrok-"+name+".service", "-n", "200", "-f")
	}
}

// --- launchd (macOS) -------------------------------------------------------

const launchdPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>            <string>%[1]s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%[2]s</string>
    <string>%[11]s</string>
    <string>%[3]s</string>
    <string>--subdomain=%[4]s</string>
    <string>--server=%[5]s</string>
    <string>--host=%[9]s</string>%[10]s
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>MYGROK_AUTHTOKEN</key><string>%[6]s</string>%[12]s
  </dict>
  <key>RunAtLoad</key>        <true/>
  <key>KeepAlive</key>        <true/>
  <key>StandardOutPath</key>  <string>%[7]s</string>
  <key>StandardErrorPath</key><string>%[7]s</string>
  <key>WorkingDirectory</key> <string>%[8]s</string>
</dict>
</plist>
`

func launchdPlistPath(name string) string {
	return filepath.Join(homeDir(), "Library/LaunchAgents", launchdLabelPrefix+name+".plist")
}

// findLaunchdPlist locates an installed agent by service name, checking the
// current label prefix first and then the legacy one. Returns the plist
// path and the launchd label it actually carries.
func findLaunchdPlist(name string) (path, label string, ok bool) {
	dir := filepath.Join(homeDir(), "Library/LaunchAgents")
	for _, prefix := range launchdLabelPrefixes {
		p := filepath.Join(dir, prefix+name+".plist")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, prefix + name, true
		}
	}
	return "", "", false
}

func installLaunchd(s svcSpec) {
	label := launchdLabelPrefix + s.Name
	plist := launchdPlistPath(s.Name)
	logPath := filepath.Join(homeDir(), "Library/Logs", "mygrok-"+s.Name+".log")
	wd := homeDir()

	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		exitf("mkdir LaunchAgents: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		exitf("mkdir Logs: %v", err)
	}

	extraArgs := ""
	// Deliberately NOT an argv entry: argv is world-readable via ps. The
	// plist itself is 0600, so the environment is the safe place for it.
	extraEnv := ""
	if s.BasicAuth != "" {
		extraEnv += "\n    <key>MYGROK_BASIC_AUTH</key><string>" + escapePlist(s.BasicAuth) + "</string>"
	}
	if s.Hostnames != "" {
		extraArgs += "\n    <string>--hostname=" + escapePlist(s.Hostnames) + "</string>"
	}
	if s.LAN != "" {
		extraArgs += "\n    <string>--lan=" + escapePlist(s.LAN) + "</string>"
		if s.LANPort != 0 {
			extraArgs += fmt.Sprintf("\n    <string>--lan-port=%d</string>", s.LANPort)
		}
	}
	if s.MCP {
		if s.MCPNoStrip {
			extraArgs += "\n    <string>--no-strip</string>"
		}
		if s.MCPPath != "" && s.MCPPath != "/mcp" {
			extraArgs += "\n    <string>--path=" + escapePlist(s.MCPPath) + "</string>"
		}
	}
	body := fmt.Sprintf(launchdPlistTemplate,
		label, s.Binary, s.Port, s.Subdomain, s.Server, escapePlist(s.Token), logPath, wd, s.LocalHost, extraArgs, s.subcommand(), extraEnv)
	if err := os.WriteFile(plist, []byte(body), 0o600); err != nil {
		exitf("write plist: %v", err)
	}

	// Best-effort: unload any existing instance under ANY known label —
	// including the pre-1.0 prefix. Missing the legacy one would leave two
	// agents fighting over the same subdomain, each pre-empting the other's
	// session on every reconnect.
	unloadAllLaunchd(s.Name)
	if err := exec.Command("launchctl", "load", "-w", plist).Run(); err != nil {
		exitf("launchctl load: %v", err)
	}

	fmt.Printf("Installed %s\n", label)
	fmt.Printf("  Plist: %s\n", plist)
	fmt.Printf("  Logs:  %s\n", logPath)
	fmt.Printf("  Public: %s  (active at login + on reboot)\n", s.publicURL())
	printMCPConnectorURL(s)
}

// publicURL is the https URL this service's tunnel is expected to appear
// at, derived from the configured server address. The tunnel server has the
// final say (it prints the authoritative URLs once connected); this is what
// we can say at install time, before any connection exists.
func (s svcSpec) publicURL() string {
	zone := publicHostFor(s.Server)
	if zone == "" {
		return "https://" + s.Subdomain + ".<your-domain>"
	}
	return "https://" + s.Subdomain + "." + zone
}

// printMCPConnectorURL prints the ready-to-paste claude.ai connector URL for
// an MCP-mode service. The URL embeds the capability token — it IS the
// credential.
func printMCPConnectorURL(s svcSpec) {
	if !s.MCP || s.MCPSecret == "" {
		return
	}
	path := s.MCPPath
	if path == "" {
		path = "/mcp"
	}
	fmt.Printf("  MCP connector URL (the URL is the credential — treat it as a secret):\n")
	fmt.Printf("    %s/%s%s\n", s.publicURL(), s.MCPSecret, path)
}

// unloadAllLaunchd unloads and removes every plist matching this service
// name, across current and legacy label prefixes. Returns the labels it
// actually removed.
func unloadAllLaunchd(name string) []string {
	dir := filepath.Join(homeDir(), "Library/LaunchAgents")
	var removed []string
	for _, prefix := range launchdLabelPrefixes {
		p := filepath.Join(dir, prefix+name+".plist")
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_ = exec.Command("launchctl", "unload", p).Run()
		if err := os.Remove(p); err == nil {
			removed = append(removed, prefix+name)
		}
	}
	return removed
}

func uninstallLaunchd(name string) {
	removed := unloadAllLaunchd(name)
	if len(removed) == 0 {
		exitf("no such service: %s", name)
	}
	for _, label := range removed {
		fmt.Printf("Uninstalled %s\n", label)
	}
}

func listLaunchd() {
	dir := filepath.Join(homeDir(), "Library/LaunchAgents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Println("(no services installed)")
		return
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".plist") {
			continue
		}
		for _, prefix := range launchdLabelPrefixes {
			if rest, ok := strings.CutPrefix(n, prefix); ok {
				svc := strings.TrimSuffix(rest, ".plist")
				if !seen[svc] {
					seen[svc] = true
					names = append(names, svc)
				}
				break
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Println("(no services installed)")
		return
	}
	fmt.Println("NAME")
	for _, n := range names {
		fmt.Println(n)
	}
}

// --- systemd (Linux) -------------------------------------------------------

// systemdUnitTemplate keeps the auth token OUT of the unit file. Units in
// /etc/systemd/system are world-readable by convention (and `systemctl cat`
// will happily print one for any user), so the token lives in a 0600
// EnvironmentFile that only root can read.
const systemdUnitTemplate = `[Unit]
Description=mygrok tunnel: %[1]s -> %[2]s
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=%[3]s
ExecStart=%[4]s %[10]s %[5]s --subdomain=%[1]s --server=%[6]s --host=%[7]s%[9]s
Restart=always
RestartSec=5
User=%[8]s

[Install]
WantedBy=multi-user.target
`

func systemdUnitPath(name string) string {
	return "/etc/systemd/system/mygrok-" + name + ".service"
}

// systemdEnvPath is the root-only file holding MYGROK_AUTHTOKEN for a unit.
func systemdEnvPath(name string) string {
	return "/etc/mygrok/" + name + ".env"
}

func installSystemd(s svcSpec) {
	user := os.Getenv("USER")
	if user == "" {
		user = "root"
	}
	extraArgs := ""
	// Not an argv entry, for the same reason as launchd: the unit file is
	// 0644 and `systemctl cat` shows it to any user. It rides in the
	// 0600 root-only EnvironmentFile instead.
	extraEnv := ""
	if s.BasicAuth != "" {
		extraEnv += "MYGROK_BASIC_AUTH=" + s.BasicAuth + "\n"
	}
	if s.Hostnames != "" {
		extraArgs += " --hostname=" + s.Hostnames
	}
	if s.LAN != "" {
		extraArgs += " --lan=" + s.LAN
		if s.LANPort != 0 {
			extraArgs += fmt.Sprintf(" --lan-port=%d", s.LANPort)
		}
	}
	if s.MCP {
		if s.MCPNoStrip {
			extraArgs += " --no-strip"
		}
		if s.MCPPath != "" && s.MCPPath != "/mcp" {
			extraArgs += " --path=" + s.MCPPath
		}
	}
	envFile := systemdEnvPath(s.Name)
	body := fmt.Sprintf(systemdUnitTemplate,
		s.Subdomain, fmt.Sprintf("%s:%s", s.LocalHost, s.Port),
		envFile, s.Binary, s.Port, s.Server, s.LocalHost, user, extraArgs, s.subcommand())

	unit := systemdUnitPath(s.Name)
	installRootFile(body, unit, "644")
	installRootFile("MYGROK_AUTHTOKEN="+s.Token+"\n"+extraEnv, envFile, "600")

	must("sudo", "systemctl", "daemon-reload")
	must("sudo", "systemctl", "enable", "--now", "mygrok-"+s.Name+".service")

	fmt.Printf("Installed mygrok-%s.service\n", s.Name)
	fmt.Printf("  Unit:  %s\n", unit)
	fmt.Printf("  Token: %s (root-only)\n", envFile)
	fmt.Printf("  Logs:  journalctl -u mygrok-%s -f\n", s.Name)
	fmt.Printf("  Public: %s  (active on boot)\n", s.publicURL())
	printMCPConnectorURL(s)
}

// installRootFile stages body in a 0600 temp file owned by the invoking
// user, then has install(1) place it at dst as root with the given mode.
// Staging first means a secret is never readable at a world-visible path,
// not even for the instant between write and chmod. -D creates any missing
// parent directory (e.g. /etc/mygrok).
func installRootFile(body, dst, mode string) {
	tmp, err := os.CreateTemp("", "mygrok-install-*")
	if err != nil {
		exitf("temp file: %v", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(body); err != nil {
		exitf("write tmp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		exitf("write tmp: %v", err)
	}
	must("sudo", "install", "-D", "-m", mode, "-o", "root", "-g", "root", tmp.Name(), dst)
}

func uninstallSystemd(name string) {
	unit := systemdUnitPath(name)
	must("sudo", "systemctl", "disable", "--now", "mygrok-"+name+".service")
	must("sudo", "rm", "-f", unit, systemdEnvPath(name))
	must("sudo", "systemctl", "daemon-reload")
	fmt.Printf("Uninstalled mygrok-%s.service\n", name)
}

func listSystemd() {
	cmd := exec.Command("systemctl", "list-units", "--type=service", "--all", "--no-legend", "mygrok-*")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

// --- helpers ---------------------------------------------------------------

func validServiceName(s string) bool {
	if s == "" || len(s) > 64 {
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

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		exitf("UserHomeDir: %v", err)
	}
	return h
}

func escapePlist(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func must(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		exitf("%s %s: %v", name, strings.Join(args, " "), err)
	}
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

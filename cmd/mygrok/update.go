package main

// `mygrok update` — pull the latest client binary from the configured
// tunnel server and install it over the running executable.
//
// The server already vends platform-specific binaries at
// /dl/mygrok-<os>-<arch>; we just download the matching one and use
// install(1) to drop it at our own path. install(1) writes via a fresh
// inode, which sidesteps the macOS "kernel cached this inode as won't
// run" SIGKILL trap that catches naive cp/mv replacements of ad-hoc
// signed Go binaries.
//
// The currently-running process keeps running its old code (it has the
// old text segment mmap'd from the now-deleted inode). New invocations
// see the new binary. Long-running tunnels managed by launchd/systemd
// pick up the new code on next restart.

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func cmdUpdate(args []string) {
	explicitCfg := scanFlag(args, "--config")
	cfg, cfgPath, _ := loadConfig(explicitCfg)

	// This command downloads an executable and installs it over the running
	// binary, with sudo if the destination needs it. A .mygrok.toml picked
	// up by walking up from the current directory is therefore not allowed
	// to choose the source: cloning a repository would otherwise be enough
	// to redirect someone's next `mygrok update` at an attacker's server.
	// An explicit --config, or the user's own ~/.mygrok/config.toml, is fine.
	if !configIsTrusted(cfgPath, explicitCfg) {
		if cfg != nil && cfg.Server != "" {
			fmt.Fprintf(os.Stderr,
				"(ignoring server = %q from %s — pass --server or --config to download from it)\n",
				cfg.Server, cfgPath)
		}
		cfg = nil
	}

	fs := flag.NewFlagSet("update", flag.ExitOnError)
	server := fs.String("server", resolveServer(cfg), "tunnel server to download from")
	from := fs.String("from", "", "explicit URL to download from (overrides --server)")
	configFlag := fs.String("config", "", "explicit path to a .mygrok.toml file")
	_ = configFlag
	fs.Parse(args)

	url := *from
	if url == "" {
		host := requireServer(*server)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		url = "https://" + host + "/dl/mygrok-" + runtime.GOOS + "-" + runtime.GOARCH
	}

	self, err := os.Executable()
	if err != nil {
		exitf("locate self: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	fmt.Printf(">> downloading %s\n", url)
	tmpPath, size, err := downloadBinary(url)
	if err != nil {
		exitf("download: %v", err)
	}
	defer os.Remove(tmpPath)
	fmt.Printf(">> downloaded %d bytes\n", size)

	if err := installOver(tmpPath, self); err != nil {
		exitf("install: %v", err)
	}

	// Confirm by asking the new binary for its version.
	if out, err := exec.Command(self, "version").Output(); err == nil {
		fmt.Printf(">> installed %s (%s)\n", self, strings.TrimSpace(string(out)))
	} else {
		fmt.Printf(">> installed %s\n", self)
	}
	fmt.Println(">> long-running services will pick up the new binary on next restart")
}

func downloadBinary(url string) (string, int64, error) {
	f, err := os.CreateTemp("", "mygrok-update-*")
	if err != nil {
		return "", 0, err
	}
	name := f.Name()

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		f.Close()
		os.Remove(name)
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.Close()
		os.Remove(name)
		return "", 0, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	n, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(name)
		return "", 0, err
	}
	if closeErr != nil {
		os.Remove(name)
		return "", 0, closeErr
	}
	if err := os.Chmod(name, 0o755); err != nil {
		os.Remove(name)
		return "", 0, err
	}
	return name, n, nil
}

// installOver tries `install -m 0755 src dst` directly, falling back to
// `sudo install ...` if the destination isn't writable.
func installOver(src, dst string) error {
	if err := exec.Command("install", "-m", "0755", src, dst).Run(); err == nil {
		return nil
	}
	fmt.Printf(">> %s not writable, retrying with sudo\n", dst)
	cmd := exec.Command("sudo", "install", "-m", "0755", src, dst)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

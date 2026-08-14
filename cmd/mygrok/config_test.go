package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		key  string
		want string
	}{
		{"separate value", []string{"--config", "foo.toml"}, "--config", "foo.toml"},
		{"equals form", []string{"--config=foo.toml"}, "--config", "foo.toml"},
		{"missing", []string{"--other", "x"}, "--config", ""},
		{"key without trailing value treated as missing", []string{"--config"}, "--config", ""},
		{"first match wins", []string{"--config=a", "--config=b"}, "--config", "a"},
		{"equals with empty value", []string{"--config="}, "--config", ""},
		{"prefix-only does not match", []string{"--configuration", "x"}, "--config", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scanFlag(tc.args, tc.key)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestLoadConfigExplicitMissing(t *testing.T) {
	_, _, err := loadConfig("/definitely/does/not/exist.toml")
	if err == nil {
		t.Error("expected error for missing explicit config")
	}
}

func TestLoadConfigExplicitValid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "custom.toml")
	contents := `
subdomain = "demo"
port = 4321
host = "h.example.com"
server = "tunnel.example.com:7000"
auth = "tok"
`
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, path, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if path != p {
		t.Errorf("path: got %q want %q", path, p)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if cfg.Subdomain != "demo" || cfg.Port != 4321 || cfg.Host != "h.example.com" ||
		cfg.Server != "tunnel.example.com:7000" || cfg.Auth != "tok" {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
}

func TestLoadConfigInvalidTOML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(p, []byte("not = valid = toml"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, gotPath, err := loadConfig(p)
	if err == nil {
		t.Error("expected parse error")
	}
	if gotPath != p {
		t.Errorf("path should be returned even on parse error: got %q want %q", gotPath, p)
	}
}

func TestLoadConfigNoExplicitNoneFound(t *testing.T) {
	// Walk findConfig away from any real config by chdiring into an empty
	// temp dir and pointing HOME at another empty dir.
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	// On macOS /tmp is a symlink to /private/tmp; chdir into a sub-dir to be
	// sure findConfig walks within the temp tree, not up into the real home.
	sub := filepath.Join(tmp, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cfg, path, err := loadConfig("")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg != nil || path != "" {
		// If a config was found, it must be outside the temp tree — which is
		// fine if the developer has a global ~/.mygrok/config.toml. Since we
		// pointed HOME at an empty dir, this should really be empty though.
		t.Errorf("expected no config found, got path=%q cfg=%+v", path, cfg)
	}
}

func TestFindConfigWalksUpFromCwd(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Place a config at tmp/.mygrok.toml and cd into tmp/a/b/c.
	cfgPath := filepath.Join(tmp, ".mygrok.toml")
	if err := os.WriteFile(cfgPath, []byte(`subdomain = "x"`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	deep := filepath.Join(tmp, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(deep); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := findConfig()
	// Symlinks on macOS (/tmp → /private/tmp) mean the returned path may have
	// a different prefix; resolve both with EvalSymlinks for comparison.
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(cfgPath)
	if gotResolved != wantResolved {
		t.Errorf("got %q (resolved %q) want %q (resolved %q)", got, gotResolved, cfgPath, wantResolved)
	}
}

func TestFindConfigFallsBackToHome(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Put a config only at ~/.mygrok/config.toml.
	if err := os.MkdirAll(filepath.Join(home, ".mygrok"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfgPath := filepath.Join(home, ".mygrok", "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`subdomain = "x"`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	sub := filepath.Join(tmp, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := findConfig()
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(cfgPath)
	if gotResolved != wantResolved {
		t.Errorf("got %q want %q", got, cfgPath)
	}
}

func TestFindConfigPrefersDotMygrokToml(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Both .mygrok.toml and .mygrok in the same dir; .mygrok.toml comes first
	// in configFilenames so it should win.
	a := filepath.Join(tmp, ".mygrok.toml")
	b := filepath.Join(tmp, ".mygrok")
	if err := os.WriteFile(a, []byte(""), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(b, []byte(""), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	got := findConfig()
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(a)
	if gotResolved != wantResolved {
		t.Errorf("got %q want %q", got, a)
	}
}

func TestFindConfigIgnoresDirectoriesWithMatchingName(t *testing.T) {
	tmp := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	// `.mygrok` as a directory shouldn't be picked up.
	if err := os.Mkdir(filepath.Join(tmp, ".mygrok"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if got := findConfig(); got != "" {
		t.Errorf("expected empty (dir entries should be skipped), got %q", got)
	}
}

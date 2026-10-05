package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLine(t *testing.T) {
	cases := []struct {
		in, key, value string
		ok             bool
	}{
		{"IA_AGENT_URL=https://x", "IA_AGENT_URL", "https://x", true},
		{"export IA_AGENT_KEY='iak_abc'", "IA_AGENT_KEY", "iak_abc", true},
		{`IA_AGENT_KEY="a b"`, "IA_AGENT_KEY", "a b", true},
		{"IA_AGENT_DOCKER=false # no docker here", "IA_AGENT_DOCKER", "false", true},
		{"# comment", "", "", false},
		{"", "", "", false},
		{"novalue", "", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseLine(c.in)
		if k != c.key || v != c.value || ok != c.ok {
			t.Errorf("parseLine(%q) = %q, %q, %v", c.in, k, v, ok)
		}
	}
}

func TestSaveValuesKeepsTheRestOfTheFile(t *testing.T) {
	restrict = func(string) error { return nil }
	defer func() { restrict = restrictFile }()
	path := filepath.Join(t.TempDir(), "sub", FileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := "# my settings\nIA_AGENT_DOCKER=false\nIA_AGENT_KEY=old\nIA_AGENT_KEY=dup\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveValues(path, map[string]string{KeyAgentKey: "iak_new", KeyServerID: "sid"}, []string{KeyServerID, KeyAgentKey})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	got := string(raw)
	for _, want := range []string{"# my settings", "IA_AGENT_DOCKER=false", "IA_AGENT_KEY=iak_new", "# IA_AGENT_KEY=dup", "IA_AGENT_SERVER_ID=sid"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	values, _ := ReadFile(path)
	if values[KeyAgentKey] != "iak_new" {
		t.Errorf("key = %q", values[KeyAgentKey])
	}
	if !isWindows {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestLoadPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(path, []byte("IA_AGENT_URL=https://file/\nIA_AGENT_SERVER_ID=from-file\nIA_AGENT_CONTAINER_LIMIT=7\n"), 0o600)
	t.Setenv("IA_AGENT_SERVER_ID", "from-env")
	cfg := Load(path)
	if !cfg.FileLoaded || cfg.File != path {
		t.Fatalf("file not loaded: %+v", cfg)
	}
	if cfg.URL != "https://file" {
		t.Errorf("URL = %q (trailing slash should be trimmed)", cfg.URL)
	}
	if cfg.ServerID != "from-env" {
		t.Errorf("ServerID = %q, env should win", cfg.ServerID)
	}
	if cfg.ContainerLimit != 7 {
		t.Errorf("ContainerLimit = %d", cfg.ContainerLimit)
	}
	if cfg.StateDir != filepath.Dir(path) {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if cfg.Enrolled() {
		t.Error("no key yet, should not count as enrolled")
	}
}

func TestModuleSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(path, []byte("IA_AGENT_DISKS=off\nIA_AGENT_DOCKER=no\n"), 0o600)
	cfg := Load(path)
	if cfg.DisksEnabled || cfg.DockerEnabled {
		t.Errorf("off / no in the file: disks = %v, docker = %v", cfg.DisksEnabled, cfg.DockerEnabled)
	}
	t.Setenv(KeyDocker, "ON")
	t.Setenv(KeyDisks, "maybe")
	cfg = Load(path)
	if !cfg.DockerEnabled {
		t.Error("the environment should win over the file")
	}
	if !cfg.DisksEnabled {
		t.Error("an unreadable value falls back to the default, on")
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg := Load(filepath.Join(t.TempDir(), "missing.env"))
	if cfg.FileLoaded {
		t.Fatal("a missing file cannot be loaded")
	}
	if cfg.SampleInterval != 2*time.Second || cfg.WindowInterval != 10*time.Second {
		t.Errorf("intervals = %v / %v", cfg.SampleInterval, cfg.WindowInterval)
	}
	if cfg.SpoolMaxAge != 48*time.Hour || cfg.SpoolMaxBytes != 65<<20 {
		t.Errorf("spool caps = %v / %d", cfg.SpoolMaxAge, cfg.SpoolMaxBytes)
	}
	if !cfg.DockerEnabled || cfg.ContainerLimit != 50 {
		t.Errorf("docker = %v / %d", cfg.DockerEnabled, cfg.ContainerLimit)
	}
	if !cfg.DisksEnabled {
		t.Error("disks should be on by default")
	}
	if len(cfg.FilesystemRoots) != 1 || cfg.FilesystemRoots[0] != "auto" {
		t.Errorf("roots = %v", cfg.FilesystemRoots)
	}
	if cfg.DockerHost != defaultDockerHost && os.Getenv("DOCKER_HOST") == "" {
		t.Errorf("DockerHost = %q", cfg.DockerHost)
	}
}

// An agent.env the agent may not read is not "not enrolled": that is how a
// root-owned file looked to the service.
func TestLoadUnreadableFile(t *testing.T) {
	if isWindows || os.Geteuid() == 0 {
		t.Skip("needs file modes that bind the user")
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte("IA_AGENT_SERVER_ID=sid\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	cfg := Load(path)
	if !errors.Is(cfg.FileErr, fs.ErrPermission) {
		t.Fatalf("FileErr = %v, want a permission error", cfg.FileErr)
	}
	if want := "cannot read " + path + ": permission denied (owner "; !strings.HasPrefix(cfg.FileErr.Error(), want) {
		t.Errorf("FileErr = %q, want it to start %q", cfg.FileErr, want)
	}
	if missing := Load(filepath.Join(t.TempDir(), FileName)); missing.FileErr != nil {
		t.Errorf("a missing file is not an error: %v", missing.FileErr)
	}
}

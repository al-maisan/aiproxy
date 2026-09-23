package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/aiproxy/internal/config"
)

func TestMainFunction(t *testing.T) {
	if os.Getenv("AIPROXY_TEST_MAIN") == "1" {
		os.Args = []string{"aiproxy", "-version"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMainFunction")
	cmd.Env = append(os.Environ(), "AIPROXY_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child process: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "aiproxy") {
		t.Fatalf("output = %q", out)
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "aiproxy") {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestRunPrintConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-print-config"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "listen") {
		t.Fatalf("print-config output = %q", out.String())
	}
	// The emitted config must itself be valid.
	path := filepath.Join(t.TempDir(), "printed.toml")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("printed config invalid: %v", err)
	}
}

func TestRunInitWritesConfig(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "config.toml")
	var out, errOut bytes.Buffer

	if err := run(context.Background(), []string{"-init", "-config", target}, &out, &errOut); err != nil {
		t.Fatalf("run -init: %v", err)
	}
	if _, err := config.Load(target); err != nil {
		t.Fatalf("generated config invalid: %v", err)
	}

	// Refuses to overwrite without -force.
	if err := run(context.Background(), []string{"-init", "-config", target}, &out, &errOut); err == nil {
		t.Fatal("expected error when config already exists")
	}

	if err := run(context.Background(), []string{"-init", "-config", target, "-force"}, &out, &errOut); err != nil {
		t.Fatalf("run -init -force: %v", err)
	}
}

func TestRunInitDefaultPath(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-init"}, &out, &errOut); err != nil {
		t.Fatalf("run -init: %v", err)
	}
	want := filepath.Join(xdg, "aiproxy", "config.toml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected config at %s: %v", want, err)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	var out, errOut bytes.Buffer
	if err := run(ctx, []string{"-config", path, "-listen", "127.0.0.1:0"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v (stderr: %s)", err, errOut.String())
	}
}

func TestRunBadConfigPath(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-config", filepath.Join(t.TempDir(), "absent.toml")}, &out, &errOut); err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestRunServeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:99999\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-config", path}, &out, &errOut); err == nil {
		t.Fatal("expected serve error for invalid port")
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-nope"}, &out, &errOut); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestDefaultUserConfigPathXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := defaultUserConfigPath(); got != filepath.Join("/xdg", "aiproxy", "config.toml") {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultUserConfigPathHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/tester")
	if got := defaultUserConfigPath(); got != filepath.Join("/home/tester", ".config", "aiproxy", "config.toml") {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultUserConfigPathNoHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	if got := defaultUserConfigPath(); got != "aiproxy.toml" {
		t.Fatalf("got %q, want aiproxy.toml", got)
	}
}

func TestWriteDefaultConfigRelativeDir(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	var out bytes.Buffer
	if err := writeDefaultConfig("config.toml", false, &out); err != nil {
		t.Fatalf("writeDefaultConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("expected config file: %v", err)
	}
}

func TestWriteDefaultConfigErrors(t *testing.T) {
	var out bytes.Buffer

	// Writing over an existing directory fails.
	if err := writeDefaultConfig(t.TempDir(), true, &out); err == nil {
		t.Fatal("expected write error for directory target")
	}

	// A parent path that is a file makes MkdirAll fail.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(file, "sub", "config.toml")
	if err := writeDefaultConfig(target, false, &out); err == nil {
		t.Fatal("expected mkdir error for file parent")
	}
}

func TestResolveConfigPathExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveConfigPath(path)
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if got != path {
		t.Fatalf("got %q, want %q", got, path)
	}
}

func TestResolveConfigPathExplicitMissing(t *testing.T) {
	if _, err := resolveConfigPath(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("expected error for missing explicit config")
	}
}

func TestResolveConfigPathFromEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIPROXY_CONFIG", path)
	got, err := resolveConfigPath("")
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if got != path {
		t.Fatalf("got %q, want %q", got, path)
	}
}

func TestResolveConfigPathFromEnvMissing(t *testing.T) {
	t.Setenv("AIPROXY_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	if _, err := resolveConfigPath(""); err == nil {
		t.Fatal("expected error for missing env config")
	}
}

func TestResolveConfigPathFromXDG(t *testing.T) {
	xdg := t.TempDir()
	dir := filepath.Join(xdg, "aiproxy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("listen = \"127.0.0.1:1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIPROXY_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	got, err := resolveConfigPath("")
	if err != nil {
		t.Fatalf("resolveConfigPath: %v", err)
	}
	if got != path {
		t.Fatalf("got %q, want %q", got, path)
	}
}

func TestResolveConfigPathNotFound(t *testing.T) {
	if _, err := os.Stat("/etc/aiproxy/config.toml"); err == nil {
		t.Skip("/etc/aiproxy/config.toml exists on this host")
	}
	if _, err := os.Stat("aiproxy.toml"); err == nil {
		t.Skip("aiproxy.toml exists in the working directory")
	}
	t.Setenv("AIPROXY_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := resolveConfigPath(""); err == nil {
		t.Fatal("expected error when no config file exists")
	}
}

func TestNewLogger(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		for _, level := range []string{"debug", "info", "warn", "error", "unknown"} {
			if logger := newLogger(config.Log{Level: level, Format: format}); logger == nil {
				t.Fatalf("newLogger(%q,%q) = nil", level, format)
			}
		}
	}
}

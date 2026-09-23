package keys

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveEnv(t *testing.T) {
	t.Setenv("AIPROXY_TEST_KEY", "env-secret")
	r := NewResolver("", 0)
	got, err := r.Resolve("env:AIPROXY_TEST_KEY")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "env-secret" {
		t.Fatalf("got %q, want %q", got, "env-secret")
	}
}

func TestResolveEnvMissing(t *testing.T) {
	r := NewResolver("", 0)
	if _, err := r.Resolve("env:AIPROXY_UNSET_KEY_12345"); err == nil {
		t.Fatal("expected error for unset environment variable")
	}
}

func TestResolveLiteralAndUnprefixed(t *testing.T) {
	r := NewResolver("", 0)
	for _, spec := range []string{"literal:plain", "plain"} {
		got, err := r.Resolve(spec)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", spec, err)
		}
		if got != "plain" {
			t.Fatalf("Resolve(%q) = %q, want %q", spec, got, "plain")
		}
	}
}

func TestResolveEmpty(t *testing.T) {
	r := NewResolver("", 0)
	if _, err := r.Resolve("   "); err == nil {
		t.Fatal("expected error for empty spec")
	}
}

func TestResolveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver("", 0)
	got, err := r.Resolve("file:" + path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "file-secret" {
		t.Fatalf("got %q, want %q", got, "file-secret")
	}
}

func TestResolveFileMissing(t *testing.T) {
	r := NewResolver("", 0)
	if _, err := r.Resolve("file:" + filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestResolveOpencodeAuth(t *testing.T) {
	path := writeAuthFile(t, `{"openrouter":{"type":"api","key":"or-secret"}}`)
	r := NewResolver(path, 0)
	got, err := r.Resolve("opencode-auth:openrouter")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "or-secret" {
		t.Fatalf("got %q, want %q", got, "or-secret")
	}
}

func TestResolveOpencodeAuthMissingProvider(t *testing.T) {
	path := writeAuthFile(t, `{"openrouter":{"type":"api","key":"or-secret"}}`)
	r := NewResolver(path, 0)
	if _, err := r.Resolve("opencode-auth:missing"); err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestResolveOpencodeAuthPicksUpRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeAt(t, path, `{"openrouter":{"type":"api","key":"first"}}`)
	r := NewResolver(path, 0) // zero TTL disables caching

	if got, _ := r.Resolve("opencode-auth:openrouter"); got != "first" {
		t.Fatalf("got %q, want first", got)
	}
	writeAt(t, path, `{"openrouter":{"type":"api","key":"second"}}`)
	if got, _ := r.Resolve("opencode-auth:openrouter"); got != "second" {
		t.Fatalf("got %q, want second after rotation", got)
	}
}

func TestResolveOpencodeAuthCache(t *testing.T) {
	path := writeAuthFile(t, `{"openrouter":{"type":"api","key":"cached"}}`)
	r := NewResolver(path, time.Hour)
	if got, _ := r.Resolve("opencode-auth:openrouter"); got != "cached" {
		t.Fatalf("got %q, want cached", got)
	}
	_ = os.Remove(path)
	if got, err := r.Resolve("opencode-auth:openrouter"); err != nil || got != "cached" {
		t.Fatalf("expected cached value, got %q, %v", got, err)
	}
}

func TestDefaultAuthPathHonoursEnv(t *testing.T) {
	t.Setenv("OPENCODE_AUTH_FILE", "/custom/auth.json")
	if got := DefaultAuthPath(); got != "/custom/auth.json" {
		t.Fatalf("got %q, want /custom/auth.json", got)
	}
}

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	writeAt(t, path, content)
	return path
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveEmptyComponents(t *testing.T) {
	r := NewResolver("", 0)
	for _, spec := range []string{"env:", "file:", "opencode-auth:", "literal:"} {
		if _, err := r.Resolve(spec); err == nil {
			t.Errorf("Resolve(%q) expected error", spec)
		}
	}
}

func TestResolveFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver("", 0)
	if _, err := r.Resolve("file:" + path); err == nil {
		t.Fatal("expected error for empty key file")
	}
}

func TestResolveFileTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "k"), []byte("home-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewResolver("", 0)
	got, err := r.Resolve("file:~/k")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "home-key" {
		t.Fatalf("got %q, want home-key", got)
	}
}

func TestResolveOpencodeAuthUnreadable(t *testing.T) {
	r := NewResolver(filepath.Join(t.TempDir(), "absent.json"), 0)
	if _, err := r.Resolve("opencode-auth:x"); err == nil {
		t.Fatal("expected error for unreadable auth file")
	}
}

func TestResolveOpencodeAuthMalformed(t *testing.T) {
	r := NewResolver(writeAuthFile(t, "{not json"), 0)
	if _, err := r.Resolve("opencode-auth:x"); err == nil {
		t.Fatal("expected error for malformed auth file")
	}
}

func TestResolveOpencodeAuthEmptyKey(t *testing.T) {
	r := NewResolver(writeAuthFile(t, `{"p":{"type":"api","key":""}}`), 0)
	if _, err := r.Resolve("opencode-auth:p"); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestDefaultAuthPathFromXDGDataHome(t *testing.T) {
	t.Setenv("OPENCODE_AUTH_FILE", "")
	t.Setenv("XDG_DATA_HOME", "/xdgdata")
	if got := DefaultAuthPath(); got != filepath.Join("/xdgdata", "opencode", "auth.json") {
		t.Fatalf("got %q", got)
	}
}

func TestDefaultAuthPathFromHome(t *testing.T) {
	t.Setenv("OPENCODE_AUTH_FILE", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/home/tester")
	want := filepath.Join("/home/tester", ".local", "share", "opencode", "auth.json")
	if got := DefaultAuthPath(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDefaultAuthPathNoHome(t *testing.T) {
	t.Setenv("OPENCODE_AUTH_FILE", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "")
	want := filepath.Join(".local", "share", "opencode", "auth.json")
	if got := DefaultAuthPath(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

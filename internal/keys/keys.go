// Package keys resolves API key sources used by upstream configuration.
package keys

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	prefixEnv          = "env:"
	prefixFile         = "file:"
	prefixOpencodeAuth = "opencode-auth:"
	prefixLiteral      = "literal:"
)

type cacheEntry struct {
	value   string
	expires time.Time
}

// Resolver resolves key source specs into secret values. Values read from an
// OpenCode auth file are cached for TTL so key rotation is picked up without a
// restart.
type Resolver struct {
	authPath string
	ttl      time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

// NewResolver returns a resolver backed by the OpenCode auth file at authPath.
func NewResolver(authPath string, ttl time.Duration) *Resolver {
	return &Resolver{
		authPath: authPath,
		ttl:      ttl,
		now:      time.Now,
		cache:    make(map[string]cacheEntry),
	}
}

// Resolve turns a key source spec into a secret value. Supported forms:
//
//	literal:<value>
//	env:<NAME>
//	file:<path>
//	opencode-auth:<provider>
//
// An unprefixed spec is treated as a literal value.
func (r *Resolver) Resolve(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", errors.New("empty api key spec")
	}
	switch {
	case strings.HasPrefix(spec, prefixEnv):
		name := strings.TrimPrefix(spec, prefixEnv)
		if name == "" {
			return "", errors.New("env: missing variable name")
		}
		value := os.Getenv(name)
		if value == "" {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		return value, nil

	case strings.HasPrefix(spec, prefixFile):
		path := strings.TrimPrefix(spec, prefixFile)
		if path == "" {
			return "", errors.New("file: missing path")
		}
		raw, err := os.ReadFile(expandHome(path))
		if err != nil {
			return "", fmt.Errorf("read key file: %w", err)
		}
		value := strings.TrimSpace(string(raw))
		if value == "" {
			return "", fmt.Errorf("key file %s is empty", path)
		}
		return value, nil

	case strings.HasPrefix(spec, prefixOpencodeAuth):
		provider := strings.TrimPrefix(spec, prefixOpencodeAuth)
		return r.fromAuthFile(provider)

	case strings.HasPrefix(spec, prefixLiteral):
		value := strings.TrimPrefix(spec, prefixLiteral)
		if value == "" {
			return "", errors.New("literal: missing value")
		}
		return value, nil

	default:
		return spec, nil
	}
}

func (r *Resolver) fromAuthFile(provider string) (string, error) {
	if provider == "" {
		return "", errors.New("opencode-auth: missing provider name")
	}

	r.mu.Lock()
	if e, ok := r.cache[provider]; ok && r.now().Before(e.expires) {
		r.mu.Unlock()
		return e.value, nil
	}
	r.mu.Unlock()

	raw, err := os.ReadFile(r.authPath)
	if err != nil {
		return "", fmt.Errorf("read opencode auth file %s: %w", r.authPath, err)
	}
	var doc map[string]struct {
		Type string `json:"type"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse opencode auth file %s: %w", r.authPath, err)
	}
	entry, ok := doc[provider]
	if !ok || entry.Key == "" {
		return "", fmt.Errorf("no api key for provider %q in %s", provider, r.authPath)
	}

	r.mu.Lock()
	r.cache[provider] = cacheEntry{value: entry.Key, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return entry.Key, nil
}

// DefaultAuthPath returns the OpenCode auth file location, honouring
// OPENCODE_AUTH_FILE and XDG_DATA_HOME.
func DefaultAuthPath() string {
	if p := os.Getenv("OPENCODE_AUTH_FILE"); p != "" {
		return expandHome(p)
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "share", "opencode", "auth.json")
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// Package config defines the aiproxy configuration file and its validation.
package config

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultTOML is the annotated default configuration shipped in the binary and
// emitted by `aiproxy -print-config` / `aiproxy -init`.
//
//go:embed default.toml
var DefaultTOML string

// Config is the root configuration.
type Config struct {
	// Listen is the host:port the proxy binds to.
	Listen string `toml:"listen"`
	// ClientToken, when set, is required as a bearer token on proxied routes.
	ClientToken string `toml:"client_token"`
	// Log configures logging.
	Log Log `toml:"log"`
	// Server configures HTTP server and upstream client behaviour.
	Server Server `toml:"server"`
	// Upstreams holds the primary (preferred) and fallback providers.
	Upstreams Upstreams `toml:"upstreams"`
	// Routes maps a requested model id to the primary upstream's model id.
	Routes []Route `toml:"routes"`
	// StripFields are JSON body fields removed before calling the primary.
	StripFields []string `toml:"strip_fields"`
	// Quota configures how upstream quota exhaustion is detected.
	Quota Quota `toml:"quota"`
	// Stats configures the request statistics endpoints.
	Stats Stats `toml:"stats"`
}

// Log configures the logger.
type Log struct {
	Level  string `toml:"level"`  // debug|info|warn|error
	Format string `toml:"format"` // text|json
}

// Server configures timeouts and limits.
type Server struct {
	ReadHeaderTimeout         Duration `toml:"read_header_timeout"`
	BodyReadTimeout           Duration `toml:"body_read_timeout"`
	UpstreamHeaderTimeout     Duration `toml:"upstream_header_timeout"`
	DialTimeout               Duration `toml:"dial_timeout"`
	IdleConnTimeout           Duration `toml:"idle_conn_timeout"`
	ShutdownTimeout           Duration `toml:"shutdown_timeout"`
	MaxBodyBytes              int64    `toml:"max_body_bytes"`
	FallbackOnConnectionError *bool    `toml:"fallback_on_connection_error"`
}

// Upstreams groups the primary and fallback upstreams.
type Upstreams struct {
	Primary  Upstream `toml:"primary"`
	Fallback Upstream `toml:"fallback"`
}

// Upstream describes a single provider endpoint.
type Upstream struct {
	Name      string `toml:"name"`
	ChatURL   string `toml:"chat_url"`
	ModelsURL string `toml:"models_url"`
	// APIKey is a key source: literal:<v>, env:<VAR>, file:<path> or
	// opencode-auth:<provider>. An unprefixed value is treated as a literal.
	APIKey string `toml:"api_key"`
}

// Route maps a requested model id to the primary upstream's model id. Models
// without a route are served directly by the fallback upstream.
type Route struct {
	// Requested is the model id the client sends.
	Requested string `toml:"requested"`
	// Primary is the model id at the primary upstream.
	Primary string `toml:"primary"`
	// Fallback is the optional model id at the fallback upstream. It defaults
	// to Requested, since clients normally speak the fallback's model ids.
	Fallback string `toml:"fallback"`
}

// FallbackModel returns the model id to use on the fallback upstream.
func (r Route) FallbackModel() string {
	if r.Fallback != "" {
		return r.Fallback
	}
	return r.Requested
}

// Quota configures quota detection.
type Quota struct {
	Statuses []int    `toml:"statuses"`
	Patterns []string `toml:"patterns"`
}

// Stats configures the request statistics endpoints. When Disabled is set,
// neither /stats nor /metrics is registered.
type Stats struct {
	Disabled bool `toml:"disabled"`
	// Buckets are latency histogram upper bounds in seconds. Empty means the
	// built-in defaults are used.
	Buckets []float64 `toml:"buckets"`
}

// Duration is a time.Duration that unmarshals from strings such as "15s".
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// FallbackOnConnErr reports whether a failed/unreachable primary triggers the
// fallback upstream. Defaults to true when unset.
func (c *Config) FallbackOnConnErr() bool {
	return c.Server.FallbackOnConnectionError == nil || *c.Server.FallbackOnConnectionError
}

// Default returns a configuration with all defaults applied.
func Default() *Config {
	yes := true
	return &Config{
		Listen: "127.0.0.1:8787",
		Log:    Log{Level: "info", Format: "text"},
		Server: Server{
			ReadHeaderTimeout:         Duration(15 * time.Second),
			BodyReadTimeout:           Duration(60 * time.Second),
			UpstreamHeaderTimeout:     Duration(180 * time.Second),
			DialTimeout:               Duration(10 * time.Second),
			IdleConnTimeout:           Duration(90 * time.Second),
			ShutdownTimeout:           Duration(15 * time.Second),
			MaxBodyBytes:              64 << 20,
			FallbackOnConnectionError: &yes,
		},
		Upstreams: Upstreams{
			Primary: Upstream{ //nolint:gosec // key-source spec, not a secret
				Name:      "opencode-go",
				ChatURL:   "https://opencode.ai/zen/go/v1/chat/completions",
				ModelsURL: "https://opencode.ai/zen/go/v1/models",
				APIKey:    "opencode-auth:opencode-go",
			},
			Fallback: Upstream{ //nolint:gosec // key-source spec, not a secret
				Name:      "openrouter",
				ChatURL:   "https://openrouter.ai/api/v1/chat/completions",
				ModelsURL: "https://openrouter.ai/api/v1/models",
				APIKey:    "opencode-auth:openrouter",
			},
		},
		StripFields: []string{
			"provider", "route", "transforms", "models", "plugins", "session_id", "usage",
		},
		Quota: Quota{
			Statuses: []int{402, 429},
			// Deliberately specific: broad terms like "limit exceeded" also
			// match context-length errors and would cause spurious fallbacks.
			Patterns: []string{
				`(?i)\bquota\b`,
				`(?i)usage limit`,
				`(?i)usage-limit`,
				`(?i)insufficient[_ ]?quota`,
				`(?i)insufficient (funds|credits|balance)`,
				`(?i)credit balance`,
				`(?i)out of credits`,
				`(?i)no (more )?credits`,
				`(?i)monthly limit`,
			},
		},
	}
}

// Load reads, decodes and validates a TOML configuration file. Fields not
// present keep their defaults; unknown fields are rejected. An empty file is
// valid and yields the defaults.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied config file
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()

	cfg := Default()
	md, err := toml.NewDecoder(f).Decode(cfg)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		return nil, fmt.Errorf("config %s: unknown fields: %s", path, strings.Join(keys, ", "))
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// normalize applies canonicalisation that is not validation: currently it trims
// surrounding whitespace from client_token so it matches the presented value
// (bearerToken already trims).
func (c *Config) normalize() error {
	if err := c.checkClientToken(); err != nil {
		return err
	}
	c.ClientToken = strings.TrimSpace(c.ClientToken)
	return nil
}

// checkClientToken rejects a client_token that is present but blank. Such a
// token would be trimmed to "" and silently disable authentication.
func (c *Config) checkClientToken() error {
	if c.ClientToken != "" && strings.TrimSpace(c.ClientToken) == "" {
		return errors.New("client_token: must not be blank")
	}
	return nil
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []error

	if strings.TrimSpace(c.Listen) == "" {
		errs = append(errs, errors.New("listen: must not be empty"))
	}
	if err := c.checkClientToken(); err != nil {
		errs = append(errs, err)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level: %q is not one of debug|info|warn|error", c.Log.Level))
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("log.format: %q is not one of text|json", c.Log.Format))
	}

	errs = append(errs, validateUpstream("upstreams.primary", c.Upstreams.Primary)...)
	errs = append(errs, validateUpstream("upstreams.fallback", c.Upstreams.Fallback)...)

	seen := make(map[string]struct{}, len(c.Routes))
	for i, r := range c.Routes {
		if strings.TrimSpace(r.Requested) == "" {
			errs = append(errs, fmt.Errorf("routes[%d].requested: must not be empty", i))
		}
		if strings.TrimSpace(r.Primary) == "" {
			errs = append(errs, fmt.Errorf("routes[%d].primary: must not be empty", i))
		}
		if _, dup := seen[r.Requested]; dup {
			errs = append(errs, fmt.Errorf("routes[%d]: duplicate requested model %q", i, r.Requested))
		}
		seen[r.Requested] = struct{}{}
	}

	for i, p := range c.Quota.Patterns {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("quota.patterns[%d]: %w", i, err))
		}
	}

	if c.Server.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("server.max_body_bytes: must be greater than zero"))
	}
	if c.Server.ReadHeaderTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.read_header_timeout: must be greater than zero"))
	}
	if c.Server.BodyReadTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.body_read_timeout: must be greater than zero"))
	}
	if c.Server.UpstreamHeaderTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.upstream_header_timeout: must be greater than zero"))
	}
	if c.Server.DialTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.dial_timeout: must be greater than zero"))
	}
	if c.Server.ShutdownTimeout.Std() <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout: must be greater than zero"))
	}

	for i, b := range c.Stats.Buckets {
		if !(b > 0) {
			errs = append(errs, fmt.Errorf("stats.buckets[%d]: must be greater than zero", i))
		}
	}

	return errors.Join(errs...)
}

func validateUpstream(field string, u Upstream) []error {
	var errs []error
	if strings.TrimSpace(u.Name) == "" {
		errs = append(errs, fmt.Errorf("%s.name: must not be empty", field))
	}
	errs = append(errs, validateURL(field+".chat_url", u.ChatURL)...)
	if u.ModelsURL != "" {
		errs = append(errs, validateURL(field+".models_url", u.ModelsURL)...)
	}
	if strings.TrimSpace(u.APIKey) == "" {
		errs = append(errs, fmt.Errorf("%s.api_key: must not be empty", field))
	} else if strings.TrimSpace(u.APIKey) == "literal:" {
		errs = append(errs, fmt.Errorf("%s.api_key: literal: must have a value", field))
	}
	return errs
}

func validateURL(field, raw string) []error {
	if strings.TrimSpace(raw) == "" {
		return []error{fmt.Errorf("%s: must not be empty", field)}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", field, err)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return []error{fmt.Errorf("%s: scheme must be http or https", field)}
	}
	if u.Host == "" {
		return []error{fmt.Errorf("%s: missing host", field)}
	}
	return nil
}

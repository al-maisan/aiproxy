package config

import (
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/aiproxy/internal/quota"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("Default().Validate() = %v, want nil", err)
	}
}

func TestLoadEmptyFileYieldsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def := Default()
	if cfg.Listen != def.Listen {
		t.Fatalf("Listen = %q, want %q", cfg.Listen, def.Listen)
	}
	if cfg.Log != def.Log {
		t.Fatalf("Log = %+v, want %+v", cfg.Log, def.Log)
	}
	if len(cfg.Routes) != 0 {
		t.Fatalf("Routes = %v, want empty", cfg.Routes)
	}
	if !cfg.FallbackOnConnErr() {
		t.Fatal("FallbackOnConnErr() = false, want default true")
	}
}

func TestLoadOverridesAndPreservesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
listen = "0.0.0.0:9000"
client_token = "sekret"

[log]
level = "debug"
format = "json"

[server]
read_header_timeout = "5s"
max_body_bytes = 1024
fallback_on_connection_error = false

[upstreams.primary]
name = "primary"
chat_url = "http://primary.test/v1/chat/completions"
models_url = "http://primary.test/v1/models"
api_key = "literal:primary-key"

[upstreams.fallback]
name = "fallback"
chat_url = "http://fallback.test/v1/chat/completions"
models_url = "http://fallback.test/v1/models"
api_key = "env:FALLBACK_KEY"

[[routes]]
requested = "requested-a"
primary = "primary-a"

[[routes]]
requested = "requested-b"
primary = "primary-b"
fallback = "fallback-b"

[quota]
statuses = [429]
patterns = ['(?i)quota']
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Listen != "0.0.0.0:9000" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.ClientToken != "sekret" {
		t.Errorf("ClientToken = %q", cfg.ClientToken)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "json" {
		t.Errorf("Log = %+v", cfg.Log)
	}
	if cfg.Server.ReadHeaderTimeout.Std() != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %s", cfg.Server.ReadHeaderTimeout.Std())
	}
	if cfg.Server.MaxBodyBytes != 1024 {
		t.Errorf("MaxBodyBytes = %d", cfg.Server.MaxBodyBytes)
	}
	if cfg.FallbackOnConnErr() {
		t.Error("FallbackOnConnErr() = true, want false")
	}
	// Defaults for fields not present in the file are preserved.
	if cfg.Server.DialTimeout.Std() != Default().Server.DialTimeout.Std() {
		t.Errorf("DialTimeout = %s, want default", cfg.Server.DialTimeout.Std())
	}
	if cfg.Upstreams.Primary.Name != "primary" || cfg.Upstreams.Fallback.Name != "fallback" {
		t.Errorf("Upstreams = %+v", cfg.Upstreams)
	}

	if got := cfg.Routes[0].FallbackModel(); got != "requested-a" {
		t.Errorf("route[0].FallbackModel() = %q, want requested-a", got)
	}
	if got := cfg.Routes[1].FallbackModel(); got != "fallback-b" {
		t.Errorf("route[1].FallbackModel() = %q, want fallback-b", got)
	}
	if len(cfg.Quota.Statuses) != 1 || cfg.Quota.Statuses[0] != 429 {
		t.Errorf("Quota.Statuses = %v", cfg.Quota.Statuses)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	_, err := Load(writeConfig(t, "listen = \"127.0.0.1:1\"\nnope = true\n"))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown fields") || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v, want mention of unknown field", err)
	}
}

func TestLoadRejectsMalformedTOML(t *testing.T) {
	if _, err := Load(writeConfig(t, "listen =")); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	_, err := Load(writeConfig(t, "[server]\nread_header_timeout = \"nope\"\n"))
	if err == nil {
		t.Fatal("expected duration error")
	}
	if !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("error = %v, want invalid duration", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.toml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty listen", func(c *Config) { c.Listen = "" }, "listen"},
		{"blank client token", func(c *Config) { c.ClientToken = "   " }, "client_token"},
		{"bad log level", func(c *Config) { c.Log.Level = "loud" }, "log.level"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"primary empty name", func(c *Config) { c.Upstreams.Primary.Name = "" }, "upstreams.primary.name"},
		{"primary empty chat url", func(c *Config) { c.Upstreams.Primary.ChatURL = "" }, "upstreams.primary.chat_url"},
		{"primary bad scheme", func(c *Config) { c.Upstreams.Primary.ChatURL = "ftp://x/y" }, "scheme must be http or https"},
		{"primary missing host", func(c *Config) { c.Upstreams.Primary.ChatURL = "http:///y" }, "missing host"},
		{"primary bad models url", func(c *Config) { c.Upstreams.Primary.ModelsURL = "://" }, "upstreams.primary.models_url"},
		{"primary empty key", func(c *Config) { c.Upstreams.Primary.APIKey = "" }, "upstreams.primary.api_key"},
		{"primary empty literal key", func(c *Config) { c.Upstreams.Primary.APIKey = "literal:" }, "upstreams.primary.api_key"},
		{"fallback empty name", func(c *Config) { c.Upstreams.Fallback.Name = "" }, "upstreams.fallback.name"},
		{"route empty requested", func(c *Config) { c.Routes = []Route{{Primary: "x"}} }, "routes[0].requested"},
		{"route empty primary", func(c *Config) { c.Routes = []Route{{Requested: "x"}} }, "routes[0].primary"},
		{"duplicate route", func(c *Config) {
			c.Routes = []Route{{Requested: "x", Primary: "a"}, {Requested: "x", Primary: "b"}}
		}, "duplicate requested model"},
		{"bad quota pattern", func(c *Config) { c.Quota.Patterns = []string{"("} }, "quota.patterns[0]"},
		{"zero body bytes", func(c *Config) { c.Server.MaxBodyBytes = 0 }, "server.max_body_bytes"},
		{"zero read header timeout", func(c *Config) { c.Server.ReadHeaderTimeout = 0 }, "server.read_header_timeout"},
		{"zero body read timeout", func(c *Config) { c.Server.BodyReadTimeout = 0 }, "server.body_read_timeout"},
		{"zero upstream header timeout", func(c *Config) { c.Server.UpstreamHeaderTimeout = 0 }, "server.upstream_header_timeout"},
		{"zero dial timeout", func(c *Config) { c.Server.DialTimeout = 0 }, "server.dial_timeout"},
		{"zero shutdown timeout", func(c *Config) { c.Server.ShutdownTimeout = 0 }, "server.shutdown_timeout"},
		{"bad stats bucket", func(c *Config) { c.Stats.Buckets = []float64{1, 0} }, "stats.buckets[1]"},
		{"duplicate stats bucket", func(c *Config) { c.Stats.Buckets = []float64{1, 1} }, "duplicate bucket"},
		{"infinite stats bucket", func(c *Config) { c.Stats.Buckets = []float64{1, math.Inf(1)} }, "stats.buckets[1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestFallbackOnConnErrExplicitTrue(t *testing.T) {
	cfg := Default()
	yes := true
	cfg.Server.FallbackOnConnectionError = &yes
	if !cfg.FallbackOnConnErr() {
		t.Fatal("expected true")
	}
}

func TestDurationUnmarshalText(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("1m30s")); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if d.Std() != 90*time.Second {
		t.Fatalf("got %s, want 1m30s", d.Std())
	}
	if err := d.UnmarshalText([]byte("bogus")); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestDurationMarshalText(t *testing.T) {
	text, err := Duration(90 * time.Second).MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(text) != "1m30s" {
		t.Fatalf("got %q, want 1m30s", text)
	}
}

func TestDefaultTemplateIsValid(t *testing.T) {
	if _, err := Load(writeConfig(t, DefaultTOML)); err != nil {
		t.Fatalf("embedded default.toml is not valid: %v", err)
	}
}

func TestLoadSurfacesValidationErrors(t *testing.T) {
	_, err := Load(writeConfig(t, "listen = \"\"\n"))
	if err == nil {
		t.Fatal("expected validation error from Load")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("error = %v, want mention of listen", err)
	}
}

func TestLoadTrimsClientToken(t *testing.T) {
	cfg, err := Load(writeConfig(t, "listen = \"127.0.0.1:1\"\nclient_token = \"  sekret  \"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientToken != "sekret" {
		t.Fatalf("ClientToken = %q, want trimmed %q", cfg.ClientToken, "sekret")
	}
}

func TestLoadRejectsBlankClientToken(t *testing.T) {
	_, err := Load(writeConfig(t, "listen = \"127.0.0.1:1\"\nclient_token = \"   \"\n"))
	if err == nil {
		t.Fatal("expected error for blank client_token")
	}
	if !strings.Contains(err.Error(), "client_token") {
		t.Fatalf("error = %v, want mention of client_token", err)
	}
}

func TestValidateRejectsBlankClientTokenWithoutMutating(t *testing.T) {
	cfg := Default()
	cfg.ClientToken = "   "
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted a blank client_token")
	}
	if cfg.ClientToken != "   " {
		t.Fatalf("Validate() mutated ClientToken to %q; it must stay pure", cfg.ClientToken)
	}
}

func TestLoadStats(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
listen = "127.0.0.1:1"

[stats]
disabled = true
buckets = [0.5, 1, 2.5]
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Stats.Disabled {
		t.Error("Stats.Disabled = false, want true")
	}
	if len(cfg.Stats.Buckets) != 3 || cfg.Stats.Buckets[2] != 2.5 {
		t.Errorf("Stats.Buckets = %v", cfg.Stats.Buckets)
	}
}

func TestDefaultStatsEnabled(t *testing.T) {
	if Default().Stats.Disabled {
		t.Error("stats should be enabled by default")
	}
}

func TestDefaultQuotaPatternsArePrecise(t *testing.T) {
	def := Default()
	d, err := quota.New(def.Quota.Statuses, def.Quota.Patterns)
	if err != nil {
		t.Fatalf("quota.New: %v", err)
	}
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"context length error is not quota", `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, false},
		{"insufficient quota", `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota"}}`, true},
		{"credit balance", `{"error":{"message":"Your credit balance is too low"}}`, true},
		{"usage limit", `{"error":{"message":"Monthly usage limit reached"}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := d.IsQuota(http.StatusBadRequest, []byte(tt.body)); got != tt.want {
				t.Fatalf("IsQuota = %v, want %v", got, tt.want)
			}
		})
	}
}

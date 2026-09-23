package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/al-maisan/aiproxy/internal/config"
	"github.com/al-maisan/aiproxy/internal/keys"
)

type fakeCall struct {
	method string
	path   string
	header http.Header
	body   []byte
}

type fake struct {
	server *httptest.Server

	mu    sync.Mutex
	calls []fakeCall
}

func newFake(t *testing.T, respond func(w http.ResponseWriter, call int)) *fake {
	t.Helper()
	f := &fake{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, fakeCall{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		call := len(f.calls)
		f.mu.Unlock()
		if respond != nil {
			respond(w, call)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fake) call(i int) fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func (f *fake) bodyJSON(t *testing.T, i int) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(f.call(i).body, &payload); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	return payload
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(t *testing.T, primaryURL, fallbackURL string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Log = config.Log{Level: "error", Format: "text"}
	cfg.Upstreams.Primary = config.Upstream{
		Name: "primary", ChatURL: primaryURL, APIKey: "literal:primary-key",
	}
	cfg.Upstreams.Fallback = config.Upstream{
		Name: "fallback", ChatURL: fallbackURL, ModelsURL: fallbackURL + "/models", APIKey: "literal:fallback-key",
	}
	cfg.Routes = []config.Route{{Requested: "req-model", Primary: "pri-model"}}
	return cfg
}

func newProxy(t *testing.T, cfg *config.Config) *httptest.Server {
	t.Helper()
	p, err := New(cfg, discardLogger(), keys.NewResolver("", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string, headers map[string]string) (*http.Response, string) { //nolint:gocritic // test helper
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func get(t *testing.T, url string) (*http.Response, string) { //nolint:gocritic // test helper
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func TestRoutedModelServedByPrimary(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	})
	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be called")
	})

	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"req-model","provider":{"sort":"throughput"},"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Request-ID": "rid-1"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "data: {\"ok\":true}\n\n" {
		t.Fatalf("body = %q", body)
	}
	if got := resp.Header.Get("X-Request-ID"); got != "rid-1" {
		t.Fatalf("X-Request-ID = %q, want rid-1", got)
	}
	if primary.count() != 1 || fallback.count() != 0 {
		t.Fatalf("primary=%d fallback=%d, want 1/0", primary.count(), fallback.count())
	}

	call := primary.call(0)
	if got := call.header.Get("Authorization"); got != "Bearer primary-key" {
		t.Errorf("primary authorization = %q", got)
	}
	if got := call.header.Get("X-Request-ID"); got != "rid-1" {
		t.Errorf("primary request id = %q", got)
	}
	payload := primary.bodyJSON(t, 0)
	if payload["model"] != "pri-model" {
		t.Errorf("primary model = %v, want pri-model", payload["model"])
	}
	if _, ok := payload["provider"]; ok {
		t.Errorf("provider field must be stripped for primary, got %v", payload["provider"])
	}
	if _, ok := payload["messages"]; !ok {
		t.Error("messages must be forwarded to primary")
	}
}

func TestQuotaFallsBackVerbatim(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"usage limit reached"}}`)
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: fallback\n\n")
	})

	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"req-model","provider":{"sort":"throughput","quantizations":["fp8"]}}`, nil)

	if resp.StatusCode != http.StatusOK || body != "data: fallback\n\n" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if primary.count() != 1 || fallback.count() != 1 {
		t.Fatalf("primary=%d fallback=%d, want 1/1", primary.count(), fallback.count())
	}

	payload := fallback.bodyJSON(t, 0)
	if payload["model"] != "req-model" {
		t.Errorf("fallback model = %v, want req-model", payload["model"])
	}
	provider, ok := payload["provider"].(map[string]any)
	if !ok {
		t.Fatalf("fallback provider = %v, want preserved object", payload["provider"])
	}
	if provider["sort"] != "throughput" {
		t.Errorf("fallback provider.sort = %v, want throughput", provider["sort"])
	}
}

func TestQuotaPatternTriggersFallbackWithoutStatusMatch(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"quota exceeded"}`)
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})

	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusOK || body != "ok" {
		t.Fatalf("status=%d body=%q, want fallback served", resp.StatusCode, body)
	}
	if fallback.count() != 1 {
		t.Fatalf("fallback count = %d, want 1", fallback.count())
	}
}

func TestNonQuotaPrimaryErrorPassesThrough(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad input"}`)
	})
	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be called")
	})

	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if body != `{"error":"bad input"}` {
		t.Fatalf("body = %q, want primary error passthrough", body)
	}
	if fallback.count() != 0 {
		t.Fatalf("fallback count = %d, want 0", fallback.count())
	}
}

func TestUnroutedModelUsesFallback(t *testing.T) {
	primary := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("primary must not be called for unrouted model")
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "fallback-served")
	})

	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"other-model"}`, nil)
	if resp.StatusCode != http.StatusOK || body != "fallback-served" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if primary.count() != 0 || fallback.count() != 1 {
		t.Fatalf("primary=%d fallback=%d, want 0/1", primary.count(), fallback.count())
	}
	if got := fallback.bodyJSON(t, 0)["model"]; got != "other-model" {
		t.Fatalf("fallback model = %v", got)
	}
}

func TestPrimaryConnectionErrorFallsBack(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "fallback-served")
	})
	cfg := testConfig(t, deadURL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusOK || body != "fallback-served" {
		t.Fatalf("status=%d body=%q, want fallback", resp.StatusCode, body)
	}
	if fallback.count() != 1 {
		t.Fatalf("fallback count = %d, want 1", fallback.count())
	}
}

func TestPrimaryConnectionErrorWithoutFallbackReturnsBadGateway(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be called")
	})
	cfg := testConfig(t, deadURL+"/chat/completions", fallback.server.URL)
	no := false
	cfg.Server.FallbackOnConnectionError = &no
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if fallback.count() != 0 {
		t.Fatalf("fallback count = %d, want 0", fallback.count())
	}
}

func TestRouteFallbackModelOverride(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, "quota")
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})

	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Routes = []config.Route{{Requested: "req-model", Primary: "pri-model", Fallback: "fallback-model"}}
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if got := fallback.bodyJSON(t, 0)["model"]; got != "fallback-model" {
		t.Fatalf("fallback model = %v, want fallback-model", got)
	}
}

func TestClientTokenAuth(t *testing.T) {
	primary := newFake(t, nil)
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.ClientToken = "sekret"
	srv := newProxy(t, cfg)

	if resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"other"}`, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", resp.StatusCode)
	}
	if resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"other"}`, map[string]string{"Authorization": "Bearer wrong"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", resp.StatusCode)
	}
	if resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"other"}`, map[string]string{"Authorization": "Bearer sekret"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200", resp.StatusCode)
	}
	// Health endpoints are not authenticated.
	if resp, _ := get(t, srv.URL+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Server.MaxBodyBytes = 10
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"much-too-large"}`, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestBodyReadTimeoutReturns408(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Server.BodyReadTimeout = config.Duration(150 * time.Millisecond)
	srv := newProxy(t, cfg)

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// Announce more body than we send so the read blocks until the deadline.
	if _, err := io.WriteString(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", resp.StatusCode)
	}
}

func TestReadyzLogsOnlyStateTransitions(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Primary.APIKey = "env:AIPROXY_READYZ_TRANSITION_UNSET"
	cfg.ClientToken = "t"
	p, err := New(cfg, log, keys.NewResolver("", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	for i := 0; i < 5; i++ {
		resp, _ := getWith(t, srv.URL+"/readyz", map[string]string{"Authorization": "Bearer t"})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	}
	got := strings.Count(buf.String(), "upstream key unavailable")
	if got != 1 {
		t.Fatalf("logged %d unavailability messages across 5 polls, want 1", got)
	}
}

func TestReadyzLogsRecoveryTransition(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// The key resolves from a file whose path we can create after the first poll.
	keyPath := filepath.Join(t.TempDir(), "primary.key")

	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Primary.APIKey = "file:" + keyPath
	cfg.ClientToken = "t"
	p, err := New(cfg, log, keys.NewResolver("", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.Handler())
	t.Cleanup(srv.Close)

	auth := map[string]string{"Authorization": "Bearer t"}

	// Unavailable: one Warn.
	for i := 0; i < 3; i++ {
		resp, _ := getWith(t, srv.URL+"/readyz", auth)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", resp.StatusCode)
		}
	}
	if got := strings.Count(buf.String(), "upstream key unavailable"); got != 1 {
		t.Fatalf("unavailable messages = %d, want 1", got)
	}

	// Make the key resolvable, then poll again: exactly one recovery Info.
	if err := os.WriteFile(keyPath, []byte("k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		resp, _ := getWith(t, srv.URL+"/readyz", auth)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	if got := strings.Count(buf.String(), "upstream key available again"); got != 1 {
		t.Fatalf("recovery messages = %d, want 1", got)
	}
	if got := strings.Count(buf.String(), "upstream key unavailable"); got != 1 {
		t.Fatalf("unavailable messages after recovery = %d, want still 1", got)
	}
}

func TestForwardFallbackKeyUnavailableMessage(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "primary-served")
	})
	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be reached without a key")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Upstreams.Fallback.APIKey = "env:AIPROXY_PROXY_UNSET_FALLBACK"
	srv := newProxy(t, cfg)

	// Unrouted model goes straight to the fallback, whose key is unresolved.
	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"other"}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(body, "api key unavailable") {
		t.Fatalf("body = %q, want api-key-unavailable message", body)
	}
}

func TestInvalidJSON(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{not json`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMissingModel(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"messages":[]}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	srv := newProxy(t, cfg)

	resp, body := get(t, srv.URL+"/healthz")
	if resp.StatusCode != http.StatusOK || body != "ok\n" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestReadyzReady(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	srv := newProxy(t, cfg)

	resp, body := get(t, srv.URL+"/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"ready":true`) {
		t.Fatalf("body = %q, want ready true", body)
	}
}

func TestReadyzUnready(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Primary.APIKey = "env:AIPROXY_READY_TEST_UNSET"
	srv := newProxy(t, cfg)

	resp, body := get(t, srv.URL+"/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(body, `"ready":false`) {
		t.Fatalf("body = %q, want ready false", body)
	}
}

func TestModelsProxiedToFallback(t *testing.T) {
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	})
	cfg := testConfig(t, "http://primary.test/chat", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := get(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"object":"list"`) {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	call := fallback.call(0)
	if call.method != http.MethodGet || call.path != "/models" {
		t.Fatalf("fallback call = %s %s, want GET /models", call.method, call.path)
	}
	if got := call.header.Get("Authorization"); got != "Bearer fallback-key" {
		t.Fatalf("models authorization = %q", got)
	}
}

func TestModelsNotConfigured(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Fallback.ModelsURL = ""
	srv := newProxy(t, cfg)

	resp, _ := get(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestModelsInvalidURL(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Fallback.ModelsURL = "://invalid"
	srv := newProxy(t, cfg)

	resp, _ := get(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestNewRejectsInvalidQuotaConfig(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Quota.Statuses = []int{999}
	if _, err := New(cfg, discardLogger(), keys.NewResolver("", 0)); err == nil {
		t.Fatal("expected error for invalid quota status")
	}
}

func TestLargeStreamingBodyPassesThrough(t *testing.T) {
	chunk := strings.Repeat("x", 4096)
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 64; i++ {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if want := 64 * len("data: "+chunk+"\n\n"); len(body) != want {
		t.Fatalf("body length = %d, want %d", len(body), want)
	}
}

func TestCloneWithModel(t *testing.T) {
	payload := map[string]any{"model": "old", "provider": "x", "keep": 1}
	strip := map[string]struct{}{"provider": {}}

	encoded, err := cloneWithModel(payload, "new", strip)
	if err != nil {
		t.Fatalf("cloneWithModel: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["model"] != "new" {
		t.Errorf("model = %v, want new", got["model"])
	}
	if _, ok := got["provider"]; ok {
		t.Error("provider should be stripped")
	}
	if got["keep"] != float64(1) {
		t.Errorf("keep = %v, want 1", got["keep"])
	}
	// The original map must be untouched.
	if payload["model"] != "old" {
		t.Errorf("original payload mutated: %v", payload["model"])
	}
}

func TestPrimaryBodyPreservesLargeIntegers(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "{}")
	})
	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be called")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	// 9007199254740993 is not representable as float64; it must not be rounded.
	resp, _ := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"req-model","seed":9007199254740993}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := string(primary.call(0).body); !strings.Contains(got, "9007199254740993") {
		t.Fatalf("primary body = %s, want exact seed 9007199254740993", got)
	}
}

func TestTrailingJSONRejected(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions",
		`{"model":"req-model"}{"model":"smuggled"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReadyzHidesKeySourceDetail(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	secretPath := "/nonexistent/secret/location/aiproxy.key"
	cfg.Upstreams.Primary.APIKey = "file:" + secretPath
	cfg.ClientToken = "t"
	srv := newProxy(t, cfg)

	resp, body := getWith(t, srv.URL+"/readyz", map[string]string{"Authorization": "Bearer t"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if strings.Contains(body, secretPath) {
		t.Fatalf("readyz body leaked key path: %s", body)
	}
}

func TestBearerToken(t *testing.T) {
	tests := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"Bearer  abc": "abc",
		"abc":         "",
		"":            "",
		"Basic abc":   "",
	}
	for header, want := range tests {
		if got := bearerToken(header); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestSafeDetail(t *testing.T) {
	if got := safeDetail([]byte("  a\n b  ")); got != "a b" {
		t.Errorf("safeDetail = %q, want %q", got, "a b")
	}
	long := strings.Repeat("z", 400)
	got := safeDetail([]byte(long))
	if !strings.HasSuffix(got, "...") || len([]rune(got)) != 303 {
		t.Errorf("safeDetail truncation = %q (len %d)", got, len([]rune(got)))
	}
}

func TestNewRequestIDUnique(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if len(id) != 32 {
			t.Fatalf("id = %q, want 32 hex chars", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestPrimaryKeyUnresolvedReturnsBadGateway(t *testing.T) {
	primary := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("primary must not be reached without a key")
	})
	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be reached on a missing primary key")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Upstreams.Primary.APIKey = "env:AIPROXY_PROXY_UNSET_PRIMARY"
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if primary.count() != 0 || fallback.count() != 0 {
		t.Fatalf("primary=%d fallback=%d, want 0/0", primary.count(), fallback.count())
	}
}

func TestPrimaryKeyUnresolvedDoesNotForwardClientAuthorization(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "primary-served")
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "fallback-served")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Upstreams.Primary.APIKey = "env:AIPROXY_PROXY_UNSET_PRIMARY"
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`,
		map[string]string{"Authorization": "Bearer client-token"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if primary.count() != 0 || fallback.count() != 0 {
		t.Fatalf("primary=%d fallback=%d, want 0/0", primary.count(), fallback.count())
	}
}

func TestFallbackKeyUnresolvedReturnsBadGateway(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	fallback := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("fallback must not be reached without a key")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Upstreams.Fallback.APIKey = "env:AIPROXY_PROXY_UNSET_FALLBACK"
	srv := newProxy(t, cfg)

	resp, _ := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestModelsUnresolvedKeyDoesNotForwardClientAuthorization(t *testing.T) {
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, `{"object":"list"}`)
	})
	cfg := testConfig(t, "http://primary.test/chat", fallback.server.URL)
	cfg.Upstreams.Fallback.APIKey = "env:AIPROXY_PROXY_UNSET_FALLBACK"
	srv := newProxy(t, cfg)

	resp, _ := getWith(t, srv.URL+"/v1/models", map[string]string{"Authorization": "Bearer client-token"})
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if fallback.count() != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallback.count())
	}
}

func TestModelsNoKeyReturnsBadGateway(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Fallback.APIKey = "env:AIPROXY_PROXY_UNSET_FALLBACK"
	srv := newProxy(t, cfg)

	resp, _ := get(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestModelsUpstreamErrorReturnsBadGateway(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Upstreams.Fallback.ModelsURL = deadURL + "/models"
	srv := newProxy(t, cfg)

	resp, _ := get(t, srv.URL+"/v1/models")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestForwardRejectsInvalidURL(t *testing.T) {
	p, err := New(testConfig(t, "http://primary.test/chat", "http://fallback.test"), discardLogger(), keys.NewResolver("", 0))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	up := config.Upstream{Name: "bad", ChatURL: "://bad", APIKey: "literal:k"}
	if _, err := p.forward(req, up, []byte("{}")); err == nil {
		t.Fatal("expected error for invalid upstream URL")
	}
}

func TestInstrumentRecoversPanic(t *testing.T) {
	p, err := New(testConfig(t, "http://primary.test/chat", "http://fallback.test"), discardLogger(), keys.NewResolver("", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p.instrument(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))
	t.Cleanup(srv.Close)

	resp, _ := get(t, srv.URL)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestRecorderDefaultsStatus(t *testing.T) {
	p, err := New(testConfig(t, "http://primary.test/chat", "http://fallback.test"), discardLogger(), keys.NewResolver("", 0))
	if err != nil {
		t.Fatal(err)
	}
	// Writer that never calls WriteHeader, and one that writes nothing at all.
	h := p.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "x")
	}))
	h2 := p.instrument(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv := httptest.NewServer(h)
	srv2 := httptest.NewServer(h2)
	t.Cleanup(func() { srv.Close(); srv2.Close() })

	if resp, _ := get(t, srv.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp, _ := get(t, srv2.URL); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCopyRequestHeadersFilters(t *testing.T) {
	src := http.Header{}
	src.Set("Authorization", "Bearer x")
	src.Set("Connection", "keep-alive")
	src.Set("Host", "example.test")
	src.Set("Content-Length", "3")
	src.Set("Accept-Encoding", "gzip")
	src.Set("Cookie", "session=1")
	src.Set("X-Forwarded-For", "10.0.0.1")
	src.Set("X-Real-IP", "10.0.0.1")
	src.Set("X-Keep", "1")

	dst := http.Header{}
	copyRequestHeaders(dst, src)

	for _, dropped := range []string{
		"Authorization", "Connection", "Host", "Content-Length", "Accept-Encoding",
		"Cookie", "X-Forwarded-For", "X-Real-IP",
	} {
		if got := dst.Get(dropped); got != "" {
			t.Errorf("%s = %q, want dropped", dropped, got)
		}
	}
	if dst.Get("X-Keep") != "1" {
		t.Errorf("X-Keep = %q, want preserved", dst.Get("X-Keep"))
	}
}

func TestCopyRequestHeadersDropsConnectionNamed(t *testing.T) {
	src := http.Header{}
	src.Set("Connection", "X-Credential, keep-alive")
	src.Set("X-Credential", "secret")
	src.Set("X-Keep", "1")

	dst := http.Header{}
	copyRequestHeaders(dst, src)

	if got := dst.Get("X-Credential"); got != "" {
		t.Errorf("X-Credential = %q, want dropped (named in Connection)", got)
	}
	if dst.Get("X-Keep") != "1" {
		t.Errorf("X-Keep = %q, want preserved", dst.Get("X-Keep"))
	}
}

func TestCopyResponseHeadersDropsConnectionNamed(t *testing.T) {
	src := http.Header{}
	src.Set("Connection", "X-Internal")
	src.Set("X-Internal", "1")
	src.Set("Content-Type", "application/json")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	if got := dst.Get("X-Internal"); got != "" {
		t.Errorf("X-Internal = %q, want dropped (named in Connection)", got)
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want preserved", dst.Get("Content-Type"))
	}
}

func TestCopyResponseHeadersFilters(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Length", "3")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Connection", "close")
	src.Set("Set-Cookie", "session=1")
	src.Set("Content-Type", "application/json")

	dst := http.Header{}
	copyResponseHeaders(dst, src)

	for _, dropped := range []string{"Content-Length", "Transfer-Encoding", "Connection", "Set-Cookie"} {
		if got := dst.Get(dropped); got != "" {
			t.Errorf("%s = %q, want dropped", dropped, got)
		}
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want preserved", dst.Get("Content-Type"))
	}
}

func TestSanitizeRequestID(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"abc-123_X.Y:Z", "abc-123_X.Y:Z"},
		{"", ""},
		{"has space", ""},
		{"bad\nnewline", ""},
		{strings.Repeat("a", 129), ""},
		{strings.Repeat("a", 128), strings.Repeat("a", 128)},
	}
	for _, tt := range tests {
		if got := sanitizeRequestID(tt.in); got != tt.want {
			t.Errorf("sanitizeRequestID(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestUnsafeRequestIDReplaced(t *testing.T) {
	p, err := New(testConfig(t, "http://primary.test/chat", "http://fallback.test"), discardLogger(), keys.NewResolver("", 0))
	if err != nil {
		t.Fatal(err)
	}
	handler := p.instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))

	// Bypass the HTTP client, which would reject the invalid header itself.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	req.Header["X-Request-Id"] = []string{"evil bad"}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("X-Request-ID")
	if got == "" || got == "evil bad" || strings.ContainsAny(got, " \r\n") {
		t.Fatalf("X-Request-ID = %q, want a clean generated id", got)
	}
}

func TestReadyzRequiresAuthWhenTokenSet(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.ClientToken = "sekret"
	srv := newProxy(t, cfg)

	if resp, _ := get(t, srv.URL+"/readyz"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("readyz without token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := getWith(t, srv.URL+"/readyz", map[string]string{"Authorization": "Bearer sekret"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz with token = %d, want 200", resp.StatusCode)
	}
}

func getWith(t *testing.T, url string, headers map[string]string) (*http.Response, string) { //nolint:gocritic // test helper
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

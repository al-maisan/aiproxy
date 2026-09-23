package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/al-maisan/aiproxy/internal/config"
	"github.com/al-maisan/aiproxy/internal/stats"
)

func statsSnapshot(t *testing.T, srvURL string) stats.Snapshot {
	t.Helper()
	resp, body := get(t, srvURL+"/stats")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", resp.StatusCode)
	}
	var snap stats.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode stats: %v\n%s", err, body)
	}
	return snap
}

func requestCountFor(snap stats.Snapshot, upstream, reason string) uint64 {
	var n uint64
	for _, r := range snap.Requests {
		if r.Upstream == upstream && r.Reason == reason {
			n += r.Count
		}
	}
	return n
}

func TestStatsCountsPrimarySuccess(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)

	snap := statsSnapshot(t, srv.URL)
	if got := requestCountFor(snap, stats.UpstreamPrimary, ""); got != 1 {
		t.Fatalf("primary count = %d, want 1", got)
	}
	if len(snap.Upstreams) != 1 || snap.Upstreams[0].Upstream != stats.UpstreamPrimary {
		t.Fatalf("upstreams = %+v", snap.Upstreams)
	}
}

func TestStatsCountsQuotaFallback(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, "quota exceeded")
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)

	snap := statsSnapshot(t, srv.URL)
	if got := requestCountFor(snap, stats.UpstreamFallback, stats.ReasonQuota); got != 1 {
		t.Fatalf("fallback quota count = %d, want 1", got)
	}
	// Both upstreams were attempted, so both have latency series.
	if len(snap.Upstreams) != 2 {
		t.Fatalf("upstreams = %+v, want primary+fallback attempts", snap.Upstreams)
	}
}

func TestStatsCountsUnroutedFallback(t *testing.T) {
	primary := newFake(t, nil)
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"other"}`, nil)

	snap := statsSnapshot(t, srv.URL)
	if got := requestCountFor(snap, stats.UpstreamFallback, stats.ReasonUnrouted); got != 1 {
		t.Fatalf("unrouted fallback count = %d, want 1", got)
	}
}

func TestStatsCountsClientError(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{not json`, nil)

	snap := statsSnapshot(t, srv.URL)
	if got := requestCountFor(snap, stats.UpstreamNone, ""); got != 1 {
		t.Fatalf("client-error count = %d, want 1", got)
	}
	for _, r := range snap.Requests {
		if r.Upstream == stats.UpstreamNone && r.Status != http.StatusBadRequest {
			t.Fatalf("client error status = %d, want 400", r.Status)
		}
	}
}

func TestStatsCountsUnauthorized(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.ClientToken = "sekret"
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil) // no token

	snap := statsSnapshotWithAuth(t, srv.URL, "sekret")
	if got := requestCountFor(snap, stats.UpstreamNone, ""); got != 1 {
		t.Fatalf("unauthorized count = %d, want 1", got)
	}
}

func statsSnapshotWithAuth(t *testing.T, srvURL, token string) stats.Snapshot {
	t.Helper()
	resp, body := getWith(t, srvURL+"/stats", map[string]string{"Authorization": "Bearer " + token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", resp.StatusCode)
	}
	var snap stats.Snapshot
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return snap
}

func TestMetricsEndpoint(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)

	resp, body := get(t, srv.URL+"/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("metrics content-type = %q", ct)
	}
	if !strings.Contains(body, `aiproxy_requests_total{upstream="primary"`) {
		t.Fatalf("metrics missing primary series:\n%s", body)
	}
}

func TestStatsDisabled(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Stats.Disabled = true
	srv := newProxy(t, cfg)

	for _, path := range []string{"/stats", "/metrics"} {
		if resp, _ := get(t, srv.URL+path); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 when disabled", path, resp.StatusCode)
		}
	}
}

func TestStatsCustomBuckets(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.Stats.Buckets = []float64{0.01, 0.02}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("custom buckets rejected: %v", err)
	}

	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "ok")
	})
	cfg.Upstreams.Primary.ChatURL = primary.server.URL + "/chat/completions"
	srv := newProxy(t, cfg)
	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)

	_, body := get(t, srv.URL+"/metrics")
	if !strings.Contains(body, `le="0.01"`) || !strings.Contains(body, `le="0.02"`) {
		t.Fatalf("custom buckets missing from metrics:\n%s", body)
	}
}

func TestStatsKeyErrorDoesNotRecordAttempt(t *testing.T) {
	primary := newFake(t, func(_ http.ResponseWriter, _ int) {
		t.Error("primary must not be reached without a key")
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Upstreams.Primary.APIKey = "env:AIPROXY_STATS_UNSET_PRIMARY"
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)

	snap := statsSnapshot(t, srv.URL)
	// No request was sent, so no latency/attempt series for the primary.
	for _, u := range snap.Upstreams {
		if u.Upstream == stats.UpstreamPrimary {
			t.Fatalf("primary should have no attempts on key error, got %+v", u)
		}
	}
	if got := requestCountFor(snap, stats.UpstreamNone, ""); got != 1 {
		t.Fatalf("unavailable count = %d, want 1", got)
	}
}

func TestStatsStreamDurationMeasuresFullBody(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, "data: x\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)

	snap := statsSnapshot(t, srv.URL)
	if len(snap.Upstreams) != 1 {
		t.Fatalf("upstreams = %+v", snap.Upstreams)
	}
	// Four 50ms chunks => at least ~150ms measured to stream completion.
	if got := snap.Upstreams[0].SumSeconds; got < 0.15 {
		t.Fatalf("streamed duration = %v, want >= 0.15 (measured to completion)", got)
	}
}

func TestStatsRecordsClientAbort(t *testing.T) {
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		// Slow response so the client can abort while the proxy waits.
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"req-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	_, _ = http.DefaultClient.Do(req) // expected to fail with context deadline

	snap := statsSnapshot(t, srv.URL)
	if got := requestCountFor(snap, stats.UpstreamNone, ""); got == 0 {
		t.Fatalf("client abort not recorded: %+v", snap.Requests)
	}
	for _, r := range snap.Requests {
		if r.Outcome != string(stats.ClientAbort) {
			t.Fatalf("outcome = %q, want client_abort", r.Outcome)
		}
	}
	// It must not be misreported as an unavailable/fallback outage.
	if got := requestCountFor(snap, stats.UpstreamFallback, stats.ReasonConnectionError); got != 0 {
		t.Fatalf("client abort recorded as connection_error fallback: %d", got)
	}
}

func TestClientGoneDistinguishesUpstreamTimeout(t *testing.T) {
	upstreamTimeout := &httpError{"net/http: timeout awaiting response headers"}

	// Live request context: an upstream timeout is NOT a client abort.
	live := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	if clientGone(live, upstreamTimeout) {
		t.Error("upstream timeout with a live context reported as clientGone")
	}
	if clientGone(live, context.DeadlineExceeded) {
		t.Error("upstream deadline with a live context reported as clientGone")
	}
	if clientGone(live, nil) {
		t.Error("nil error with a live context reported as clientGone")
	}

	// Cancelled request context (client hung up): treated as client gone.
	cancelled := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	cctx, cancel := context.WithCancel(cancelled.Context())
	cancel()
	cancelled = cancelled.WithContext(cctx)
	if !clientGone(cancelled, context.Canceled) {
		t.Error("cancelled context not reported as clientGone")
	}
	if !clientGone(cancelled, nil) {
		t.Error("cancelled context with nil error not reported as clientGone")
	}
	// A transport error wrapping context.Canceled is also a client abort.
	wrapped := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	wctx, wcancel := context.WithCancel(wrapped.Context())
	wcancel()
	wrapped = wrapped.WithContext(wctx)
	if !clientGone(wrapped, fmt.Errorf("do: %w", context.Canceled)) {
		t.Error("wrapped context.Canceled not reported as clientGone")
	}
}

// httpError is a minimal error that does not wrap context errors, standing in
// for an upstream transport timeout.
type httpError struct{ msg string }

func (e *httpError) Error() string { return e.msg }

func TestStatsUpstreamTimeoutIsNotClientAbort(t *testing.T) {
	// Primary stalls past the upstream header timeout; the client stays
	// connected, so this is an upstream failure (with fallback), not an abort.
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, "too late")
	})
	fallback := newFake(t, func(w http.ResponseWriter, _ int) {
		_, _ = io.WriteString(w, "fallback-ok")
	})
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	cfg.Server.UpstreamHeaderTimeout = config.Duration(100 * time.Millisecond)
	srv := newProxy(t, cfg)

	resp, body := post(t, srv.URL+"/v1/chat/completions", `{"model":"req-model"}`, nil)
	if resp.StatusCode != http.StatusOK || body != "fallback-ok" {
		t.Fatalf("status=%d body=%q, want fallback-ok", resp.StatusCode, body)
	}

	snap := statsSnapshot(t, srv.URL)
	for _, r := range snap.Requests {
		if r.Outcome == string(stats.ClientAbort) {
			t.Fatalf("upstream timeout misrecorded as client_abort: %+v", r)
		}
	}
	if got := requestCountFor(snap, stats.UpstreamFallback, stats.ReasonConnectionError); got != 1 {
		t.Fatalf("fallback connection_error count = %d, want 1", got)
	}
	// The primary attempt must be recorded (a request was actually sent).
	found := false
	for _, u := range snap.Upstreams {
		if u.Upstream == stats.UpstreamPrimary && u.Attempts == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("primary attempt not recorded: %+v", snap.Upstreams)
	}
}

func TestStatsRecordsMidStreamClientAbort(t *testing.T) {
	// The upstream returns 200 and streams slowly; the client disconnects
	// mid-stream. The write to the client fails, which is not a context error,
	// so stream() must signal it explicitly.
	primary := newFake(t, func(w http.ResponseWriter, _ int) {
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			if _, err := io.WriteString(w, "data: chunk\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(40 * time.Millisecond)
		}
	})
	fallback := newFake(t, nil)
	cfg := testConfig(t, primary.server.URL+"/chat/completions", fallback.server.URL)
	srv := newProxy(t, cfg)

	// Raw connection so we can close it after the first bytes arrive.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"req-model"}`
	req := "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\n" +
		"Content-Type: application/json\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	// Read a little (headers + first chunk), then hang up.
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Read(buf)
	_ = conn.Close()

	// Allow the proxy to observe the broken pipe and record it.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := statsSnapshot(t, srv.URL)
		for _, r := range snap.Requests {
			if r.Outcome == string(stats.ClientAbort) {
				return // recorded correctly
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	snap := statsSnapshot(t, srv.URL)
	t.Fatalf("mid-stream abort not recorded as client_abort: %+v", snap.Requests)
}

func TestStatsRequiresAuthWhenTokenSet(t *testing.T) {
	cfg := testConfig(t, "http://primary.test/chat", "http://fallback.test")
	cfg.ClientToken = "sekret"
	srv := newProxy(t, cfg)

	if resp, _ := get(t, srv.URL+"/stats"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stats without token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/metrics"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics without token = %d, want 401", resp.StatusCode)
	}
}

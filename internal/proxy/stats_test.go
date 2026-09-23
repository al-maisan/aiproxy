package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

package stats

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestObserveRequestCountsAndPercent(t *testing.T) {
	c := New()
	c.ObserveRequest(UpstreamPrimary, Success, "", 200)
	c.ObserveRequest(UpstreamPrimary, Success, "", 200)
	c.ObserveRequest(UpstreamFallback, Success, ReasonQuota, 200)
	c.ObserveRequest(UpstreamNone, ClientErr, "", 400)

	snap := c.Snapshot()
	if len(snap.Requests) != 3 {
		t.Fatalf("series = %d, want 3", len(snap.Requests))
	}
	byKey := map[string]RequestStat{}
	for _, r := range snap.Requests {
		byKey[r.Upstream+"/"+r.Outcome+"/"+r.Reason] = r
	}
	if got := byKey["primary/success/"]; got.Count != 2 || got.Status != 200 {
		t.Fatalf("primary series = %+v", got)
	}
	if got := byKey["fallback/success/quota"]; got.Count != 1 {
		t.Fatalf("fallback series = %+v", got)
	}
	if got := byKey["none/client_error/"]; got.Count != 1 {
		t.Fatalf("client-error series = %+v", got)
	}
	// 2 primary / 4 total = 50%.
	if got := byKey["primary/success/"].Percent; got != 50 {
		t.Fatalf("primary percent = %v, want 50", got)
	}
}

func TestObserveLatencyStats(t *testing.T) {
	c := NewWithBuckets([]float64{1, 2, 5})
	for _, d := range []time.Duration{250 * time.Millisecond, 1500 * time.Millisecond, 4 * time.Second} {
		c.ObserveLatency(UpstreamPrimary, d)
	}
	c.ObserveLatency(UpstreamNone, time.Second) // ignored

	snap := c.Snapshot()
	if len(snap.Upstreams) != 1 {
		t.Fatalf("upstreams = %d, want 1", len(snap.Upstreams))
	}
	u := snap.Upstreams[0]
	if u.Upstream != UpstreamPrimary || u.Attempts != 3 {
		t.Fatalf("upstream stat = %+v", u)
	}
	if u.MinSeconds != 0.25 || u.MaxSeconds != 4.0 {
		t.Fatalf("min/max = %v/%v", u.MinSeconds, u.MaxSeconds)
	}
	if got := u.AvgSeconds; got < 1.9 || got > 1.93 {
		t.Fatalf("avg = %v, want ~1.916", got)
	}
	// p50 falls in the [1,2) bucket: rank 1.5, lower 1, 1 of 3 -> interpolated.
	if q := quantileOf(&u, 0.5); q < 1 || q > 2 {
		t.Fatalf("p50 = %v, want within [1,2]", q)
	}
	if q := quantileOf(&u, 0.99); q < 2 || q > 5 {
		t.Fatalf("p99 = %v, want within [2,5]", q)
	}
}

func quantileOf(u *UpstreamStat, q float64) float64 {
	for _, x := range u.Quantiles {
		if x.Q == q {
			return x.Seconds
		}
	}
	return -1
}

func TestPercentServedFromServedNotAttempts(t *testing.T) {
	c := New()
	// One request falls back: primary attempted then fallback served.
	c.ObserveLatency(UpstreamPrimary, 0)
	c.ObserveLatency(UpstreamFallback, 0)
	c.ObserveRequest(UpstreamFallback, Success, ReasonQuota, 200)

	snap := c.Snapshot()
	byName := map[string]UpstreamStat{}
	for _, u := range snap.Upstreams {
		byName[u.Upstream] = u
	}
	// Primary was attempted but served nothing: 0% served.
	if p := byName[UpstreamPrimary].PercentServed; p != 0 {
		t.Fatalf("primary percent_served = %v, want 0", p)
	}
	// Fallback served the only request: 100%.
	if p := byName[UpstreamFallback].PercentServed; p != 100 {
		t.Fatalf("fallback percent_served = %v, want 100", p)
	}
	// Attempts still record the primary attempt.
	if a := byName[UpstreamPrimary].Attempts; a != 1 {
		t.Fatalf("primary attempts = %d, want 1", a)
	}
}

func TestSnapshotIsolatedFromConcurrentObserve(_ *testing.T) {
	// A snapshot must not share the live counts slice with the collector: a
	// concurrent ObserveLatency would otherwise race with rendering. Run with
	// -race to catch a regression.
	c := NewWithBuckets([]float64{1, 2, 5})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				c.ObserveLatency(UpstreamPrimary, time.Millisecond)
			}
		}
	}()

	for i := 0; i < 2000; i++ {
		_ = c.Snapshot()
		_ = c.Prometheus()
	}
	close(stop)
	<-done
}

func TestPrometheusFormat(t *testing.T) {
	c := NewWithBuckets([]float64{1, 5})
	c.ObserveRequest(UpstreamFallback, Success, ReasonUnrouted, 200)
	c.ObserveLatency(UpstreamFallback, 3*time.Second)

	out := c.Prometheus()
	for _, want := range []string{
		"# TYPE aiproxy_uptime_seconds gauge",
		`aiproxy_requests_total{upstream="fallback",outcome="success",reason="unrouted",status="200"} 1`,
		`aiproxy_upstream_attempts_total{upstream="fallback"} 1`,
		`aiproxy_upstream_request_duration_seconds_bucket{upstream="fallback",le="1"} 0`,
		`aiproxy_upstream_request_duration_seconds_bucket{upstream="fallback",le="5"} 1`,
		`aiproxy_upstream_request_duration_seconds_bucket{upstream="fallback",le="+Inf"} 1`,
		`aiproxy_upstream_request_duration_seconds_sum{upstream="fallback"} 3`,
		`aiproxy_upstream_request_duration_seconds_count{upstream="fallback"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Prometheus output missing %q\n%s", want, out)
		}
	}
}

func TestSnapshotJSONShape(t *testing.T) {
	c := New()
	c.ObserveRequest(UpstreamPrimary, Success, "", 200)
	raw, err := json.Marshal(c.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	for _, k := range []string{"uptime_seconds", "requests", "upstreams"} {
		if _, ok := got[k]; !ok {
			t.Errorf("snapshot missing %q: %s", k, raw)
		}
	}
}

func TestEmptySnapshot(t *testing.T) {
	c := New()
	snap := c.Snapshot()
	if len(snap.Requests) != 0 || len(snap.Upstreams) != 0 {
		t.Fatalf("empty snapshot not empty: %+v", snap)
	}
	if !strings.Contains(c.Prometheus(), "aiproxy_uptime_seconds") {
		t.Fatal("empty Prometheus output lacks uptime")
	}
}

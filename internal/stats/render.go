package stats

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Snapshot is a point-in-time view of the collector, used for rendering.
type Snapshot struct {
	UptimeSeconds float64        `json:"uptime_seconds"`
	Requests      []RequestStat  `json:"requests"`
	Upstreams     []UpstreamStat `json:"upstreams"`
}

// RequestStat is one (upstream,outcome,reason,status) series.
type RequestStat struct {
	Upstream string  `json:"upstream"`
	Outcome  string  `json:"outcome"`
	Reason   string  `json:"reason,omitempty"`
	Status   int     `json:"status"`
	Count    uint64  `json:"count"`
	Percent  float64 `json:"percent"`
}

// UpstreamStat summarises one upstream's latency and share of requests.
type UpstreamStat struct {
	Upstream   string     `json:"upstream"`
	Count      uint64     `json:"count"`
	Percent    float64    `json:"percent"`
	SumSeconds float64    `json:"sum_seconds"`
	AvgSeconds float64    `json:"avg_seconds"`
	MinSeconds float64    `json:"min_seconds"`
	MaxSeconds float64    `json:"max_seconds"`
	Quantiles  []Quantile `json:"quantiles"`
}

// Quantile is a named latency estimate.
type Quantile struct {
	Q       float64 `json:"q"`
	Seconds float64 `json:"seconds"`
}

// Snapshot returns a consistent copy of the collected statistics.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	var total uint64
	for _, n := range c.series {
		total += n
	}

	snap := Snapshot{UptimeSeconds: time.Since(c.start).Seconds()}

	keys := make([]seriesKey, 0, len(c.series))
	for k := range c.series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.upstream != b.upstream {
			return a.upstream < b.upstream
		}
		if a.outcome != b.outcome {
			return a.outcome < b.outcome
		}
		if a.reason != b.reason {
			return a.reason < b.reason
		}
		return a.status < b.status
	})
	for _, k := range keys {
		n := c.series[k]
		snap.Requests = append(snap.Requests, RequestStat{
			Upstream: k.upstream, Outcome: string(k.outcome), Reason: k.reason,
			Status: k.status, Count: n, Percent: percent(n, total),
		})
	}

	ups := make([]string, 0, len(c.hists))
	for u := range c.hists {
		ups = append(ups, u)
	}
	sort.Strings(ups)
	for _, u := range ups {
		h := c.hists[u]
		st := UpstreamStat{
			Upstream:   u,
			Count:      h.count,
			Percent:    percent(h.count, total),
			SumSeconds: h.sum,
		}
		if h.count > 0 {
			st.AvgSeconds = h.sum / float64(h.count)
			st.MinSeconds = h.min
			st.MaxSeconds = h.max
		}
		for _, q := range []float64{0.5, 0.9, 0.99} {
			st.Quantiles = append(st.Quantiles, Quantile{Q: q, Seconds: h.quantile(q)})
		}
		snap.Upstreams = append(snap.Upstreams, st)
	}
	return snap
}

func percent(part, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(part) / float64(total)
}

// Prometheus renders the collector in Prometheus text exposition format
// (version 0.0.4).
func (c *Collector) Prometheus() string {
	snap := c.Snapshot()
	var b strings.Builder

	b.WriteString("# HELP aiproxy_uptime_seconds Seconds since the process started.\n")
	b.WriteString("# TYPE aiproxy_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "aiproxy_uptime_seconds %g\n", snap.UptimeSeconds)

	b.WriteString("# HELP aiproxy_requests_total Chat requests by serving upstream, outcome, reason and status.\n")
	b.WriteString("# TYPE aiproxy_requests_total counter\n")
	for _, r := range snap.Requests {
		fmt.Fprintf(&b, "aiproxy_requests_total{upstream=%q,outcome=%q,reason=%q,status=\"%d\"} %d\n",
			r.Upstream, r.Outcome, r.Reason, r.Status, r.Count)
	}

	b.WriteString("# HELP aiproxy_upstream_attempts_total Requests attempted per upstream.\n")
	b.WriteString("# TYPE aiproxy_upstream_attempts_total counter\n")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(&b, "aiproxy_upstream_attempts_total{upstream=%q} %d\n", u.Upstream, u.Count)
	}

	b.WriteString("# HELP aiproxy_upstream_request_duration_seconds Upstream attempt latency.\n")
	b.WriteString("# TYPE aiproxy_upstream_request_duration_seconds histogram\n")
	c.mu.Lock()
	ups := make([]string, 0, len(c.hists))
	for u := range c.hists {
		ups = append(ups, u)
	}
	sort.Strings(ups)
	for _, u := range ups {
		h := c.hists[u]
		cum := h.cumulative()
		for i, ub := range h.bounds {
			fmt.Fprintf(&b, "aiproxy_upstream_request_duration_seconds_bucket{upstream=%q,le=%q} %d\n",
				u, strconv.FormatFloat(ub, 'g', -1, 64), cum[i])
		}
		fmt.Fprintf(&b, "aiproxy_upstream_request_duration_seconds_bucket{upstream=%q,le=\"+Inf\"} %d\n",
			u, cum[len(cum)-1])
		fmt.Fprintf(&b, "aiproxy_upstream_request_duration_seconds_sum{upstream=%q} %g\n", u, h.sum)
		fmt.Fprintf(&b, "aiproxy_upstream_request_duration_seconds_count{upstream=%q} %d\n", u, h.count)
	}
	c.mu.Unlock()
	return b.String()
}

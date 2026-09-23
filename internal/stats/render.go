package stats

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
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

// UpstreamStat summarises one upstream: how many requests it served (and its
// share of all requests) and how long its attempts took. Attempts may exceed
// served requests because a request that falls back is attempted at both
// upstreams.
type UpstreamStat struct {
	Upstream      string     `json:"upstream"`
	Attempts      uint64     `json:"attempts"`
	Served        uint64     `json:"served"`
	PercentServed float64    `json:"percent_served"`
	SumSeconds    float64    `json:"sum_seconds"`
	AvgSeconds    float64    `json:"avg_seconds"`
	MinSeconds    float64    `json:"min_seconds"`
	MaxSeconds    float64    `json:"max_seconds"`
	Quantiles     []Quantile `json:"quantiles"`
}

// Quantile is a named latency estimate.
type Quantile struct {
	Q       float64 `json:"q"`
	Seconds float64 `json:"seconds"`
}

// Snapshot returns a consistent copy of the collected statistics.
func (c *Collector) Snapshot() Snapshot {
	v := c.snapshot()

	snap := Snapshot{UptimeSeconds: v.uptime}
	for _, s := range v.series {
		snap.Requests = append(snap.Requests, RequestStat{
			Upstream: s.key.upstream, Outcome: string(s.key.outcome), Reason: s.key.reason,
			Status: s.key.status, Count: s.count, Percent: percent(s.count, v.total),
		})
	}

	ups := make([]string, 0, len(v.hists))
	for u := range v.hists {
		ups = append(ups, u)
	}
	sort.Strings(ups)
	for _, u := range ups {
		h := v.hists[u]
		st := UpstreamStat{
			Upstream:      u,
			Attempts:      h.count,
			Served:        v.served[u],
			PercentServed: percent(v.served[u], v.total),
			SumSeconds:    h.sum,
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
	v := c.snapshot()
	var b strings.Builder

	b.WriteString("# HELP aiproxy_uptime_seconds Seconds since the process started.\n")
	b.WriteString("# TYPE aiproxy_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "aiproxy_uptime_seconds %g\n", v.uptime)

	b.WriteString("# HELP aiproxy_requests_total Chat requests by serving upstream, outcome, reason and status.\n")
	b.WriteString("# TYPE aiproxy_requests_total counter\n")
	for _, s := range v.series {
		fmt.Fprintf(&b, "aiproxy_requests_total{upstream=%q,outcome=%q,reason=%q,status=\"%d\"} %d\n",
			s.key.upstream, s.key.outcome, s.key.reason, s.key.status, s.count)
	}

	b.WriteString("# HELP aiproxy_upstream_attempts_total Requests attempted per upstream.\n")
	b.WriteString("# TYPE aiproxy_upstream_attempts_total counter\n")
	ups := make([]string, 0, len(v.hists))
	for u := range v.hists {
		ups = append(ups, u)
	}
	sort.Strings(ups)
	for _, u := range ups {
		fmt.Fprintf(&b, "aiproxy_upstream_attempts_total{upstream=%q} %d\n", u, v.hists[u].count)
	}

	b.WriteString("# HELP aiproxy_upstream_request_duration_seconds Upstream attempt latency.\n")
	b.WriteString("# TYPE aiproxy_upstream_request_duration_seconds histogram\n")
	for _, u := range ups {
		h := v.hists[u]
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
	return b.String()
}

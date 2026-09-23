// Package stats collects in-process request statistics for aiproxy: which
// upstream served each proxied chat request, why fallback happened, and how
// long upstream attempts took. Results are rendered as Prometheus text or JSON.
package stats

import (
	"sort"
	"sync"
	"time"
)

// Outcome classifies how a request ended.
type Outcome string

const (
	// Success means an upstream returned a 2xx response that streamed fully.
	Success Outcome = "success"
	// UpstreamErr means an upstream returned an error response, or its response
	// stream broke before completing.
	UpstreamErr Outcome = "upstream_error"
	// ClientErr means the request was rejected before any upstream was called.
	ClientErr Outcome = "client_error"
	// ClientAbort means the client disconnected before the request completed.
	ClientAbort Outcome = "client_abort"
	// Unavailable means no upstream could serve the request (missing key,
	// connection failure, or encoding failure).
	Unavailable Outcome = "unavailable"
)

// Upstream identifiers used as metric labels. UpstreamNone means no upstream
// response was returned to the client.
const (
	UpstreamPrimary  = "primary"
	UpstreamFallback = "fallback"
	UpstreamNone     = "none"
)

// Reasons a request was served by the fallback upstream.
const (
	ReasonUnrouted        = "unrouted"
	ReasonConnectionError = "connection_error"
	ReasonQuota           = "quota"
)

// DefaultBuckets are latency histogram upper bounds, in seconds.
var DefaultBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300}

type seriesKey struct {
	upstream string
	outcome  Outcome
	reason   string
	status   int
}

// histogram is a fixed-bucket latency histogram. counts holds one entry per
// bucket plus a trailing +Inf bucket; entries are non-cumulative.
type histogram struct {
	bounds []float64
	counts []uint64
	sum    float64
	count  uint64
	min    float64
	max    float64
}

func newHistogram(bounds []float64) *histogram {
	return &histogram{bounds: bounds, counts: make([]uint64, len(bounds)+1)}
}

func (h *histogram) observe(v float64) {
	h.count++
	h.sum += v
	if h.count == 1 || v < h.min {
		h.min = v
	}
	if h.count == 1 || v > h.max {
		h.max = v
	}
	for i, ub := range h.bounds {
		if v <= ub {
			h.counts[i]++
			return
		}
	}
	h.counts[len(h.bounds)]++
}

// cumulative returns the bucket counts as Prometheus-style cumulative counts,
// including the trailing +Inf bucket.
func (h *histogram) cumulative() []uint64 {
	out := make([]uint64, len(h.counts))
	var running uint64
	for i, c := range h.counts {
		running += c
		out[i] = running
	}
	return out
}

// quantile estimates the q-quantile (0..1) by linear interpolation within the
// bucket containing the rank, following Prometheus histogram_quantile.
func (h *histogram) quantile(q float64) float64 {
	if h.count == 0 {
		return 0
	}
	rank := q * float64(h.count)
	var cum, prev uint64
	lower := 0.0
	for i, ub := range h.bounds {
		cum += h.counts[i]
		if float64(cum) >= rank {
			if cum == prev {
				return ub
			}
			frac := (rank - float64(prev)) / float64(cum-prev)
			return lower + frac*(ub-lower)
		}
		prev = cum
		lower = ub
	}
	return h.max
}

// Collector accumulates request statistics. All methods are safe for
// concurrent use.
type Collector struct {
	mu      sync.Mutex
	start   time.Time
	buckets []float64
	series  map[seriesKey]uint64
	hists   map[string]*histogram
}

// New returns a Collector using DefaultBuckets.
func New() *Collector {
	return NewWithBuckets(DefaultBuckets)
}

// NewWithBuckets returns a Collector using the supplied latency buckets
// (seconds), which are copied and sorted.
func NewWithBuckets(bounds []float64) *Collector {
	b := append([]float64(nil), bounds...)
	sort.Float64s(b)
	return &Collector{
		start:   time.Now(),
		buckets: b,
		series:  make(map[seriesKey]uint64),
		hists:   make(map[string]*histogram),
	}
}

// ObserveRequest records the final outcome of one proxied chat request.
// upstream is the upstream whose response was returned (UpstreamNone if none);
// outcome is the result; reason explains a fallback ("" otherwise); status is
// the HTTP status returned to the client.
func (c *Collector) ObserveRequest(upstream string, outcome Outcome, reason string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.series[seriesKey{upstream: upstream, outcome: outcome, reason: reason, status: status}]++
}

// ObserveLatency records the duration of one upstream attempt. A request that
// falls back produces two observations: one per attempted upstream. It must
// only be called when a request was actually sent (not, for example, when key
// resolution failed).
func (c *Collector) ObserveLatency(upstream string, d time.Duration) {
	if upstream == "" || upstream == UpstreamNone {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.hists[upstream]
	if h == nil {
		h = newHistogram(c.buckets)
		c.hists[upstream] = h
	}
	h.observe(d.Seconds())
}

// view is a consistent point-in-time copy of the collector taken under a single
// lock, so request counters and latency histograms cannot disagree within one
// render.
type view struct {
	uptime float64
	total  uint64
	served map[string]uint64
	series []seriesSample
	hists  map[string]histogram
}

type seriesSample struct {
	key   seriesKey
	count uint64
}

// snapshot copies the collector state under one lock.
func (c *Collector) snapshot() view {
	c.mu.Lock()
	defer c.mu.Unlock()

	v := view{
		uptime: time.Since(c.start).Seconds(),
		served: make(map[string]uint64),
		hists:  make(map[string]histogram, len(c.hists)),
	}
	v.series = make([]seriesSample, 0, len(c.series))
	for k, n := range c.series {
		v.total += n
		v.served[k.upstream] += n
		v.series = append(v.series, seriesSample{key: k, count: n})
	}
	sort.Slice(v.series, func(i, j int) bool {
		a, b := v.series[i].key, v.series[j].key
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
	for u, h := range c.hists {
		v.hists[u] = *h
	}
	return v
}

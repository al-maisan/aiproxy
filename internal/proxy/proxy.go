// Package proxy implements the request routing and quota-based failover logic.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/al-maisan/aiproxy/internal/config"
	"github.com/al-maisan/aiproxy/internal/keys"
	"github.com/al-maisan/aiproxy/internal/quota"
	"github.com/al-maisan/aiproxy/internal/stats"
)

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// hopByHopHeaders are removed in both directions per RFC 7230.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

// requestHeaderDenylist drops ambient client credentials and forwarding
// headers before a request reaches an upstream.
var requestHeaderDenylist = map[string]struct{}{
	"cookie":            {},
	"cookie2":           {},
	"set-cookie":        {},
	"forwarded":         {},
	"x-forwarded-for":   {},
	"x-forwarded-host":  {},
	"x-forwarded-proto": {},
	"x-real-ip":         {},
}

// responseHeaderDenylist drops upstream headers that must not reach the client.
var responseHeaderDenylist = map[string]struct{}{
	"set-cookie": {},
}

// errKeyUnavailable marks a failure to resolve an upstream API key. It is
// distinct from a transport error so callers do not trigger a fallback: a
// missing key is a configuration problem, not an unreachable upstream.
var errKeyUnavailable = errors.New("api key unavailable")

// Proxy is an http.Handler that prefers the primary upstream for routed models
// and transparently falls back to the fallback upstream on quota exhaustion.
type Proxy struct {
	cfg    *config.Config
	log    *slog.Logger
	keys   *keys.Resolver
	quota  *quota.Detector
	client *http.Client
	routes map[string]config.Route
	strip  map[string]struct{}
	stats  *stats.Collector

	// readyMu guards readyState, the last observed readiness per upstream.
	// The state is tracked so /readyz can log transitions instead of logging
	// on every poll.
	readyMu    sync.Mutex
	readyState map[string]bool
}

// New constructs a Proxy from config. It does not perform any I/O.
func New(cfg *config.Config, log *slog.Logger, kr *keys.Resolver) (*Proxy, error) {
	q, err := quota.New(cfg.Quota.Statuses, cfg.Quota.Patterns)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: cfg.Server.DialTimeout.Std(), KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       cfg.Server.IdleConnTimeout.Std(),
		ResponseHeaderTimeout: cfg.Server.UpstreamHeaderTimeout.Std(),
		ExpectContinueTimeout: time.Second,
	}

	routes := make(map[string]config.Route, len(cfg.Routes))
	for _, r := range cfg.Routes {
		routes[r.Requested] = r
	}
	strip := make(map[string]struct{}, len(cfg.StripFields))
	for _, f := range cfg.StripFields {
		strip[f] = struct{}{}
	}

	collector := stats.New()
	if len(cfg.Stats.Buckets) > 0 {
		collector = stats.NewWithBuckets(cfg.Stats.Buckets)
	}

	return &Proxy{
		cfg:        cfg,
		log:        log,
		keys:       kr,
		quota:      q,
		client:     &http.Client{Transport: transport},
		routes:     routes,
		strip:      strip,
		stats:      collector,
		readyState: make(map[string]bool),
	}, nil
}

// Handler returns the root HTTP handler.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", p.handleHealth)
	mux.Handle("GET /readyz", p.instrument(p.authenticate(http.HandlerFunc(p.handleReady))))
	for _, path := range []string{"/v1/chat/completions", "/chat/completions"} {
		mux.Handle("POST "+path, p.instrument(p.authenticateWith(http.HandlerFunc(p.handleChat), func() {
			p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusUnauthorized)
		})))
	}
	for _, path := range []string{"/v1/models", "/models"} {
		mux.Handle("GET "+path, p.instrument(p.authenticate(http.HandlerFunc(p.handleModels))))
	}
	if !p.cfg.Stats.Disabled {
		// Stats are operator-facing, so they require the client token when one
		// is configured, like the other proxied routes.
		mux.Handle("GET /stats", p.instrument(p.authenticate(http.HandlerFunc(p.handleStats))))
		mux.Handle("GET /metrics", p.instrument(p.authenticate(http.HandlerFunc(p.handleMetrics))))
	}
	return mux
}

func (p *Proxy) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

func (p *Proxy) handleReady(w http.ResponseWriter, _ *http.Request) {
	type check struct {
		Upstream string `json:"upstream"`
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
	}
	checks := make([]check, 0, 2)
	ready := true
	for _, u := range []struct {
		role, name string
		up         config.Upstream
	}{
		{"primary", p.cfg.Upstreams.Primary.Name, p.cfg.Upstreams.Primary},
		{"fallback", p.cfg.Upstreams.Fallback.Name, p.cfg.Upstreams.Fallback},
	} {
		c := check{Upstream: u.name, OK: true}
		if _, err := p.keys.Resolve(u.up.APIKey); err != nil {
			// Keep the diagnostic server-side: it may name local paths or
			// environment variables that should not be disclosed to clients.
			c.OK = false
			c.Error = "api key unavailable"
			ready = false
			p.noteReadiness(u.role, u.name, false, err)
		} else {
			p.noteReadiness(u.role, u.name, true, nil)
		}
		checks = append(checks, c)
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

func (p *Proxy) handleStats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, p.stats.Snapshot())
}

func (p *Proxy) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, p.stats.Prometheus())
}

// noteReadiness records an upstream's readiness and logs it only when the state
// changes, so a health checker polling a misconfigured deployment does not
// flood the logs. State is keyed by role (primary/fallback), not the
// operator-supplied name, which the two upstreams may share.
func (p *Proxy) noteReadiness(role, name string, ok bool, err error) {
	p.readyMu.Lock()
	prev, seen := p.readyState[role]
	p.readyState[role] = ok
	p.readyMu.Unlock()

	switch {
	case !seen && ok:
		// First successful check: nothing to report.
	case !seen || prev != ok:
		if ok {
			p.log.Info("upstream key available again", "upstream", name)
		} else {
			p.log.Warn("upstream key unavailable", "upstream", name, "err", err)
		}
	}
}

func (p *Proxy) handleChat(w http.ResponseWriter, r *http.Request) {
	// Bound the time a client may take to deliver the body; ReadHeaderTimeout
	// only covers headers. Cleared afterwards so long SSE responses are
	// unaffected.
	if d := p.cfg.Server.BodyReadTimeout.Std(); d > 0 {
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(d)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			p.log.Debug("could not set body read deadline", "err", err)
		}
		defer func() { _ = http.NewResponseController(w).SetReadDeadline(time.Time{}) }()
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, p.cfg.Server.MaxBodyBytes+1))
	_ = r.Body.Close()
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusRequestTimeout)
			p.writeError(w, http.StatusRequestTimeout, "request body read timed out")
			return
		}
		p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusBadRequest)
		p.writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if int64(len(body)) > p.cfg.Server.MaxBodyBytes {
		p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusRequestEntityTooLarge)
		p.writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}

	var payload map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusBadRequest)
		p.writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusBadRequest)
		p.writeError(w, http.StatusBadRequest, "unexpected data after JSON request body")
		return
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientErr, "", http.StatusBadRequest)
		p.writeError(w, http.StatusBadRequest, "missing model")
		return
	}

	log := p.log.With("request_id", requestID(r.Context()), "model", model)

	route, routed := p.routes[model]
	if !routed {
		log.Debug("no route for model; using fallback", "upstream", p.cfg.Upstreams.Fallback.Name)
		p.forwardFallback(w, r, body, log, stats.ReasonUnrouted)
		return
	}

	// The fallback normally receives the client body verbatim (clients speak the
	// fallback's model ids); only rewrite it when the route maps a different id.
	fallbackBody := body
	if route.FallbackModel() != model {
		fallbackBody, err = cloneWithModel(payload, route.FallbackModel(), nil)
		if err != nil {
			log.Error("failed to encode fallback request", "err", err)
			p.stats.ObserveRequest(stats.UpstreamNone, stats.Unavailable, "", http.StatusInternalServerError)
			p.writeError(w, http.StatusInternalServerError, "failed to encode request")
			return
		}
	}

	primaryBody, err := p.encodePrimary(payload, route.Primary)
	if err != nil {
		log.Error("failed to encode primary request", "err", err)
		p.stats.ObserveRequest(stats.UpstreamNone, stats.Unavailable, "", http.StatusInternalServerError)
		p.writeError(w, http.StatusInternalServerError, "failed to encode request")
		return
	}

	primaryStart := time.Now()
	resp, err := p.forward(r, p.cfg.Upstreams.Primary, primaryBody)
	if err != nil {
		if clientGone(r, err) {
			log.Info("client aborted request", "upstream", p.cfg.Upstreams.Primary.Name)
			p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientAbort, "", statusClientClosedRequest)
			return
		}
		if errors.Is(err, errKeyUnavailable) {
			log.Error("primary api key unavailable", "upstream", p.cfg.Upstreams.Primary.Name, "err", err)
			p.stats.ObserveRequest(stats.UpstreamNone, stats.Unavailable, "", http.StatusBadGateway)
			p.writeError(w, http.StatusBadGateway, "primary upstream api key unavailable")
			return
		}
		p.stats.ObserveLatency(stats.UpstreamPrimary, time.Since(primaryStart))
		if !p.cfg.FallbackOnConnErr() {
			log.Error("primary request failed", "upstream", p.cfg.Upstreams.Primary.Name, "err", err)
			p.stats.ObserveRequest(stats.UpstreamNone, stats.Unavailable, "", http.StatusBadGateway)
			p.writeError(w, http.StatusBadGateway, "primary upstream request failed")
			return
		}
		log.Warn("primary unreachable; falling back",
			"upstream", p.cfg.Upstreams.Primary.Name, "fallback", p.cfg.Upstreams.Fallback.Name, "err", err)
		p.forwardFallback(w, r, fallbackBody, log, stats.ReasonConnectionError)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Debug("served by primary", "upstream", p.cfg.Upstreams.Primary.Name, "status", resp.StatusCode)
		p.finishStream(w, r, resp, primaryStart, stats.UpstreamPrimary, "", log)
		return
	}

	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if reason, ok := p.quota.Match(resp.StatusCode, errBody); ok {
		// Log which configured rule matched (status code or pattern) at Warn:
		// it is leak-free and answers "was this really quota?" without dropping
		// to Debug. The raw body stays at Debug, since a provider 400 may echo
		// request content.
		log.Warn("primary quota reached; falling back",
			"upstream", p.cfg.Upstreams.Primary.Name,
			"status", resp.StatusCode,
			"fallback", p.cfg.Upstreams.Fallback.Name,
			"matched", reason.String())
		log.Debug("primary quota response detail", "detail", safeDetail(errBody))
		p.stats.ObserveLatency(stats.UpstreamPrimary, time.Since(primaryStart))
		p.forwardFallback(w, r, fallbackBody, log, stats.ReasonQuota)
		return
	}

	log.Warn("primary error; passing through", "upstream", p.cfg.Upstreams.Primary.Name, "status", resp.StatusCode)
	p.stats.ObserveLatency(stats.UpstreamPrimary, time.Since(primaryStart))
	if clientGone(r, nil) {
		p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientAbort, "", statusClientClosedRequest)
		return
	}
	p.stats.ObserveRequest(stats.UpstreamPrimary, stats.UpstreamErr, "", resp.StatusCode)
	writeUpstream(w, resp, errBody)
}

// finishStream streams resp to the client, then records the attempt latency and
// the final outcome. Latency is recorded after the body has been copied, so a
// long SSE response is timed to completion rather than to first byte. The
// outcome is ClientAbort if the client disconnected (its context is cancelled
// or the write to it failed), UpstreamErr if the upstream body failed, and
// Success only when the body completed.
func (p *Proxy) finishStream(w http.ResponseWriter, r *http.Request, resp *http.Response, start time.Time, upstream, reason string, log *slog.Logger) {
	err := p.stream(w, resp)
	p.stats.ObserveLatency(upstream, time.Since(start))

	status := resp.StatusCode
	ok := status >= 200 && status < 300
	switch {
	case err == nil && ok:
		p.stats.ObserveRequest(upstream, stats.Success, reason, status)
	case errors.Is(err, errClientWrite) || (err != nil && clientGone(r, err)):
		log.Info("client aborted while streaming", "upstream", upstream)
		p.stats.ObserveRequest(upstream, stats.ClientAbort, reason, status)
	default:
		if err != nil {
			log.Error("upstream stream failed", "upstream", upstream, "err", err)
		}
		p.stats.ObserveRequest(upstream, stats.UpstreamErr, reason, status)
	}
}

func (p *Proxy) forwardFallback(w http.ResponseWriter, r *http.Request, body []byte, log *slog.Logger, reason string) {
	start := time.Now()
	resp, err := p.forward(r, p.cfg.Upstreams.Fallback, body)
	if err != nil {
		if clientGone(r, err) {
			log.Info("client aborted request", "upstream", p.cfg.Upstreams.Fallback.Name, "reason", reason)
			p.stats.ObserveRequest(stats.UpstreamNone, stats.ClientAbort, reason, statusClientClosedRequest)
			return
		}
		if errors.Is(err, errKeyUnavailable) {
			log.Error("fallback api key unavailable", "upstream", p.cfg.Upstreams.Fallback.Name, "reason", reason, "err", err)
			p.stats.ObserveRequest(stats.UpstreamNone, stats.Unavailable, reason, http.StatusBadGateway)
			p.writeError(w, http.StatusBadGateway, "fallback upstream api key unavailable")
			return
		}
		p.stats.ObserveLatency(stats.UpstreamFallback, time.Since(start))
		log.Error("fallback request failed", "upstream", p.cfg.Upstreams.Fallback.Name, "reason", reason, "err", err)
		p.stats.ObserveRequest(stats.UpstreamNone, stats.Unavailable, reason, http.StatusBadGateway)
		p.writeError(w, http.StatusBadGateway, "fallback upstream request failed")
		return
	}
	log.Info("served by fallback", "upstream", p.cfg.Upstreams.Fallback.Name, "status", resp.StatusCode, "reason", reason)
	defer func() { _ = resp.Body.Close() }()
	p.finishStream(w, r, resp, start, stats.UpstreamFallback, reason, log)
}

func (p *Proxy) handleModels(w http.ResponseWriter, r *http.Request) {
	up := p.cfg.Upstreams.Fallback
	if strings.TrimSpace(up.ModelsURL) == "" {
		p.writeError(w, http.StatusNotFound, "models endpoint not configured")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, up.ModelsURL, http.NoBody)
	if err != nil {
		p.writeError(w, http.StatusInternalServerError, "failed to build models request")
		return
	}
	key, err := p.keys.Resolve(up.APIKey)
	if err != nil {
		p.writeError(w, http.StatusBadGateway, "no API key for models upstream")
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := p.client.Do(req)
	if err != nil {
		p.writeError(w, http.StatusBadGateway, "models upstream request failed")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_ = p.stream(w, resp)
}

// encodePrimary clones the payload for the primary upstream, dropping
// fallback-only fields and swapping in the primary model id.
func (p *Proxy) encodePrimary(payload map[string]any, model string) ([]byte, error) {
	return cloneWithModel(payload, model, p.strip)
}

// cloneWithModel returns a copy of payload with the model id set and any keys
// in strip removed. Values are re-encoded as-is; callers must have decoded the
// payload with UseNumber so large integers keep full precision.
func cloneWithModel(payload map[string]any, model string, strip map[string]struct{}) ([]byte, error) {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if _, drop := strip[k]; drop {
			continue
		}
		out[k] = v
	}
	out["model"] = model
	return json.Marshal(out)
}

func (p *Proxy) forward(clientReq *http.Request, up config.Upstream, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(clientReq.Context(), http.MethodPost, up.ChatURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, clientReq.Header)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")

	key, err := p.keys.Resolve(up.APIKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errKeyUnavailable, up.Name, err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return p.client.Do(req)
}

// errClientWrite marks a failure to write to the client's response. It is
// distinct from an upstream read error: the client is gone, not the upstream.
var errClientWrite = errors.New("client write failed")

// stream copies the upstream response body to the client. It returns nil only
// when the body was fully consumed; a failed read is returned as an upstream
// error, and a failed write is wrapped in errClientWrite.
func (p *Proxy) stream(w http.ResponseWriter, resp *http.Response) error {
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return fmt.Errorf("%w: %w", errClientWrite, werr)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func writeUpstream(w http.ResponseWriter, resp *http.Response, body []byte) {
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (p *Proxy) writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": message, "type": "aiproxy_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// statusClientClosedRequest is the non-standard 499 status used to classify a
// request abandoned by the client.
const statusClientClosedRequest = 499

// clientGone reports whether err (or the request context) indicates the client
// disconnected, as opposed to an upstream failure. The server cancels the
// request context when the client hangs up; an upstream timeout, by contrast,
// leaves the context live and must not be treated as a client abort. err may be
// nil, in which case only the context is consulted.
func clientGone(r *http.Request, err error) bool {
	if r.Context().Err() == nil {
		return false
	}
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (p *Proxy) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := sanitizeRequestID(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)

		rec := &recorder{ResponseWriter: w}
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)

		defer func() {
			if v := recover(); v != nil {
				p.log.Error("recovered from panic", "request_id", id, "panic", fmt.Sprint(v))
				if rec.status == 0 {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
				}
			}
			p.log.Info("request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.statusCode(),
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()

		next.ServeHTTP(rec, r.WithContext(ctx))
	})
}

func (p *Proxy) authenticate(next http.Handler) http.Handler {
	return p.authenticateWith(next, nil)
}

// authenticateWith wraps next with authentication. If onReject is non-nil it is
// called before the 401 is written (used to record unauthorized attempts).
func (p *Proxy) authenticateWith(next http.Handler, onReject func()) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.authorized(r) {
			if onReject != nil {
				onReject()
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="aiproxy"`)
			p.writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorized reports whether the request carries valid credentials. It is true
// for every request when no client_token is configured.
func (p *Proxy) authorized(r *http.Request) bool {
	if p.cfg.ClientToken == "" {
		return true
	}
	// Compare fixed-size digests to avoid a length-dependent branch.
	got := sha256.Sum256([]byte(bearerToken(r.Header.Get("Authorization"))))
	want := sha256.Sum256([]byte(p.cfg.ClientToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter.
func (r *recorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *recorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func copyRequestHeaders(dst, src http.Header) {
	named := connectionNamedHeaders(src)
	for k, vs := range src {
		lk := strings.ToLower(k)
		if _, hop := hopByHopHeaders[lk]; hop {
			continue
		}
		if _, isNamed := named[lk]; isNamed {
			continue
		}
		if _, denied := requestHeaderDenylist[lk]; denied {
			continue
		}
		switch lk {
		case "host", "content-length", "authorization", "accept-encoding":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	named := connectionNamedHeaders(src)
	for k, vs := range src {
		lk := strings.ToLower(k)
		if _, hop := hopByHopHeaders[lk]; hop {
			continue
		}
		if _, isNamed := named[lk]; isNamed {
			continue
		}
		if _, denied := responseHeaderDenylist[lk]; denied {
			continue
		}
		if lk == "content-length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// connectionNamedHeaders returns the lower-cased names listed in the
// Connection header, which RFC 7230 §6.1 makes hop-by-hop. It returns nil when
// there is no Connection header, avoiding an allocation on the common path.
func connectionNamedHeaders(h http.Header) map[string]struct{} {
	values := h.Values("Connection")
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{})
	for _, v := range values {
		for _, name := range strings.Split(v, ",") {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				out[name] = struct{}{}
			}
		}
	}
	return out
}

func requestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRequestID).(string)
	return v
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// sanitizeRequestID accepts a caller-supplied correlation id only if it is
// short and made of a conservative character set; otherwise it returns "" so a
// fresh id is generated.
func sanitizeRequestID(id string) string {
	if id == "" || len(id) > 128 {
		return ""
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ':':
		default:
			return ""
		}
	}
	return id
}

func safeDetail(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "..."
	}
	return s
}

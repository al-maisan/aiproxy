// Package quota classifies upstream HTTP errors as quota exhaustion.
package quota

import (
	"fmt"
	"regexp"
	"strconv"
)

// Detector decides whether an upstream response indicates exhausted quota.
type Detector struct {
	statuses map[int]struct{}
	patterns []*regexp.Regexp
}

// Reason identifies the rule that classified a response as quota exhaustion. It
// names only the configured status or pattern — never response content — so it
// is safe to log at any level.
type Reason struct {
	Source string // "status" or "pattern"
	Value  string // the status code, or the pattern that matched
}

// String renders the reason as "source=value".
func (r Reason) String() string {
	if r.Source == "" {
		return "unknown"
	}
	return r.Source + "=" + r.Value
}

// New compiles a detector from a set of HTTP status codes and body patterns.
func New(statuses []int, patterns []string) (*Detector, error) {
	d := &Detector{
		statuses: make(map[int]struct{}, len(statuses)),
		patterns: make([]*regexp.Regexp, 0, len(patterns)),
	}
	for _, s := range statuses {
		if s < 100 || s > 599 {
			return nil, fmt.Errorf("invalid quota status code %d", s)
		}
		d.statuses[s] = struct{}{}
	}
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("quota pattern %d: %w", i, err)
		}
		d.patterns = append(d.patterns, re)
	}
	return d, nil
}

// Match reports whether the status/body combination represents exhausted quota
// and, if so, which configured rule matched. A configured status matches
// regardless of body; otherwise the body is scanned against the patterns.
func (d *Detector) Match(status int, body []byte) (Reason, bool) {
	if _, ok := d.statuses[status]; ok {
		return Reason{Source: "status", Value: strconv.Itoa(status)}, true
	}
	for _, re := range d.patterns {
		if re.Match(body) {
			return Reason{Source: "pattern", Value: re.String()}, true
		}
	}
	return Reason{}, false
}

// IsQuota reports whether the status/body combination represents exhausted
// quota. A configured status always matches; otherwise the body is scanned.
func (d *Detector) IsQuota(status int, body []byte) bool {
	_, ok := d.Match(status, body)
	return ok
}

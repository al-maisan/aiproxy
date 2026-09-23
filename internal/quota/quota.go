// Package quota classifies upstream HTTP errors as quota exhaustion.
package quota

import (
	"fmt"
	"regexp"
)

// Detector decides whether an upstream response indicates exhausted quota.
type Detector struct {
	statuses map[int]struct{}
	patterns []*regexp.Regexp
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

// IsQuota reports whether the status/body combination represents exhausted
// quota. A configured status always matches; otherwise the body is scanned.
func (d *Detector) IsQuota(status int, body []byte) bool {
	if _, ok := d.statuses[status]; ok {
		return true
	}
	for _, re := range d.patterns {
		if re.Match(body) {
			return true
		}
	}
	return false
}

package quota

import "testing"

func TestIsQuota(t *testing.T) {
	d, err := New([]int{402, 429}, []string{`(?i)quota`, `(?i)usage limit`})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "payment required", status: 402, body: `{"error":"no"}`, want: true},
		{name: "too many requests", status: 429, body: `{"error":"slow down"}`, want: true},
		{name: "body quota", status: 400, body: `{"error":"Quota exceeded"}`, want: true},
		{name: "body usage limit", status: 403, body: "Usage Limit reached", want: true},
		{name: "unrelated error", status: 400, body: `{"error":"bad request"}`, want: false},
		{name: "success", status: 200, body: "", want: false},
		{name: "server error", status: 500, body: "internal error", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := d.IsQuota(tt.status, []byte(tt.body)); got != tt.want {
				t.Fatalf("IsQuota(%d, %q) = %v, want %v", tt.status, tt.body, got, tt.want)
			}
		})
	}
}

func TestNewRejectsInvalidStatus(t *testing.T) {
	if _, err := New([]int{999}, nil); err == nil {
		t.Fatal("expected error for out-of-range status")
	}
}

func TestMatchReason(t *testing.T) {
	d, err := New([]int{402, 429}, []string{`(?i)quota`, `(?i)usage limit`})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Status rule wins and names the status code.
	r, ok := d.Match(429, []byte("anything"))
	if !ok || r.Source != "status" || r.Value != "429" {
		t.Fatalf("status match = %+v, %v", r, ok)
	}
	if got := r.String(); got != "status=429" {
		t.Fatalf("String() = %q", got)
	}

	// Pattern rule names the pattern.
	r, ok = d.Match(400, []byte(`{"error":"Quota exceeded"}`))
	if !ok || r.Source != "pattern" || r.Value != "(?i)quota" {
		t.Fatalf("pattern match = %+v, %v", r, ok)
	}
	if got := r.String(); got != "pattern=(?i)quota" {
		t.Fatalf("String() = %q", got)
	}

	// A match past the first 300 bytes is still reported.
	pad := make([]byte, 400)
	for i := range pad {
		pad[i] = 'x'
	}
	r, ok = d.Match(400, append(pad, []byte(" monthly usage limit")...))
	if !ok || r.Source != "pattern" || r.Value != "(?i)usage limit" {
		t.Fatalf("late pattern match = %+v, %v", r, ok)
	}

	// No match.
	if r, ok := d.Match(500, []byte("internal error")); ok {
		t.Fatalf("unexpected match: %+v", r)
	}
}

func TestReasonStringUnknown(t *testing.T) {
	if got := (Reason{}).String(); got != "unknown" {
		t.Fatalf("empty Reason.String() = %q, want unknown", got)
	}
}

func TestNewRejectsInvalidPattern(t *testing.T) {
	if _, err := New(nil, []string{"("}); err == nil {
		t.Fatal("expected error for invalid pattern")
	}
}

package httpserver

import (
	"errors"
	"testing"
)

func TestClassifyCats(t *testing.T) {
	tests := []struct {
		cats []int
		want string
	}{
		{nil, "other"},
		{[]int{}, "other"},
		{[]int{2000}, "movies"},
		{[]int{2030}, "movies"},
		{[]int{2999}, "movies"},
		{[]int{3000}, "audio"},
		{[]int{3010}, "audio"},
		{[]int{4000}, "software"},
		{[]int{5000}, "tv"},
		{[]int{5040}, "tv"},
		{[]int{5999}, "tv"},
		{[]int{6000}, "xxx"},
		{[]int{7000}, "books"},
		{[]int{7020}, "books"},
		{[]int{8000}, "other"}, // 8xxx is "other" Newznab range
		{[]int{9999}, "other"},
		// First recognised category wins.
		{[]int{8000, 5040, 2000}, "tv"},
		{[]int{9999, 8888, 3010}, "audio"},
	}
	for _, tt := range tests {
		got := classifyCats(tt.cats)
		if got != tt.want {
			t.Errorf("classifyCats(%v): got %q, want %q", tt.cats, got, tt.want)
		}
	}
}

func TestStatusForErr(t *testing.T) {
	if got := statusForErr(nil); got != "ok" {
		t.Errorf("statusForErr(nil): got %q, want \"ok\"", got)
	}
	if got := statusForErr(errors.New("boom")); got != "error" {
		t.Errorf("statusForErr(err): got %q, want \"error\"", got)
	}
}

func TestResultCountIfOK(t *testing.T) {
	if got := resultCountIfOK(nil, 42); got != 42 {
		t.Errorf("resultCountIfOK(nil, 42): got %d, want 42", got)
	}
	if got := resultCountIfOK(errors.New("x"), 42); got != 0 {
		t.Errorf("resultCountIfOK(err, 42): got %d, want 0", got)
	}
}

func TestMetrics_NilSafe(t *testing.T) {
	// observeSearch / observeCaps / observeAuth must be no-ops on a
	// nil receiver so a test or a partially-wired build can call
	// them without panicking.
	var m *Metrics
	m.observeSearch("default", "tvsearch", []int{5000}, 5, 0, "ok")
	m.observeCaps("default")
	m.observeAuth("default", "ok")
	// If we got here without panicking, success.
}

func TestMetrics_Lifecycle(t *testing.T) {
	m := NewMetrics()
	if len(m.Collectors()) != 5 {
		t.Errorf("Collectors() len: got %d, want 5", len(m.Collectors()))
	}
	// Exercise each path so the underlying counters/histograms get
	// at least one observation. Bare-counter-vec series only emit
	// after first .Inc(), so this is also a sanity check that the
	// labels are well-formed.
	m.observeSearch("default", "search", []int{5000}, 10, 0, "ok")
	m.observeSearch("default", "tvsearch", []int{5040}, 0, 0, "ok")
	m.observeSearch("default", "tvsearch", nil, 0, 0, "error")
	m.observeCaps("default")
	m.observeCaps("custom")
	m.observeAuth("default", "ok")
	m.observeAuth("friend-bob", "ok")
	m.observeAuth("open", "ok")
	m.observeAuth("", "rejected") // rejected path stamps key_name="rejected"
}

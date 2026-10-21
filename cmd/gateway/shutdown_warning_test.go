package main

import (
	"testing"
	"time"

	"ai-gateway/internal/proxy"
)

// P1-16: SHUTDOWN_GRACE_SECONDS must cover the image first-byte budget, or a
// rolling deploy cancels in-flight image generations. deploy/README.md §1.1
// documents this boot-time warning as the backstop for hand-set env overrides;
// these tests pin the decision logic.

func TestImageBudget(t *testing.T) {
	withTTFB := func(d time.Duration) *proxy.Handler {
		p := &proxy.Handler{}
		p.ImageTTFB = d
		return p
	}
	cases := []struct {
		name string
		p    *proxy.Handler
		want time.Duration
	}{
		{"default (zero, enabled)", &proxy.Handler{}, proxy.DefaultImageTTFB},
		{"explicit override", withTTFB(42 * time.Second), 42 * time.Second},
		{"watchdog disabled", func() *proxy.Handler {
			p := withTTFB(42 * time.Second)
			p.ImageTTFBDisabled = true
			return p
		}(), 0},
	}
	for _, c := range cases {
		if got := imageBudget(c.p); got != c.want {
			t.Errorf("%s: imageBudget = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestShutdownGraceWarning(t *testing.T) {
	cases := []struct {
		name     string
		graceSec int
		budget   time.Duration
		wantMsg  bool
	}{
		{"grace below budget", 90, 300 * time.Second, true},
		{"grace above budget", 330, 300 * time.Second, false},
		{"grace equal to budget", 300, 300 * time.Second, false},
		{"watchdog disabled — no budget to compare", 90, 0, false},
	}
	for _, c := range cases {
		msg := shutdownGraceWarning(c.graceSec, c.budget)
		if (msg != "") != c.wantMsg {
			t.Errorf("%s: warning = %q, wantMsg = %v", c.name, msg, c.wantMsg)
		}
		if c.wantMsg && msg == "" {
			t.Errorf("%s: expected a non-empty warning", c.name)
		}
	}
}

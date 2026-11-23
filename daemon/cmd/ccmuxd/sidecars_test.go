package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/meridian"
)

// The sidecars run where the proxy runs: in its service on the installed
// Linux daemon, so a deploy leaves them up; in the daemon everywhere the
// proxy is in the daemon too.
func TestSidecarsRunWhereTheProxyRuns(t *testing.T) {
	db := "/home/u/.config/ccmuxd/ccmuxd.db"
	for _, c := range []struct {
		name     string
		unitFlag string
		host     func(h proxyHost) proxyHost
		inDaemon bool
	}{
		{"linux with the unit", "", nil, false},
		{"a scratch daemon with its own unit", "ccmux-llm-proxy-t1", func(h proxyHost) proxyHost { h.scratch = "/run/user/1000/t1"; return h }, false},
		{"-llm-proxy-unit none", llmProxyInProcess, nil, true},
		{"macOS", "", func(h proxyHost) proxyHost { h.goos = "darwin"; return h }, true},
		{"no systemd", "", func(h proxyHost) proxyHost { h.systemctl = false; return h }, true},
		{"a scratch daemon with no unit", "", func(h proxyHost) proxyHost { h.scratch = "/run/user/1000/t1"; return h }, true},
	} {
		host := linuxHost(t)
		if c.host != nil {
			host = c.host(host)
		}
		p := planLLMProxy(c.unitFlag, "", "127.0.0.1:7900", db, host)
		if got := sidecarsInDaemon(p); got != c.inDaemon {
			t.Errorf("%s: sidecars in the daemon = %v, want %v (proxy unit %q)", c.name, got, c.inDaemon, p.unit)
		}
	}
}

type fakeSidecarClient struct {
	mu     sync.Mutex
	reads  int
	pokes  int
	answer map[string]meridian.Status
	err    error
}

func (f *fakeSidecarClient) Sidecars(context.Context) (map[string]meridian.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.answer, f.err
}

func (f *fakeSidecarClient) ReconcileSidecars(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pokes++
	return f.err
}

// The daemon's view of the service's sidecars: one read serves every poll
// for a moment, and a settings apply only pokes the service.
func TestRemoteSidecarsReadOnceAMomentAndPoke(t *testing.T) {
	c := &fakeSidecarClient{answer: map[string]meridian.Status{"sub": {Running: true}}}
	r := newRemoteSidecars(c, noAccounts)
	for range 3 {
		if !r.Status()["sub"].Running {
			t.Fatal("sub not running")
		}
	}
	if c.reads != 1 {
		t.Fatalf("%d reads for three polls in a moment, want 1", c.reads)
	}
	r.ttl, r.at = time.Millisecond, time.Now().Add(-time.Second)
	r.Status()
	if c.reads != 2 {
		t.Fatalf("%d reads after the moment passed, want 2", c.reads)
	}
	r.Reconcile([]meridian.Spec{{Name: "ignored"}})
	if c.pokes != 1 {
		t.Fatalf("%d pokes, want 1", c.pokes)
	}
}

func noAccounts() ([]llmproxy.Account, error) { return nil, nil }

// A service that does not answer (down, or an older build with no such
// route) leaves each meridian account with the reason, never "not
// supervised"; other accounts are not listed.
func TestRemoteSidecarsWithNoAnswerSayWhy(t *testing.T) {
	accs := func() ([]llmproxy.Account, error) {
		return []llmproxy.Account{{Name: "sub", Kind: llmproxy.KindMeridian}, {Name: "key", Kind: "anthropic"}}, nil
	}
	r := newRemoteSidecars(&fakeSidecarClient{err: errors.New("connection refused")}, accs)
	st := r.Status()
	if len(st) != 1 || st["sub"].Running || !strings.Contains(st["sub"].LastError, "connection refused") {
		t.Fatalf("Status = %#v, want only sub, not running, with the reason", st)
	}
	r.at = time.Time{}
	if st := r.Status(); len(st) != 1 {
		t.Fatalf("second read %#v", st)
	}
}

// Unreadable accounts as well: an empty set, never nil, and the log says
// why, once.
func TestRemoteSidecarsWithNothingToGoOnShowNone(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	accs := func() ([]llmproxy.Account, error) { return nil, errors.New("db locked") }
	r := newRemoteSidecars(&fakeSidecarClient{err: errors.New("404")}, accs)
	for range 2 {
		r.at = time.Time{}
		if st := r.Status(); st == nil || len(st) != 0 {
			t.Fatalf("Status = %#v, want an empty map", st)
		}
	}
	if got := logged.String(); strings.Count(got, "the accounts were not read either") != 1 || !strings.Contains(got, "db locked") {
		t.Fatalf("the accounts' error is logged once: %q", got)
	}
}

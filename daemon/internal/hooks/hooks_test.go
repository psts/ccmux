package hooks

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// shortSock returns a short /tmp socket path. macOS caps Unix socket paths at
// ~104 bytes, and t.TempDir() paths are far longer than that.
func shortSock(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("/tmp", "ccmux-ht-"+name+".sock")
	_ = os.Remove(p)
	t.Cleanup(func() { _ = os.Remove(p) })
	return p
}

func TestOutcome(t *testing.T) {
	cases := []struct {
		typ, notif string
		want       model.Attention
		ok         bool
	}{
		{"notification", "idle_prompt", model.AttentionNeedsInput, true},
		{"notification", "permission_prompt", model.AttentionNeedsInput, true},
		{"notification", "elicitation_dialog", model.AttentionNeedsInput, true},
		{"notification", "auth_success", "", false},
		{"notification", "", "", false},
		{"permission_request", "", model.AttentionNeedsInput, true},
		{"ask_user_question", "", model.AttentionNeedsInput, true},
		{"stop", "", model.AttentionDone, true},
		{"user_prompt_submit", "", model.AttentionIdle, true},
		{"session_end", "", model.AttentionIdle, true},
		{"bogus", "", "", false},
	}
	for _, c := range cases {
		got, ok := outcome(c.typ, c.notif)
		if got != c.want || ok != c.ok {
			t.Errorf("outcome(%q,%q) = (%q,%v), want (%q,%v)", c.typ, c.notif, got, ok, c.want, c.ok)
		}
	}
}

type mockRouter struct {
	mu       sync.Mutex
	resolve  string
	gotPane  string
	gotAtt   model.Attention
	calls    int
	gotSig   model.SessionSignal
	gotSess  string
	sigCalls int
}

// ResolvePane mirrors the manager: an explicit pane id wins, otherwise fall
// back to a cwd match — the behaviour that makes the session path dangerous.
func (r *mockRouter) ResolvePane(paneID, cwd string) string {
	if paneID != "" {
		return paneID
	}
	if cwd != "" {
		return r.resolve
	}
	return ""
}
func (r *mockRouter) ApplyAttention(paneID string, att model.Attention) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gotPane, r.gotAtt, r.calls = paneID, att, r.calls+1
}

func (r *mockRouter) ApplySession(paneID, sessionID string, sig model.SessionSignal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gotPane, r.gotSess, r.gotSig, r.sigCalls = paneID, sessionID, sig, r.sigCalls+1
}

// TestListener_RoutesMessage sends the exact JSON ccmux-notify.sh emits and
// asserts it resolves to a pane and applies the mapped attention.
func TestListener_RoutesMessage(t *testing.T) {
	sock := shortSock(t, "routes")
	r := &mockRouter{resolve: "pane-xyz"}
	l, err := Listen(sock, r)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	send(t, sock, `{"type":"permission_request","cwd":"/repo","pane_id":"pane-xyz","session_id":"s1"}`)

	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.calls == 1 })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotPane != "pane-xyz" || r.gotAtt != model.AttentionNeedsInput {
		t.Fatalf("got (%q,%q), want (pane-xyz, needs_input)", r.gotPane, r.gotAtt)
	}
}

// TestListener_IgnoresNonActionable ensures ignored events don't call the router.
func TestListener_IgnoresNonActionable(t *testing.T) {
	sock := shortSock(t, "ignore")
	r := &mockRouter{resolve: "pane-xyz"}
	l, err := Listen(sock, r)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	send(t, sock, `{"type":"notification","notification_type":"auth_success","cwd":"/repo"}`)
	time.Sleep(150 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Fatalf("expected no attention calls for auth_success, got %d", r.calls)
	}
}

func send(t *testing.T, sock, msg string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestSessionOutcome(t *testing.T) {
	cases := []struct {
		typ  string
		want model.SessionSignal
		ok   bool
	}{
		{"session_start", model.SessionStarted, true},
		{"session_end", model.SessionEnded, true},
		{"user_prompt_submit", model.SessionActive, true},
		{"stop", model.SessionActive, true},
		{"permission_request", model.SessionActive, true},
		{"ask_user_question", model.SessionActive, true},
		// A bare notification can fire outside a session; treating it as proof of
		// life would resurrect the very phantom this signal exists to remove.
		{"notification", "", false},
		{"bogus", "", false},
	}
	for _, c := range cases {
		got, ok := sessionOutcome(c.typ)
		if got != c.want || ok != c.ok {
			t.Errorf("sessionOutcome(%q) = (%q,%v), want (%q,%v)", c.typ, got, ok, c.want, c.ok)
		}
	}
}

// session_start carries no attention meaning at all, so it only reaches the
// router through the session path — the event the bus most needs must not be
// dropped by the attention filter.
func TestListener_RoutesSessionStart(t *testing.T) {
	sock := shortSock(t, "sessstart")
	r := &mockRouter{resolve: "pane-xyz"}
	l, err := Listen(sock, r)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	send(t, sock, `{"type":"session_start","cwd":"/repo","pane_id":"pane-xyz","session_id":"s1"}`)

	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.sigCalls == 1 })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotPane != "pane-xyz" || r.gotSess != "s1" || r.gotSig != model.SessionStarted {
		t.Fatalf("got (%q,%q,%q), want (pane-xyz, s1, started)", r.gotPane, r.gotSess, r.gotSig)
	}
	if r.calls != 0 {
		t.Fatalf("session_start has no attention outcome, got %d attention calls", r.calls)
	}
}

// session_end must drive BOTH outcomes: idle attention for the lenses and an
// end signal for the bus.
func TestListener_RoutesSessionEndToBothOutcomes(t *testing.T) {
	sock := shortSock(t, "sessend")
	r := &mockRouter{resolve: "pane-xyz"}
	l, err := Listen(sock, r)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	send(t, sock, `{"type":"session_end","cwd":"/repo","pane_id":"pane-xyz","session_id":"s1"}`)

	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.sigCalls == 1 && r.calls == 1
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gotSig != model.SessionEnded || r.gotAtt != model.AttentionIdle {
		t.Fatalf("got (%q,%q), want (ended, idle)", r.gotSig, r.gotAtt)
	}
}

// A session outside ccmux carries no CCMUX_PANE_ID and shares a cwd prefix with
// hosted panes. Crediting its exit to whichever pane matched that prefix would
// hide a live, unrelated peer — the one direction this signal must never get
// wrong. Attention still flows: a stray flash is cosmetic.
func TestListener_SessionEndWithoutPaneIDIsNotAttributed(t *testing.T) {
	sock := shortSock(t, "nopane")
	r := &mockRouter{resolve: "someone-elses-pane"}
	l, err := Listen(sock, r)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	send(t, sock, `{"type":"session_end","cwd":"/Users/x/Work/Coding/ccmux","session_id":"s1"}`)

	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.calls == 1 })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sigCalls != 0 {
		t.Fatalf("a pane-less session must not end another pane's session, got %d calls on %q",
			r.sigCalls, r.gotPane)
	}
}

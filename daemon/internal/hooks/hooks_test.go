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
	mu      sync.Mutex
	resolve string
	gotPane string
	gotAtt  model.Attention
	calls   int
}

func (r *mockRouter) ResolvePane(paneID, cwd string) string { return r.resolve }
func (r *mockRouter) ApplyAttention(paneID string, att model.Attention) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gotPane, r.gotAtt, r.calls = paneID, att, r.calls+1
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

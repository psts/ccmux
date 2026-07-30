package hooks

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ccmux.dev/ccmuxd/internal/hooktrace"
	"ccmux.dev/ccmuxd/internal/model"
)

func traceTo(t *testing.T) func() []hooktrace.Line {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	old := hooktrace.DefaultPath()
	hooktrace.SetPath(p)
	t.Cleanup(func() { hooktrace.SetPath(old) })

	return func() []hooktrace.Line {
		t.Helper()
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		var out []hooktrace.Line
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var l hooktrace.Line
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				t.Fatalf("trace line is not JSON: %v", err)
			}
			out = append(out, l)
		}
		return out
	}
}

func routeOnly(lines []hooktrace.Line, decision string) []hooktrace.Line {
	var out []hooktrace.Line
	for _, l := range lines {
		if l.Stage == hooktrace.StageRoute && l.Decision == decision {
			out = append(out, l)
		}
	}
	return out
}

// The trace id minted by ccmux-notify.sh has to survive the socket hop, otherwise
// a route line can't be tied to the hook that caused it and the log stops being
// one story per hook.
func TestRouteTrace_CarriesTheScriptsTraceID(t *testing.T) {
	read := traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "stop", CWD: "/repo", PaneID: "pane-1", TraceID: "cafe1234"})

	got := routeOnly(read(), "attention")
	if len(got) != 1 {
		t.Fatalf("want 1 attention line, got %d", len(got))
	}
	if got[0].TraceID != "cafe1234" {
		t.Errorf("trace_id = %q, want the id the hook script minted", got[0].TraceID)
	}
	if got[0].Resolved != "pane-1" || got[0].Attention != string(model.AttentionDone) {
		t.Errorf("line lost the decision it recorded: %+v", got[0])
	}
}

// An ignored hook is the case the log exists for: something fired, ccmux chose
// not to act, and without a line there is no evidence either way.
func TestRouteTrace_IgnoredEventStillLogs(t *testing.T) {
	read := traceTo(t)
	l := newListener(&mockRouter{resolve: "pane-1"})

	l.route(hookMsg{Type: "notification", NotificationType: "auth_success", CWD: "/repo"})

	got := routeOnly(read(), "ignored")
	if len(got) != 1 {
		t.Fatalf("want 1 ignored line, got %d", len(got))
	}
	if got[0].Event != "notification" {
		t.Errorf("event = %q, want the hook type", got[0].Event)
	}
}

func TestRouteTrace_UnresolvedPaneIsRecorded(t *testing.T) {
	read := traceTo(t)
	l := newListener(&mockRouter{resolve: ""}) // nothing matches

	l.route(hookMsg{Type: "stop", CWD: "/somewhere/else"})

	if got := routeOnly(read(), "unresolved"); len(got) != 1 {
		t.Fatalf("want 1 unresolved line, got %d", len(got))
	}
}

// A session-bearing event writes two lines — one for the attention it applied,
// one for the session signal — because the two resolve panes under different
// rules and can disagree.
func TestRouteTrace_SessionSignalGetsItsOwnLine(t *testing.T) {
	read := traceTo(t)
	l := newListener(&mockRouter{resolve: "pane-1"})

	l.route(hookMsg{Type: "user_prompt_submit", CWD: "/repo", PaneID: "pane-1", SessionID: "s1"})

	lines := read()
	if got := routeOnly(lines, "attention"); len(got) != 1 {
		t.Errorf("want 1 attention line, got %d", len(got))
	}
	got := routeOnly(lines, "session")
	if len(got) != 1 {
		t.Fatalf("want 1 session line, got %d", len(got))
	}
	if got[0].Session != string(model.SessionActive) {
		t.Errorf("session_signal = %q, want %q", got[0].Session, model.SessionActive)
	}
}

// Tracing must not change what the router is told to do — the log is an observer,
// not a participant.
func TestRouteTrace_DoesNotAlterRouting(t *testing.T) {
	traceTo(t)
	r := &mockRouter{resolve: "pane-1"}
	l := newListener(r)

	l.route(hookMsg{Type: "stop", CWD: "/repo", PaneID: "pane-1"})

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 1 || r.gotPane != "pane-1" || r.gotAtt != model.AttentionDone {
		t.Errorf("router saw calls=%d pane=%q att=%q, want one done on pane-1", r.calls, r.gotPane, r.gotAtt)
	}
}

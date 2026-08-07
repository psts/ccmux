// Package hooks ingests Claude Code hook events over a Unix domain socket and
// routes them to pane attention updates. The wire format is the existing
// ccmux-notify.sh message ({type, cwd, notification_type} plus session_id and
// pane_id), so the daemon binds the same socket the app used in driver mode.
package hooks

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"ccmux.dev/ccmuxd/internal/hooktrace"
	"ccmux.dev/ccmuxd/internal/model"
)

// Router resolves a hook to a pane and applies its outcomes: an attention state
// for the lenses, and a session-lifecycle signal for the peers bus.
type Router interface {
	ResolvePane(paneID, cwd string) string
	ApplyAttention(paneID string, att model.Attention)
	ApplySession(paneID, sessionID string, sig model.SessionSignal)
}

type hookMsg struct {
	Type             string `json:"type"`
	CWD              string `json:"cwd"`
	NotificationType string `json:"notification_type"`
	SessionID        string `json:"session_id"`
	PaneID           string `json:"pane_id"`
	// TraceID ties this message back to the ccmux-notify.sh line that produced
	// it, so the trace file reads as one story per hook. Absent from older
	// scripts; an empty id just means the route line stands on its own.
	TraceID string `json:"trace_id"`
}

// Listener owns the hooks Unix socket.
type Listener struct {
	path   string
	router Router
	ln     net.Listener
}

// Listen binds path (removing any stale socket) and serves hook messages. The
// socket is world-writable so hook scripts running as the same user — or any
// local process — can connect; network-level auth is the tailnet's job.
func Listen(path string, r Router) (*Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o777)
	l := newListener(r)
	l.path, l.ln = path, ln
	go l.serve()
	return l, nil
}

// WritePointer records the socket a hook should send to when the path frozen
// into its pane's environment no longer exists — see the fallback in
// hooks/ccmux-notify.sh.
//
// Pane environment is written once, at session creation, and tmux sessions
// outlive daemon restarts and upgrades by design. When this socket's path last
// moved, every pane older than the move went on addressing the old one, and
// nothing anywhere reported it: the hooks simply stopped arriving. A pointer the
// daemon rewrites on every start is what makes the live path discoverable
// instead of remembered.
//
// 0644, unlike the 0600 peers info file beside it: this names an already
// world-writable socket and carries no credential. The 0700 parent directory is
// the boundary that matters.
func WritePointer(path, socket string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(socket+"\n"), 0o644)
}

// newListener builds the routing half of a Listener, with no socket bound. Tests
// route messages through this; Listen adds the socket.
func newListener(r Router) *Listener {
	return &Listener{router: r}
}

func (l *Listener) serve() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.handle(conn)
	}
}

func (l *Listener) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, err := io.ReadAll(io.LimitReader(conn, 64*1024))
	if err != nil {
		return
	}
	var msg hookMsg
	if json.Unmarshal(data, &msg) != nil {
		return
	}
	l.route(msg)
}

// route applies a hook's outcomes and traces every branch, including the ones
// that do nothing. "The daemon received this hook and deliberately ignored it" is
// as useful to a reader chasing a stray notification as "the daemon flashed a
// pane" — the silent branches are exactly where a missing flash hides.
func (l *Listener) route(msg hookMsg) {
	att, wantAtt := outcome(msg.Type, msg.NotificationType)
	sig, wantSig := sessionOutcome(msg.Type)
	if !wantAtt && !wantSig {
		l.trace(msg, hooktrace.Line{Decision: "ignored", Detail: "no attention or session meaning"})
		return
	}
	if wantAtt {
		l.applyAttention(msg, att)
	}
	// Session truth demands an EXPLICIT pane. Attention tolerates the cwd
	// fallback because a flash on the wrong pane is cosmetic; a session-end
	// credited to the wrong pane hides a live teammate. Sessions outside ccmux
	// carry no CCMUX_PANE_ID and share a cwd prefix with hosted panes, so the
	// fallback would routinely blame someone else's pane for their exit.
	// Resolving with an empty cwd yields "" unless the pane id itself is real.
	if wantSig {
		l.applySession(msg, sig)
	}
}

func (l *Listener) applyAttention(msg hookMsg, att model.Attention) {
	paneID := l.router.ResolvePane(msg.PaneID, msg.CWD)
	if paneID == "" {
		l.trace(msg, hooktrace.Line{Decision: "unresolved", Detail: "no pane matches this pane id or cwd"})
		return
	}
	l.router.ApplyAttention(paneID, att)
	l.trace(msg, hooktrace.Line{Decision: "attention", Resolved: paneID, Attention: string(att)})
}

func (l *Listener) applySession(msg hookMsg, sig model.SessionSignal) {
	sp := l.router.ResolvePane(msg.PaneID, "")
	if sp == "" {
		l.trace(msg, hooktrace.Line{Decision: "session-unresolved", Detail: "hook carries no ccmux pane id"})
		return
	}
	l.router.ApplySession(sp, msg.SessionID, sig)
	l.trace(msg, hooktrace.Line{Decision: "session", Resolved: sp, Session: string(sig)})
}

// trace fills in the fields every route line shares, so each call site states
// only what it decided.
func (l *Listener) trace(msg hookMsg, line hooktrace.Line) {
	line.Stage = hooktrace.StageRoute
	line.TraceID = msg.TraceID
	line.Event = msg.Type
	line.CWD = msg.CWD
	line.SessionID = msg.SessionID
	line.PaneID = msg.PaneID
	if line.Detail == "" && msg.NotificationType != "" {
		line.Detail = msg.NotificationType
	}
	hooktrace.Write(line)
}

// Close stops the listener and removes the socket.
func (l *Listener) Close() error {
	err := l.ln.Close()
	_ = os.Remove(l.path)
	return err
}

// outcome maps a hook event (+ optional notification subtype) to an attention
// state, or ok=false to ignore. Ported verbatim from
// ClaudeHookListener.outcome(forEvent:notificationType:); "clear" collapses to
// idle since that is what clearing attention means here.
func outcome(eventType, notificationType string) (model.Attention, bool) {
	switch eventType {
	case "notification":
		switch notificationType {
		case "idle_prompt", "permission_prompt", "elicitation_dialog":
			return model.AttentionNeedsInput, true
		default:
			return "", false // auth_success and other non-blocking notifications
		}
	case "permission_request", "ask_user_question":
		return model.AttentionNeedsInput, true
	case "stop":
		return model.AttentionDone, true
	case "user_prompt_submit", "session_end":
		return model.AttentionIdle, true
	default:
		return "", false
	}
}

// sessionOutcome maps a hook event to what it proves about the pane's Claude
// SESSION. The four activity events are unambiguously session-bound — prompting,
// stopping, asking permission — so they let a missed session_start (hooks
// installed mid-session, daemon restarted) heal itself instead of leaving a live
// session looking dead. Bare notifications are deliberately NOT proof of life:
// they can fire outside a session, and a false positive here would resurrect
// exactly the phantom this signal exists to remove.
func sessionOutcome(eventType string) (model.SessionSignal, bool) {
	switch eventType {
	case "session_start":
		return model.SessionStarted, true
	case "session_end":
		return model.SessionEnded, true
	case "user_prompt_submit", "stop", "permission_request", "ask_user_question":
		return model.SessionActive, true
	default:
		return "", false
	}
}

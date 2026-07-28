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
	"time"

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
	l := &Listener{path: path, router: r, ln: ln}
	go l.serve()
	return l, nil
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
	att, wantAtt := outcome(msg.Type, msg.NotificationType)
	sig, wantSig := sessionOutcome(msg.Type)
	if !wantAtt && !wantSig {
		return
	}
	paneID := l.router.ResolvePane(msg.PaneID, msg.CWD)
	if paneID == "" {
		return
	}
	if wantAtt {
		l.router.ApplyAttention(paneID, att)
	}
	// Session truth demands an EXPLICIT pane. Attention tolerates the cwd
	// fallback because a flash on the wrong pane is cosmetic; a session-end
	// credited to the wrong pane hides a live teammate. Sessions outside ccmux
	// carry no CCMUX_PANE_ID and share a cwd prefix with hosted panes, so the
	// fallback would routinely blame someone else's pane for their exit.
	// Resolving with an empty cwd yields "" unless the pane id itself is real.
	if wantSig {
		if sp := l.router.ResolvePane(msg.PaneID, ""); sp != "" {
			l.router.ApplySession(sp, msg.SessionID, sig)
		}
	}
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

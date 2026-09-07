package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/model"
)

// The agent chat view. An agent pane's terminal shows opencode's TUI; the
// chat view shows the same conversation as a transcript with a prompt box,
// so a human never has to work the TUI. The daemon is the one reader of
// opencode's server (history, prompts, aborts, permission replies, the event
// stream) and hands both lenses one normalized shape (agent.Turn).
//
// States: "asleep" (the pane is at its shell; history comes from opencode's
// store through its CLI, a prompt wakes the agent with that text),
// "starting" (woken, server not up yet), "running" (live: prompts go to the
// server, events stream back).

// chatFrame is the envelope both ways on /v1/panes/{id}/agent/ws and the
// body of GET /v1/panes/{id}/agent (a hello without the socket).
type chatFrame struct {
	T string `json:"t"`
	// hello: the whole picture; state alone on a change.
	Agent       string                    `json:"agent,omitempty"`
	State       string                    `json:"state,omitempty"`
	Session     string                    `json:"session,omitempty"`
	Title       string                    `json:"title,omitempty"`
	Turns       []agent.Turn              `json:"turns,omitempty"`
	Permissions []agent.PermissionRequest `json:"permissions,omitempty"`
	// turn / part / delta: one change to the transcript.
	Turn      *agent.Turn     `json:"turn,omitempty"`
	Part      *agent.TurnPart `json:"part,omitempty"`
	MessageID string          `json:"messageId,omitempty"`
	PartID    string          `json:"partId,omitempty"`
	Field     string          `json:"field,omitempty"`
	Delta     string          `json:"delta,omitempty"`
	// permission (asked) / permission-replied; the client's reply rides the
	// same fields: {t:"permission", id, reply}.
	Permission *agent.PermissionRequest `json:"permission,omitempty"`
	ID         string                   `json:"id,omitempty"`
	Reply      string                   `json:"reply,omitempty"`
	// prompt (client → daemon) carries Text; error carries Error.
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// chatConn is one lens's chat socket on one agent pane.
type chatConn struct {
	s      *Server
	pane   *model.Pane
	out    chan chatFrame
	wake   chan string
	cancel context.CancelFunc
	mu     sync.Mutex
	oc     *agent.Opencode // live client, nil while asleep
	sid    string
}

// paneAgentHistory: GET /v1/panes/{id}/agent → the hello frame: state,
// session and transcript, for a lens that wants a one-shot read.
func (s *Server) paneAgentHistory(w http.ResponseWriter, r *http.Request) {
	p := s.agentPaneOr404(w, r)
	if p == nil {
		return
	}
	c := &chatConn{s: s, pane: p}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if oc := c.liveClient(ctx); oc != nil {
		if hello, err := c.liveHello(ctx, oc); err == nil {
			writeJSON(w, http.StatusOK, hello)
			return
		}
	}
	writeJSON(w, http.StatusOK, c.asleepHello(ctx))
}

// paneAgentChat: GET /v1/panes/{id}/agent/ws upgrades to the chat socket.
// One writer goroutine (this one), one reader, one source loop that
// follows the agent between asleep and running.
func (s *Server) paneAgentChat(w http.ResponseWriter, r *http.Request) {
	p := s.agentPaneOr404(w, r)
	if p == nil {
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &chatConn{s: s, pane: p, out: make(chan chatFrame, 64), wake: make(chan string, 1), cancel: cancel}
	go c.readLoop(conn)
	go c.run(ctx)
	c.writeLoop(ctx, conn)
}

func (s *Server) agentPaneOr404(w http.ResponseWriter, r *http.Request) *model.Pane {
	p := s.mgr.PaneByID(r.PathValue("id"))
	if p == nil || p.Agent == "" {
		writeError(w, http.StatusNotFound, "not an agent pane")
		return nil
	}
	return p
}

func (c *chatConn) writeLoop(ctx context.Context, conn *websocket.Conn) {
	ping := time.NewTicker(c.s.ka.pingEvery())
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := c.s.ka.writePing(conn); err != nil {
				return
			}
		case f := <-c.out:
			if err := conn.WriteJSON(f); err != nil {
				return
			}
		}
	}
}

func (c *chatConn) readLoop(conn *websocket.Conn) {
	defer c.cancel()
	c.s.ka.armReads(conn)
	for {
		var f chatFrame
		if err := conn.ReadJSON(&f); err != nil {
			return
		}
		c.s.ka.touchReads(conn)
		c.handle(f)
	}
}

// handle is a client frame: a prompt (to the server when live, else a
// wake), an abort, or a permission reply.
func (c *chatConn) handle(f chatFrame) {
	c.mu.Lock()
	oc, sid := c.oc, c.sid
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var err error
	switch f.T {
	case "prompt":
		if oc == nil {
			c.requestWake(f.Text)
			return
		}
		err = c.s.mgr.PushLocked(c.pane.ID, func() error { return oc.Prompt(ctx, sid, f.Text) })
	case "abort":
		if oc != nil {
			err = oc.Abort(ctx, sid)
		}
	case "permission":
		if oc != nil {
			err = oc.ReplyPermission(ctx, f.ID, f.Reply)
		}
	}
	if err != nil {
		c.send(chatFrame{T: "error", Error: err.Error()})
	}
}

func (c *chatConn) requestWake(text string) {
	select {
	case c.wake <- text:
	default:
		c.send(chatFrame{T: "error", Error: "already starting"})
	}
}

func (c *chatConn) send(f chatFrame) {
	select {
	case c.out <- f:
	default:
		// A lens that cannot keep up loses a frame rather than stalling the
		// source; the next hello (state change) resyncs it.
	}
}

// run follows the agent: live while its server answers, asleep otherwise.
func (c *chatConn) run(ctx context.Context) {
	for ctx.Err() == nil {
		if oc := c.liveClient(ctx); oc != nil {
			c.runLive(ctx, oc)
			continue
		}
		c.send(c.asleepHello(ctx))
		c.awaitWake(ctx)
	}
}

// liveClient is the pane's server when the agent is running: an opencode
// port on its startup line, a foreground that is not the shell, and a
// server that answers.
func (c *chatConn) liveClient(ctx context.Context) *agent.Opencode {
	p := c.s.mgr.PaneByID(c.pane.ID)
	if p == nil {
		return nil
	}
	c.pane = p
	port := agent.OpencodePort(p.StartupCommand)
	if port == 0 || c.s.mgr.PaneAtShell(p.ID) {
		return nil
	}
	oc := agent.OpencodeAt(port)
	if !oc.Up(ctx) {
		return nil
	}
	return oc
}

// awaitWake waits, while asleep, for a prompt from the lens (which starts
// the agent with it) or for the agent to come up by another path (the bus).
func (c *chatConn) awaitWake(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case text := <-c.wake:
			if msg := c.s.wakeFromChat(c.pane, text); msg != "" {
				c.send(chatFrame{T: "error", Error: msg})
				continue
			}
			c.send(chatFrame{T: "state", State: "starting"})
			c.awaitServer(ctx)
			return
		case <-tick.C:
			if c.liveClient(ctx) != nil {
				return
			}
		}
	}
}

// awaitServer polls for the woken agent's server; the caller's loop then
// runs live. A start that never comes up falls back to asleep with a note.
func (c *chatConn) awaitServer(ctx context.Context) {
	deadline := time.Now().Add(45 * time.Second)
	for ctx.Err() == nil && time.Now().Before(deadline) {
		if c.liveClient(ctx) != nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.send(chatFrame{T: "error", Error: "the agent did not come up"})
}

// wakeFromChat starts the agent with text as its first message, through the
// one add-or-wake flow (the pane's session must sit in a shared window).
func (s *Server) wakeFromChat(p *model.Pane, text string) string {
	group, ok := s.mgr.GroupForPane(p.ID)
	if !ok || group == "" {
		return "this agent's session is in no shared window; add it to one first"
	}
	win, _, msg := s.windowByName(group)
	if msg != "" {
		return msg
	}
	return s.startWindowAgent(win, p.Agent, text, "chat").msg
}

// runLive serves one live phase: hello with the transcript, then the event
// stream, until the server goes away or the pane returns to its shell.
func (c *chatConn) runLive(ctx context.Context, oc *agent.Opencode) {
	hello, err := c.liveHello(ctx, oc)
	if err != nil {
		log.Printf("agent chat %s: %v", c.pane.ID, err)
		time.Sleep(time.Second)
		return
	}
	c.mu.Lock()
	c.oc, c.sid = oc, hello.Session
	c.mu.Unlock()
	c.send(hello)
	live, stop := context.WithCancel(ctx)
	go c.watchLive(live, stop, oc)
	if err := oc.Events(live, c.onEvent); err != nil && ctx.Err() == nil {
		log.Printf("agent chat %s: event stream ended: %v", c.pane.ID, err)
	}
	stop()
	c.mu.Lock()
	c.oc, c.sid = nil, ""
	c.mu.Unlock()
}

func (c *chatConn) watchLive(ctx context.Context, stop context.CancelFunc, oc *agent.Opencode) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if c.s.mgr.PaneAtShell(c.pane.ID) || !oc.Up(ctx) {
				stop()
				return
			}
		}
	}
}

// liveHello reads the running agent's newest session in its own folder,
// its transcript and the permission requests waiting on it.
func (c *chatConn) liveHello(ctx context.Context, oc *agent.Opencode) (chatFrame, error) {
	sessions, err := oc.Sessions(ctx)
	if err != nil {
		return chatFrame{}, err
	}
	hello := chatFrame{T: "hello", Agent: c.pane.Agent, State: "running", Turns: []agent.Turn{}}
	cur := newestIn(sessions, c.pane.CWD)
	if cur == nil {
		return hello, nil
	}
	hello.Session, hello.Title = cur.ID, cur.Title
	if hello.Turns, err = oc.Messages(ctx, cur.ID); err != nil {
		return chatFrame{}, err
	}
	perms, err := oc.Permissions(ctx)
	if err != nil {
		return chatFrame{}, err
	}
	for _, p := range perms {
		if p.SessionID == cur.ID {
			hello.Permissions = append(hello.Permissions, p)
		}
	}
	return hello, nil
}

// asleepHello is the transcript of the newest session opened in the
// instance folder, from opencode's store; empty when it has none, or when
// opencode's CLI cannot answer (said in Error, not hidden).
func (c *chatConn) asleepHello(ctx context.Context) chatFrame {
	hello := chatFrame{T: "hello", Agent: c.pane.Agent, State: "asleep", Turns: []agent.Turn{}}
	sessions, err := c.s.offlineSessions(ctx, c.pane.CWD)
	if err != nil {
		hello.Error = err.Error()
		return hello
	}
	if len(sessions) == 0 {
		return hello
	}
	hello.Session, hello.Title = sessions[0].ID, sessions[0].Title
	if hello.Turns, err = c.s.offlineTranscript(ctx, sessions[0].ID); err != nil {
		hello.Error, hello.Turns = err.Error(), []agent.Turn{}
	}
	return hello
}

func newestIn(sessions []agent.OpencodeSession, dir string) *agent.OpencodeSession {
	for i := range sessions {
		if sessions[i].Directory == dir {
			return &sessions[i]
		}
	}
	if len(sessions) > 0 {
		return &sessions[0]
	}
	return nil
}

// eventProps is the union of what the chat view reads from opencode's
// events; each kind fills its own fields.
type eventProps struct {
	SessionID string          `json:"sessionID"`
	Info      json.RawMessage `json:"info"`
	Part      json.RawMessage `json:"part"`
	MessageID string          `json:"messageID"`
	PartID    string          `json:"partID"`
	Field     string          `json:"field"`
	Delta     string          `json:"delta"`
	RequestID string          `json:"requestID"`
	Reply     string          `json:"reply"`
	Error     json.RawMessage `json:"error"`
}

// onEvent maps one opencode event onto the socket. Only the current
// session's events pass; a session created in the instance folder becomes
// the current one (the TUI opened a new conversation) with a fresh hello.
func (c *chatConn) onEvent(ev agent.OpencodeEvent) {
	var p eventProps
	_ = json.Unmarshal(ev.Props, &p)
	if ev.Type == "session.created" {
		c.maybeSwitchSession(p)
		return
	}
	c.mu.Lock()
	sid := c.sid
	c.mu.Unlock()
	if p.SessionID != sid {
		return
	}
	if f, ok := c.frameFor(ev.Type, p); ok {
		c.send(f)
	}
}

func (c *chatConn) frameFor(kind string, p eventProps) (chatFrame, bool) {
	switch kind {
	case "message.updated", "message.part.updated", "message.part.delta":
		return transcriptFrame(kind, p)
	case "session.idle":
		return chatFrame{T: "idle"}, true
	case "permission.asked":
		var req agent.PermissionRequest
		if err := json.Unmarshal(mustJSON(p), &req); err != nil {
			return chatFrame{}, false
		}
		return chatFrame{T: "permission", Permission: &req}, true
	case "permission.replied":
		return chatFrame{T: "permission-replied", ID: p.RequestID, Reply: p.Reply}, true
	case "session.error":
		return chatFrame{T: "error", Error: string(p.Error)}, true
	}
	return chatFrame{}, false
}

// transcriptFrame is one change to the transcript: a turn (its header), a
// whole part, or a streamed delta to one part's field.
func transcriptFrame(kind string, p eventProps) (chatFrame, bool) {
	switch kind {
	case "message.updated":
		t, err := agent.NormalizeInfo(p.Info)
		if err != nil {
			return chatFrame{}, false
		}
		return chatFrame{T: "turn", Turn: &t}, true
	case "message.part.updated":
		mid, part, ok := agent.NormalizePart(p.Part)
		if !ok {
			return chatFrame{}, false
		}
		return chatFrame{T: "part", MessageID: mid, Part: &part}, true
	default:
		return chatFrame{T: "delta", MessageID: p.MessageID, PartID: p.PartID, Field: p.Field, Delta: p.Delta}, true
	}
}

// maybeSwitchSession follows the TUI to a new session in this folder.
func (c *chatConn) maybeSwitchSession(p eventProps) {
	var info struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
	}
	_ = json.Unmarshal(p.Info, &info)
	if info.Directory != c.pane.CWD || info.ID == "" {
		return
	}
	c.mu.Lock()
	oc := c.oc
	c.sid = info.ID
	c.mu.Unlock()
	if oc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if hello, err := c.liveHello(ctx, oc); err == nil {
		c.send(hello)
	}
}

// mustJSON re-encodes the permission.asked properties, which ARE the
// request object, for the typed decode.
func mustJSON(p eventProps) []byte {
	b, _ := json.Marshal(p)
	return b
}

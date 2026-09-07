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
	// question (asked) / question-replied; the client answers with
	// {t:"question", id, answers} (one list of labels per question) or
	// {t:"question-reject", id}. Questions ride the hello too.
	Questions []agent.QuestionRequest `json:"questions,omitempty"`
	Question  *agent.QuestionRequest  `json:"question,omitempty"`
	Answers   [][]string              `json:"answers,omitempty"`
	// prompt (client → daemon) carries Text, and Resume when the agent is
	// asleep: whether this start continues the last conversation. On the
	// hello, Resume is the base's default for that choice.
	Text   string `json:"text,omitempty"`
	Resume *bool  `json:"resume,omitempty"`
	Error  string `json:"error,omitempty"`
}

// historySessions is how many of an agent's conversations a chat shows in
// one scroll, newest last, each behind a session marker.
const historySessions = 4

// chatConn is one lens's chat socket on one agent pane. paneID, agent and
// cwd never change for a pane, so every goroutine reads them freely; oc,
// sid and starting are the live state, under mu.
type chatConn struct {
	s      *Server
	paneID string
	agent  string
	cwd    string
	out    chan chatFrame
	wake   chan wakeReq
	cancel context.CancelFunc
	mu     sync.Mutex
	oc     *agent.Opencode // live client, nil while asleep
	sid    string
	// starting is set from the wake until the server answers: a prompt in
	// that window is refused with a note rather than queued into nothing.
	starting bool
}

func newChatConn(s *Server, p *model.Pane, cancel context.CancelFunc) *chatConn {
	return &chatConn{s: s, paneID: p.ID, agent: p.Agent, cwd: p.CWD, out: make(chan chatFrame, 64), wake: make(chan wakeReq, 1), cancel: cancel}
}

// paneAgentHistory: GET /v1/panes/{id}/agent → the hello frame: state,
// session and transcript, for a lens that wants a one-shot read.
func (s *Server) paneAgentHistory(w http.ResponseWriter, r *http.Request) {
	p := s.agentPaneOr404(w, r)
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c := newChatConn(s, p, cancel)
	if oc := c.liveClient(ctx); oc != nil {
		hello, err := c.liveHello(ctx, oc)
		if err != nil {
			// The server answers but its history does not: say so rather
			// than call a running agent asleep.
			writeError(w, http.StatusBadGateway, "opencode: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, hello)
		return
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
	c := newChatConn(s, p, cancel)
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
// wake), an abort, or an answer to a permission or question request.
func (c *chatConn) handle(f chatFrame) {
	c.mu.Lock()
	oc, sid, starting := c.oc, c.sid, c.starting
	c.mu.Unlock()
	if f.T == "prompt" && oc == nil {
		if starting {
			c.send(chatFrame{T: "error", Error: "the agent is still starting; send that again in a moment"})
			return
		}
		c.requestWake(wakeReq{text: f.Text, resume: f.Resume})
		return
	}
	if oc == nil {
		c.send(chatFrame{T: "error", Error: "the agent is not running, so there is nothing to " + f.T})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.liveAction(ctx, oc, sid, f); err != nil {
		c.send(chatFrame{T: "error", Error: err.Error()})
	}
}

// liveAction is one client frame against the running server. The prompt
// goes under the pane's push lock so it cannot interleave with a bus
// delivery into the same instance.
func (c *chatConn) liveAction(ctx context.Context, oc *agent.Opencode, sid string, f chatFrame) error {
	switch f.T {
	case "prompt":
		return c.s.mgr.PushLocked(c.paneID, func() error { return oc.Prompt(ctx, sid, f.Text) })
	case "abort":
		return oc.Abort(ctx, sid)
	case "permission":
		return oc.ReplyPermission(ctx, f.ID, f.Reply)
	case "question":
		return oc.ReplyQuestion(ctx, f.ID, f.Answers)
	case "question-reject":
		return oc.RejectQuestion(ctx, f.ID)
	}
	return nil
}

// wakeReq is a prompt typed into an asleep agent: the text, and whether to
// continue the last conversation (nil = the base's default).
type wakeReq struct {
	text   string
	resume *bool
}

func (c *chatConn) requestWake(w wakeReq) {
	select {
	case c.wake <- w:
	default:
		c.send(chatFrame{T: "error", Error: "already starting"})
	}
}

func (c *chatConn) send(f chatFrame) {
	select {
	case c.out <- f:
	default:
		// A lens that cannot keep up loses a frame rather than stalling the
		// source; the next hello (state change) resyncs it. Said in the log,
		// since a dropped permission or question is a request nobody sees.
		log.Printf("agent chat %s: lens not reading; %s frame dropped", c.paneID, f.T)
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
	p := c.s.mgr.PaneByID(c.paneID)
	if p == nil {
		return nil
	}
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
		case w := <-c.wake:
			if msg := c.s.wakeFromChat(c.paneID, c.agent, w); msg != "" {
				c.send(chatFrame{T: "error", Error: msg})
				continue
			}
			c.setStarting(true)
			c.send(chatFrame{T: "state", State: "starting"})
			c.awaitServer(ctx)
			c.setStarting(false)
			return
		case <-tick.C:
			if c.liveClient(ctx) != nil {
				return
			}
		}
	}
}

func (c *chatConn) setStarting(v bool) {
	c.mu.Lock()
	c.starting = v
	c.mu.Unlock()
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
func (s *Server) wakeFromChat(paneID, agentName string, w wakeReq) string {
	group, ok := s.mgr.GroupForPane(paneID)
	if !ok || group == "" {
		return "this agent's session is in no shared window; add it to one first"
	}
	win, _, msg := s.windowByName(group)
	if msg != "" {
		return msg
	}
	return s.startWindowAgent(win, agentName, w.text, "chat", w.resume).msg
}

// runLive serves one live phase: hello with the transcript, then the event
// stream, until the server goes away or the pane returns to its shell.
func (c *chatConn) runLive(ctx context.Context, oc *agent.Opencode) {
	hello, err := c.liveHello(ctx, oc)
	if err != nil {
		log.Printf("agent chat %s: %v", c.paneID, err)
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
		log.Printf("agent chat %s: event stream ended: %v", c.paneID, err)
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
			if c.s.mgr.PaneAtShell(c.paneID) || !oc.Up(ctx) {
				stop()
				return
			}
		}
	}
}

// liveHello reads the running agent's newest session in its own folder,
// the last few conversations there (history), and the permission and
// question requests waiting on the current one.
func (c *chatConn) liveHello(ctx context.Context, oc *agent.Opencode) (chatFrame, error) {
	sessions, err := oc.Sessions(ctx)
	if err != nil {
		return chatFrame{}, err
	}
	hello := chatFrame{T: "hello", Agent: c.agent, State: "running", Turns: []agent.Turn{}, Resume: c.resumeDefault()}
	mine := sessionsIn(sessions, c.cwd)
	if len(mine) == 0 {
		return hello, nil
	}
	cur := mine[0]
	hello.Session, hello.Title = cur.ID, cur.Title
	if hello.Turns, err = history(mine, func(id string) ([]agent.Turn, error) { return oc.Messages(ctx, id) }); err != nil {
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
	questions, err := oc.Questions(ctx)
	if err != nil {
		return chatFrame{}, err
	}
	for _, q := range questions {
		if q.SessionID == cur.ID {
			hello.Questions = append(hello.Questions, q)
		}
	}
	return hello, nil
}

// asleepHello is the last few conversations opened in the instance folder,
// from opencode's store, the newest as the current session; empty when it
// has none, or when opencode's CLI cannot answer (said in Error, not
// hidden).
func (c *chatConn) asleepHello(ctx context.Context) chatFrame {
	hello := chatFrame{T: "hello", Agent: c.agent, State: "asleep", Turns: []agent.Turn{}, Resume: c.resumeDefault()}
	sessions, err := c.s.offlineSessions(ctx, c.cwd)
	if err != nil {
		hello.Error = err.Error()
		return hello
	}
	if len(sessions) == 0 {
		return hello
	}
	hello.Session, hello.Title = sessions[0].ID, sessions[0].Title
	if hello.Turns, err = history(sessions, func(id string) ([]agent.Turn, error) { return c.s.offlineTranscript(ctx, id) }); err != nil {
		hello.Error, hello.Turns = err.Error(), []agent.Turn{}
	}
	return hello
}

// resumeDefault is the base's start setting as the chat's default for the
// "continue previous conversation" choice; nil when the base cannot be read.
func (c *chatConn) resumeDefault() *bool {
	if c.s.agents == nil {
		return nil
	}
	d, err := c.s.agents.Get(c.agent)
	if err != nil {
		return nil
	}
	v := d.Start == "continue"
	return &v
}

// sessionsIn is the sessions opened in dir, newest first (the list is
// already newest first). Only those: another folder's conversation is not
// this agent's, whatever else the server holds.
func sessionsIn(sessions []agent.OpencodeSession, dir string) []agent.OpencodeSession {
	var mine []agent.OpencodeSession
	for _, s := range sessions {
		if agent.SameDir(s.Directory, dir) {
			mine = append(mine, s)
		}
	}
	return mine
}

// history is the last historySessions conversations in one scroll, oldest
// first, each behind its marker, so a woken agent's earlier work stays on
// screen above the fresh conversation.
func history(newestFirst []agent.OpencodeSession, fetch func(id string) ([]agent.Turn, error)) ([]agent.Turn, error) {
	if len(newestFirst) > historySessions {
		newestFirst = newestFirst[:historySessions]
	}
	turns := []agent.Turn{}
	for i := len(newestFirst) - 1; i >= 0; i-- {
		s := newestFirst[i]
		got, err := fetch(s.ID)
		if err != nil {
			return nil, err
		}
		turns = append(turns, agent.SessionMarker(s))
		turns = append(turns, got...)
	}
	return turns, nil
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
	// permission.asked and question.asked carry the request itself as the
	// properties; these fields let mustJSON hand it to a typed decode.
	ID         string          `json:"id,omitempty"`
	Permission string          `json:"permission,omitempty"`
	Patterns   []string        `json:"patterns,omitempty"`
	Always     []string        `json:"always,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Questions  json.RawMessage `json:"questions,omitempty"`
	Tool       json.RawMessage `json:"tool,omitempty"`
}

// onEvent maps one opencode event onto the socket. Only the current
// session's events pass; a session created in the instance folder becomes
// the current one (the TUI opened a new conversation) with a fresh hello.
func (c *chatConn) onEvent(ev agent.OpencodeEvent) {
	var p eventProps
	if err := json.Unmarshal(ev.Props, &p); err != nil {
		log.Printf("agent chat %s: %s event unreadable: %v", c.paneID, ev.Type, err)
		return
	}
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
	case "permission.asked", "permission.replied", "question.asked", "question.replied", "question.rejected":
		return requestFrame(kind, p)
	case "session.idle":
		return chatFrame{T: "idle"}, true
	case "session.error":
		return chatFrame{T: "error", Error: string(p.Error)}, true
	}
	return chatFrame{}, false
}

// requestFrame is opencode asking the human something (a permission before
// a tool runs, a question from the question tool) or reporting it answered.
func requestFrame(kind string, p eventProps) (chatFrame, bool) {
	switch kind {
	case "permission.asked":
		var req agent.PermissionRequest
		if err := json.Unmarshal(requestJSON(p), &req); err != nil || req.ID == "" {
			log.Printf("agent chat: permission.asked unreadable (%v); the agent waits on a request nobody sees", err)
			return chatFrame{}, false
		}
		return chatFrame{T: "permission", Permission: &req}, true
	case "permission.replied":
		return chatFrame{T: "permission-replied", ID: p.RequestID, Reply: p.Reply}, true
	case "question.asked":
		var req agent.QuestionRequest
		if err := json.Unmarshal(requestJSON(p), &req); err != nil || req.ID == "" {
			log.Printf("agent chat: question.asked unreadable (%v); the agent waits on a question nobody sees", err)
			return chatFrame{}, false
		}
		return chatFrame{T: "question", Question: &req}, true
	default:
		return chatFrame{T: "question-replied", ID: p.RequestID}, true
	}
}

// transcriptFrame is one change to the transcript: a turn (its header), a
// whole part, or a streamed delta to one part's field.
func transcriptFrame(kind string, p eventProps) (chatFrame, bool) {
	switch kind {
	case "message.updated":
		t, err := agent.NormalizeInfo(p.Info)
		if err != nil {
			log.Printf("agent chat: message.updated unreadable: %v", err)
			return chatFrame{}, false
		}
		return chatFrame{T: "turn", Turn: &t}, true
	case "message.part.updated":
		mid, part, ok := agent.NormalizePart(p.Part)
		if !ok {
			return chatFrame{}, false // a kind the transcript does not show, or garbage NormalizePart logged
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
	if info.ID == "" || !agent.SameDir(info.Directory, c.cwd) {
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
	hello, err := c.liveHello(ctx, oc)
	if err != nil {
		log.Printf("agent chat %s: switched to session %s but its history failed: %v", c.paneID, info.ID, err)
		c.send(chatFrame{T: "error", Error: "new conversation started, but its history could not be read: " + err.Error()})
		return
	}
	c.send(hello)
}

// requestJSON re-encodes the event properties of permission.asked and
// question.asked, which ARE the request object, for the typed decode.
func requestJSON(p eventProps) []byte {
	b, _ := json.Marshal(p)
	return b
}

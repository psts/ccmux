package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"

	"ccmux.dev/ccmuxd/internal/agent"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/runner"
	"ccmux.dev/ccmuxd/internal/store"
)

// Role agents as sessions (task #177). A role agent on the claude harness
// starts as a runner session (internal/runner) where it used to be a
// sidecar typed into a tmux pane: its home is still its workspace in the
// window, with no tmux session behind it, and the home has one session,
// kept across sleeps (store/agentsessions.go).
//
// Which way an instance goes is decided by where it is, not only by its
// harness (onSession): a claude agent that is running in a tmux pane right
// now, started before this, keeps every pane path (its chat port, the push
// into it, ctrl-d to sleep) until that run has ended. It moves at its next
// start. A daemon with no session runners (macOS, no systemd) starts every
// agent in a pane, as before.
//
// The flows here are the session twins of the pane's in agents_ws.go, and
// that file's startAgent and startWindowAgent hand over to them first.

// agentSender is who a prompt the daemon gives an agent's session is from,
// in its inbox: the first message of a start.
const agentSender = "ccmux"

// agentMode is the permission mode an agent's session runs in: it asks by
// its base's gates, as it did in a pane.
const agentMode = "default"

// agentAddMu makes "is the agent in this window, and if not add it" one
// step. Two starts of an agent that is not there yet (a click and a
// message) would otherwise both make it a home.
var agentAddMu sync.Mutex

// agentMoveMu makes taking a home off tmux and making its session one step.
var agentMoveMu sync.Mutex

// onSession says whether base d's instance, whose home is ws (nil when it
// has none in the window yet), runs as a session: this daemon runs
// sessions, the base is on the claude harness, and the instance is not
// running in a tmux pane.
func (s *Server) onSession(d agent.Definition, ws *model.Workspace) bool {
	if s.runners == nil || s.runnerRows == nil || s.mgr.Harnesses == nil {
		return false
	}
	h, err := s.mgr.Harnesses.Resolve(d.Harness)
	if err != nil || h.Name != harness.Builtin {
		return false
	}
	return ws == nil || !s.mgr.TmuxAgentUp(ws.ID)
}

// startAgentSession is startAgent for an instance that runs as a session;
// false hands the start back to the pane's flow, which also words the
// refusals for a session that is not this agent's.
func (s *Server) startAgentSession(wsID, name, prompt string, resume *bool) (startOutcome, bool) {
	ws := s.mgr.Workspace(wsID)
	if ws == nil || ws.Agent != name {
		return startOutcome{}, false
	}
	d, _, msg := s.agentDefinition(name)
	if msg != "" || !s.onSession(d, ws) {
		return startOutcome{}, false
	}
	return s.wakeAgentSession(ws, d, prompt, resume), true
}

// addAgentSession adds base d to window win as a session: its home (no
// tmux session), its place in the window, then its session, started with
// prompt as its first message.
func (s *Server) addAgentSession(win manager.WindowInfo, d agent.Definition, prompt, createdBy string, resume *bool) startOutcome {
	agentAddMu.Lock()
	defer agentAddMu.Unlock()
	if ws := s.agentWorkspace(win, d.Name); ws != nil {
		return s.startExistingInstance(ws, d, prompt, resume) // added by another start while this one waited
	}
	if msg := s.agentCapMessage(); msg != "" {
		return startOutcome{status: http.StatusConflict, msg: msg, err: manager.ErrAgentCap}
	}
	if _, _, msg := s.windowDirs(win); msg != "" {
		return startOutcome{status: http.StatusInternalServerError, msg: msg}
	}
	dir := s.agents.InstanceDir(win.ID, win.Name, d.Name)
	if err := s.agentPreflight(d, dir); err != nil {
		return startOutcome{status: http.StatusServiceUnavailable, msg: "agent " + d.Name + ": " + err.Error()}
	}
	ws, err := s.mgr.CreateAgentHome(agentSessionName(d), createdBy, win.Name, d.Name, dir)
	if err != nil {
		return startOutcome{status: http.StatusInternalServerError, msg: err.Error(), err: err}
	}
	if err := s.mgr.SeedWindowMembership(ws.ID, win.Name); err != nil {
		// As for a pane's session: without the row the window never finds
		// this home again, and the next add would make a second one.
		if kerr := s.mgr.KillWorkspace(ws.ID); kerr != nil {
			log.Printf("agent %s: home %s left behind after a membership failure: %v", d.Name, ws.ID, kerr)
		}
		return startOutcome{status: http.StatusInternalServerError, msg: "window membership: " + err.Error(), err: err}
	}
	out := s.wakeAgentSession(ws, d, prompt, resume)
	if out.msg == "" {
		out.status = http.StatusCreated
	}
	return out
}

// wakeAgentSession starts the session of the agent whose home is ws: made
// when the home has none (its first start as a session, or after a person
// ended the last one), woken when it is asleep, refused when it runs.
func (s *Server) wakeAgentSession(ws *model.Workspace, d agent.Definition, prompt string, resume *bool) startOutcome {
	sess, err := s.runnerRows.AgentSession(ws.ID)
	if err != nil {
		return startOutcome{status: http.StatusServiceUnavailable, msg: "agent " + d.Name + ": its session was not read: " + err.Error()}
	}
	if sess == nil {
		return s.newAgentSession(ws, d, prompt, resume)
	}
	pane := s.mgr.PaneByID(sess.PaneID)
	if sess.Wanted == store.SessionRunning {
		return startOutcome{pane: pane, status: http.StatusConflict, msg: "agent " + d.Name + " is running in this window: message it in its session", err: manager.ErrAgentRunning}
	}
	if msg := s.agentCapMessage(); msg != "" {
		return startOutcome{status: http.StatusConflict, msg: msg, err: manager.ErrAgentCap}
	}
	if resume != nil {
		// This start says itself which conversation it wants; without that,
		// what the sleep left (the base's start setting) stands.
		if err := s.runners.SetFresh(sess.ID, !*resume); err != nil {
			return startOutcome{status: http.StatusInternalServerError, msg: err.Error()}
		}
	}
	if err := s.promptOrWake(sess.ID, prompt); err != nil {
		return startOutcome{status: http.StatusInternalServerError, msg: "agent " + d.Name + ": " + err.Error()}
	}
	return startOutcome{pane: pane, status: http.StatusOK}
}

// promptOrWake wakes a session with prompt as a message, or with nothing
// to do when there is none (a keep-alive wake).
func (s *Server) promptOrWake(sessionID, prompt string) error {
	if prompt == "" {
		return s.runners.SetWanted(sessionID, store.SessionRunning)
	}
	_, err := s.runners.Send(sessionID, prompt, agentSender)
	return err
}

// newAgentSession makes the session of a home that has none. A home that
// was a tmux session until now is taken off tmux first, and the session's
// pane takes over the old agent pane's id: the agent keeps its peer on the
// bus, what waits for it there, and its llm route.
func (s *Server) newAgentSession(ws *model.Workspace, d agent.Definition, prompt string, resume *bool) startOutcome {
	if msg := s.agentCapMessage(); msg != "" {
		return startOutcome{status: http.StatusConflict, msg: msg, err: manager.ErrAgentCap}
	}
	if err := s.agentPreflight(d, ws.RepoPath); err != nil {
		return startOutcome{status: http.StatusServiceUnavailable, msg: "agent " + d.Name + ": " + err.Error()}
	}
	conv, err := s.conversationToContinue(d, ws.RepoPath, resume)
	if err != nil {
		return startOutcome{status: http.StatusBadGateway, msg: err.Error()}
	}
	sess, refusal := s.makeAgentSession(ws, d, conv)
	if refusal.msg != "" {
		return refusal
	}
	if prompt != "" {
		if _, err := s.runners.Send(sess.ID, prompt, agentSender); err != nil {
			return startOutcome{status: http.StatusInternalServerError, msg: "agent " + d.Name + " started, but its first message was not queued: " + err.Error()}
		}
	}
	log.Printf("agent %s: runs as session %s (home %s)", d.Name, sess.ID, ws.ID)
	return startOutcome{pane: s.mgr.PaneByID(sess.PaneID), status: http.StatusOK}
}

// makeAgentSession makes a home's session and takes the home off tmux, as
// one step: of two starts racing here, the second is told the agent is
// being started, which the bus reads as "deliver to it".
//
// The order is what keeps the agent's identity. The tmux pane's id is read
// first and handed to the session; the tmux session is closed only once the
// session exists. A session that could not be made leaves the home as it
// was, pane and all, for the next start to move.
func (s *Server) makeAgentSession(ws *model.Workspace, d agent.Definition, conv string) (store.RunnerSession, startOutcome) {
	agentMoveMu.Lock()
	defer agentMoveMu.Unlock()
	if made, err := s.runnerRows.AgentSession(ws.ID); err != nil || made != nil {
		return store.RunnerSession{}, agentBeingStarted(s.mgr, d.Name, made, err)
	}
	paneID, err := s.mgr.TmuxAgentPane(ws.ID)
	if err != nil {
		return store.RunnerSession{}, startOutcome{status: agentErrStatus(err), msg: "agent " + d.Name + ": " + err.Error(), err: err}
	}
	if err := s.settleMailbox(paneID); err != nil {
		return store.RunnerSession{}, startOutcome{status: http.StatusServiceUnavailable, msg: "agent " + d.Name + ": what the bus already sent its pane could not be marked read, so it is not moved onto a session yet: " + err.Error()}
	}
	sess, err := s.runners.Create(runner.NewSession{
		Cwd: ws.RepoPath, Name: d.Name, Model: d.Model, WorkspaceID: ws.ID, Kind: store.SessionAgent, Title: d.Name,
		PermissionMode: agentMode, PaneID: paneID, Conversation: conv,
	})
	if err != nil {
		return sess, startOutcome{status: http.StatusInternalServerError, msg: "agent " + d.Name + ": " + err.Error(), err: err}
	}
	if err := s.mgr.RetireAgentTmux(ws.ID); err != nil {
		log.Printf("agent %s: runs as session %s, but its old tmux session was not closed: %v", d.Name, sess.ID, err)
	}
	return sess, startOutcome{}
}

// agentBeingStarted answers a start that found the home's session made by
// another start a moment ago, or could not read whether it was.
func agentBeingStarted(mgr *manager.Manager, name string, made *store.RunnerSession, err error) startOutcome {
	if err != nil {
		return startOutcome{status: http.StatusServiceUnavailable, msg: "agent " + name + ": its session was not read: " + err.Error()}
	}
	return startOutcome{pane: mgr.PaneByID(made.PaneID), status: http.StatusConflict, msg: "agent " + name + " is being started in this window", err: manager.ErrAgentRunning}
}

// settleMailbox marks what the bus already sent an agent's tmux pane as
// read, before its session takes the pane's id over and, with it, the
// mailbox (peers.SettlePaneMailbox). A home with no tmux pane has none.
func (s *Server) settleMailbox(tmuxPane string) error {
	if tmuxPane == "" || s.peersSvc == nil {
		return nil
	}
	return s.peersSvc.SettlePaneMailbox(tmuxPane)
}

// conversationToContinue is the conversation a new session of this home
// picks up: the home's newest, when the start asked to continue or the base
// says so, which is how an agent that moves off tmux keeps its
// conversation; "" starts a new one.
func (s *Server) conversationToContinue(d agent.Definition, dir string, resume *bool) (string, error) {
	want := d.Start == "continue"
	if resume != nil {
		want = *resume
	}
	if !want {
		return "", nil
	}
	// The sidecar first: the history is read through it.
	if _, _, err := s.ensureSidecar(); err != nil {
		return "", err
	}
	return s.resumeSession(d, harness.Harness{Name: harness.Builtin}, dir, resume)
}

// AgentStart is what one start of an agent's session adds to its runner's
// own flags (runner.Config.Agent): everything the pane's launch line said,
// from the base as it is now. It runs at every start, the supervisor's own
// wakes included, so the instance folder, the trust marks and the llm route
// are refreshed here as resolveAgentLaunch refreshes them for a pane. An
// error fails the start, which is tried again and then parked with the
// reason in the session's note.
//
// Called under the session's lock: it must never call the supervisor.
func (s *Server) AgentStart(sess store.RunnerSession) (runner.AgentStart, error) {
	if s.agents == nil || s.mgr.Harnesses == nil {
		return runner.AgentStart{}, errors.New(agentsUnavailable)
	}
	d, err := s.agents.Get(sess.Name)
	if err != nil {
		return runner.AgentStart{}, fmt.Errorf("its base: %w", err)
	}
	h, err := s.mgr.Harnesses.Resolve(d.Harness)
	if err != nil {
		return runner.AgentStart{}, fmt.Errorf("its harness: %w", err)
	}
	if h.Name != harness.Builtin {
		return runner.AgentStart{}, fmt.Errorf("its base now runs on the %s harness, which starts in a terminal: remove this agent from the window and add it again", h.Name)
	}
	opts, err := s.agentSessionOpts(d, h, sess)
	if err != nil {
		return runner.AgentStart{}, err
	}
	vars, problems, err := agent.LoadEnvFiles(opts.EnvFiles)
	if err != nil {
		return runner.AgentStart{}, fmt.Errorf("its .env files: %w", err)
	}
	for _, p := range problems {
		log.Printf("agent %s: %s", d.Name, p)
	}
	return runner.AgentStart{Args: agent.ClaudeRunnerArgs(d, s.agents.Dir(d.Name), opts), Env: vars, Model: d.Model, Version: d.Version}, nil
}

// agentSessionOpts prepares one start of d's session and returns what its
// flags are made from: the window's folders, the env files, the walls and,
// for an isolated base, its capabilities.
func (s *Server) agentSessionOpts(d agent.Definition, h harness.Harness, sess store.RunnerSession) (agent.LaunchOpts, error) {
	dirs, shared, _, msg := s.windowDirsOfPane(sess.PaneID)
	if msg != "" {
		return agent.LaunchOpts{}, errors.New(msg)
	}
	route, err := s.agentHomeReady(d, h, sess.Cwd)
	if err != nil {
		return agent.LaunchOpts{}, err
	}
	if err := s.mgr.RouteLLM(sess.PaneID, route); err != nil {
		return agent.LaunchOpts{}, err
	}
	opts, err := launchOpts(d, h.Name, sess.Cwd, dirs, shared, "", 0)
	if err != nil || !d.Isolated {
		return opts, err
	}
	opts.Isolated, err = s.isolatedSession(d)
	return opts, err
}

// isolatedSession resolves an isolated base's capabilities for a session.
// No peers shim is named: the session's runner names the daemon's own.
func (s *Server) isolatedSession(d agent.Definition) (*agent.IsolatedLaunch, error) {
	claudeDir, err := agent.ClaudeConfigDir()
	if err != nil {
		return nil, fmt.Errorf("isolated agent: %w", err)
	}
	in, err := s.agents.IsolatedLaunchFor(d, claudeDir, "")
	if err != nil {
		return nil, fmt.Errorf("isolated agent: %w", err)
	}
	return in, nil
}

// agentHomeReady is what every start does to the instance before its Claude
// comes up, and can fail on: the llm account its base pins (returned, for
// the pane's route), the instance folder, its generated config, and the
// trust marks a headless session cannot be asked for.
func (s *Server) agentHomeReady(d agent.Definition, h harness.Harness, dir string) (route string, err error) {
	route, _, msg := s.agentRoute(d, h)
	if msg != "" {
		return "", errors.New(msg)
	}
	if _, err := agent.Bootstrap(dir, d); err != nil {
		return "", fmt.Errorf("instance folder: %w", err)
	}
	if err := s.agents.WriteInstanceConfig(d, dir); err != nil {
		return "", fmt.Errorf("instance config: %w", err)
	}
	return route, agent.ApproveClaudeProject(s.claudeConfig, dir)
}

// agentPreflight runs, before a session is made, everything its first
// start would fail on that does not need the session: the sidecar, the
// base's account, the instance folder and an isolated base's capabilities.
// A session's start fails quietly (it is tried again, then parked with a
// note on its page), which is right for a wake nobody is waiting on; a
// person who clicked Start, and a peer whose message started the agent,
// must hear "could not be started" as they did from a pane.
func (s *Server) agentPreflight(d agent.Definition, dir string) error {
	h, err := s.mgr.Harnesses.Resolve(d.Harness)
	if err != nil {
		return fmt.Errorf("its harness: %w", err)
	}
	if _, _, err := s.ensureSidecar(); err != nil {
		return err
	}
	if _, err := s.agentHomeReady(d, h, dir); err != nil {
		return err
	}
	if !d.Isolated {
		return nil
	}
	_, err = s.isolatedSession(d)
	return err
}

// SleepAgentSession is the one door that puts an agent's session to sleep:
// the lifecycle loop's idle exit, a person's Sleep, a restart from the
// agent editor and the thread page all come through it, so each of them
// leaves the same answer to "which conversation does it wake in": a new
// one, unless its base says continue.
func (s *Server) SleepAgentSession(sessionID string) error {
	sess, err := s.runnerRows.RunnerSession(sessionID)
	if err != nil || sess == nil {
		return orNoRunner(err)
	}
	return s.runners.Sleep(sessionID, s.agentStartsFresh(sess.Name))
}

// agentStartsFresh is the base's start setting. A base that cannot be read
// reads as fresh, which is the default.
func (s *Server) agentStartsFresh(name string) bool {
	if s.agents == nil {
		return true
	}
	d, err := s.agents.Get(name)
	return err != nil || d.Start != "continue"
}

// setAgentWanted is the thread page's sleep, wake and end for an agent's
// session, each by the agents' own rules: a sleep goes through the door, a
// wake counts against the cap, an end is an end (the home stays, and its
// next start makes a new session). handled is false for any other session,
// and for what the plain route does as well (an end, a wake under the cap).
//
// A row that cannot be read is an error, never "not an agent": the plain
// route would then put an agent to sleep past its door, or wake it past
// the cap.
func (s *Server) setAgentWanted(sessionID, wanted string) (handled bool, err error) {
	sess, err := s.runnerRows.RunnerSession(sessionID)
	if err != nil {
		return true, fmt.Errorf("the session was not read, so nothing was changed: %w", err)
	}
	if sess == nil || sess.Kind != store.SessionAgent {
		return false, nil
	}
	switch wanted {
	case store.SessionAsleep:
		return true, s.SleepAgentSession(sessionID)
	case store.SessionRunning:
		if msg := s.agentCapMessage(); msg != "" && sess.Wanted == store.SessionAsleep {
			return true, errAgentCap{msg}
		}
	}
	return false, nil
}

// errAgentCap is a wake refused by the cap on running agents.
type errAgentCap struct{ msg string }

func (e errAgentCap) Error() string { return e.msg }

// agentWakeHeld says why a message for an asleep agent's session must wait
// in its mailbox instead of waking it: the cap on running agents is
// reached. "" lets it through. The bus hands the message over again at its
// next sweep, so it wakes the agent once another has gone to sleep. A row
// that cannot be read holds the message too, for the same sweep: letting it
// through would wake an agent past the cap.
func (s *Server) agentWakeHeld(sessionID string) string {
	sess, err := s.runnerRows.RunnerSession(sessionID)
	if err != nil {
		return "the session was not read, so it is not woken yet: " + err.Error()
	}
	if sess == nil || sess.Kind != store.SessionAgent || sess.Wanted != store.SessionAsleep {
		return ""
	}
	return s.agentCapMessage()
}

// agentWakeRefused answers 409 for a prompt or a restart from the thread
// page that would wake an asleep agent over the cap, and says whether it
// did. The person sees why, and can put another agent to sleep first.
func (s *Server) agentWakeRefused(w http.ResponseWriter, sessionID string) bool {
	held := s.agentWakeHeld(sessionID)
	if held == "" {
		return false
	}
	writeError(w, http.StatusConflict, held)
	return true
}

// sendToAgentPane hands text to a running agent: as a prompt in its
// session's inbox, or into its pane's chat server.
func (s *Server) sendToAgentPane(paneID, text, sender string) error {
	if info, ok := s.mgr.RunnerPane(paneID); ok && s.runners != nil {
		_, err := s.runners.Send(info.SessionID, text, sender)
		return err
	}
	return s.mgr.PushToAgentPane(paneID, text)
}

// sessionAgentPane answers an old-lens chat route for an agent that runs as
// a session: its conversation is on the thread page, and nothing serves it
// over a pane's chat port. False when p is a tmux pane.
func sessionAgentPane(w http.ResponseWriter, p *model.Pane) bool {
	if p.Runtime != model.RuntimeRunner {
		return false
	}
	writeError(w, http.StatusConflict, "this agent runs as a session: open it at /threads.html?s="+p.SessionID)
	return true
}

// Runner sessions' panes. A runner session (internal/runner) is a Claude
// session on the Agent SDK in its own systemd service: no tmux window, no
// shell. It still has a pane id and a place in its repo's workspace, so
// everything already keyed on a pane id keeps working for it without a
// second, session-keyed path: its bus peer and token, its window group
// (GroupForPane), its attention, the alerts routed by its workspace, and
// its LLM route (/llm/pane/<id>).
//
// The pane is held BESIDE the workspace's Panes, never in them. That list is
// tmux bookkeeping: revive gives every entry a window (revivePanes), the
// last-entry rules archive the session (KillPane, dropPane), a bus spawn
// types a Claude into its newest claude entry (lastClaudePane), and the
// terminal lens draws a tab per entry. A pane with nothing in tmux would be
// wrong in each of them. findPaneLocked looks in both lists, which is all a
// pane-id lookup needs.
//
// The list is a projection of the session rows (store/sessions.go): rebuilt
// at start (LoadRunnerPanes), kept in step by SyncRunnerPane, and never
// written to the panes table, so nothing that walks that table ever meets
// one (savePane is the one way a pane is written).
package manager

import (
	"log"

	"ccmux.dev/ccmuxd/internal/buscontext"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// Liveness is where a pane's session is: running, asleep (it can be woken:
// a prompt or a message starts it again) or done (ended, or no such pane).
type Liveness string

const (
	LiveRunning Liveness = "running"
	LiveAsleep  Liveness = "asleep"
	LiveDone    Liveness = "done"
)

// PaneLiveness is the one answer to "is anyone home in this pane". A runner
// pane asks the supervisor (RunnerLiveness: what its row wants and whether
// its unit is up), and when the supervisor cannot say, answers what its row
// last wanted; a tmux pane keeps the rules it has always had: asleep when
// its workspace is closed or its foreground is a bare shell, running
// otherwise. An unknown pane is done.
func (m *Manager) PaneLiveness(paneID string) Liveness {
	m.mu.RLock()
	e, p := m.findPaneLocked(paneID)
	live, sessionID := LiveDone, ""
	switch {
	case p == nil:
	case p.Runtime == model.RuntimeRunner:
		live, sessionID = runnerLiveness(p), p.SessionID
	case e.ctrl == nil || atBareShell(p):
		live = LiveAsleep
	default:
		live = LiveRunning
	}
	m.mu.RUnlock()
	if sessionID == "" || m.RunnerLiveness == nil {
		return live
	}
	asked, err := m.RunnerLiveness(sessionID) // off the lock: it asks systemd
	if err != nil {
		// What the row last said, and why that is all there is: a supervisor
		// that cannot answer is not a session that ended.
		log.Printf("runner pane %s: liveness not read, answering %q from its row: %v", paneID, live, err)
		return live
	}
	return Liveness(asked)
}

// runnerLiveness is a runner pane's liveness as its row last said it, for
// when no supervisor is wired to ask.
func runnerLiveness(p *model.Pane) Liveness {
	if p.Status == model.StatusLive {
		return LiveRunning
	}
	return LiveAsleep
}

// LoadRunnerPanes rebuilds every runner pane from the session rows, and
// learns the pane ids of sessions that have ended. Called once at start,
// before the hooks socket opens: a hook that names a runner's pane must
// find it, or be dropped, never pinned on another pane (ResolvePane).
func (m *Manager) LoadRunnerPanes() error {
	ids, err := m.store.RunnerPaneIDs()
	if err != nil {
		return err
	}
	rows, err := m.store.RunnerSessions()
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, id := range ids {
		m.runnerIDs[id] = true
	}
	m.mu.Unlock()
	for _, sess := range rows {
		m.SyncRunnerPane(sess)
	}
	return nil
}

// SyncRunnerPane brings a session's pane in step with its row: made when
// the session is, renamed and moved with it, gone once it has ended. A session with
// no pane id has none. One whose workspace is gone (deleted under it) is
// logged and left without a pane: it runs, off the bus.
func (m *Manager) SyncRunnerPane(sess store.RunnerSession) {
	if sess.PaneID == "" {
		return
	}
	if sess.Wanted == store.SessionDone {
		m.dropRunnerPane(sess.PaneID)
		return
	}
	m.mu.Lock()
	m.runnerIDs[sess.PaneID] = true
	e := m.byID[sess.WorkspaceID]
	if e == nil {
		m.mu.Unlock()
		log.Printf("runner session %s: its workspace %s is gone; it runs with no pane (no bus, no alerts)", sess.ID, sess.WorkspaceID)
		return
	}
	p := runnerPaneIn(e, sess.PaneID)
	made := p == nil
	if made {
		p = newRunnerPane(sess)
		e.runners = append(e.runners, p)
	}
	p.Title = m.runnerNameLocked(sess, e.ws)
	p.CWD = sess.Cwd // a thread member moves into its worktree, and back
	p.Status = model.StatusCold
	if sess.Wanted == store.SessionRunning {
		p.Status = model.StatusLive
	}
	stampRunnerAgent(p, sess)
	noteMainPane(e, sess)
	notePlannerPane(e, sess)
	pane := *p
	m.mu.Unlock()
	if made {
		m.seedAgentClock(pane)
	}
}

// newRunnerPane is a session's pane as its row describes it. The attention
// is the one the last daemon recorded; a row that never had one is idle.
func newRunnerPane(sess store.RunnerSession) *model.Pane {
	att := model.Attention(sess.Attention)
	if att == "" {
		att = model.AttentionIdle
	}
	return &model.Pane{
		ID: sess.PaneID, WorkspaceID: sess.WorkspaceID, Runtime: model.RuntimeRunner, SessionID: sess.ID,
		CWD: sess.Cwd, Harness: harness.Builtin, HostedClaude: true, CreatedBy: runnerStamp(sess), CreatedAt: sess.CreatedAt,
		Attention: att, AttentionReason: model.AttentionReason(sess.AttentionReason), AttentionSince: sess.AttentionSince,
	}
}

// noteMainPane keeps which of a workspace's runner panes is the repo's main
// session: its session on the thread page, of kind lead and in no thread
// (the rule store.SideLeads reads the rows by, planners aside). The bus hands that
// session what is sent to the repo's plain name (peers/mainsession.go).
func noteMainPane(e *entry, sess store.RunnerSession) {
	switch {
	case sess.Kind == store.SessionLead && sess.ThreadID == 0:
		e.mainPane = sess.PaneID
	case e.mainPane == sess.PaneID:
		e.mainPane = ""
	}
}

// notePlannerPane keeps which of a workspace's runner panes is the repo's
// planner (store/planners.go): never its main session, and the bus never
// hands a planner's by-name message to a repo's main session
// (peers/mainsession.go).
func notePlannerPane(e *entry, sess store.RunnerSession) {
	switch {
	case sess.Kind == store.SessionPlanner:
		e.plannerPane = sess.PaneID
	case e.plannerPane == sess.PaneID:
		e.plannerPane = ""
	}
}

// runnerNameLocked is a session's name on the bus: the row's name when it
// has one, else "<repo folder>-s-<first 8 of its id>". Never the plain name
// of a repo folder: a peer registered under exactly that name (a terminal
// Claude a person started in the repo) keeps answering to it. With no such
// peer, the bus hands the plain name's messages to the repo's main session
// (RunnerPaneInfo.Main, peers/mainsession.go).
func (m *Manager) runnerNameLocked(sess store.RunnerSession, ws *model.Workspace) string {
	if sess.Name != "" && !m.repoNamedLocked(sess.Name) {
		return sess.Name
	}
	id := sess.ID
	if len(id) > 8 {
		id = id[:8]
	}
	return baseName(ws.RepoPath) + "-s-" + id
}

// repoNamedLocked reports whether some repo workspace answers to name.
func (m *Manager) repoNamedLocked(name string) bool {
	for _, e := range m.byID {
		if e.ws.Agent == "" && repoNamed(e.ws, name) {
			return true
		}
	}
	return false
}

func runnerPaneIn(e *entry, paneID string) *model.Pane {
	for _, p := range e.runners {
		if p.ID == paneID {
			return p
		}
	}
	return nil
}

// dropRunnerPane forgets an ended session's pane and everything keyed on it.
// Its id stays known (runnerIDs), so a late hook from it resolves nowhere.
func (m *Manager) dropRunnerPane(paneID string) {
	m.restart.forget(paneID)
	m.runnerReminders.set(paneID, false, nil)
	pushLocks.Delete(paneID)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runnerIDs[paneID] = true
	for _, e := range m.byID {
		kept := e.runners[:0]
		for _, p := range e.runners {
			if p.ID != paneID {
				kept = append(kept, p)
			}
		}
		e.runners = kept
		if e.mainPane == paneID {
			e.mainPane = ""
		}
		if e.plannerPane == paneID {
			e.plannerPane = ""
		}
	}
}

// RunnerPaneInfo is what the rest of the daemon needs to know about a
// runner session's pane: the name the daemon gives it on the bus, its
// session's id, its workspace's repo folder, whether its row says it is
// asleep, and whether it is the repo's main session (noteMainPane) or its
// planner (notePlannerPane).
type RunnerPaneInfo struct {
	Name, SessionID, Repo string
	Asleep, Main, Planner bool
}

// RunnerPane reports a runner session's pane. ok is false for a tmux pane,
// an unknown one, and a session that has ended. The peers bus asks this
// under its own lock, so it reads memory only.
func (m *Manager) RunnerPane(paneID string) (RunnerPaneInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, p := m.findPaneLocked(paneID); p != nil && p.Runtime == model.RuntimeRunner {
		return RunnerPaneInfo{
			Name: p.Title, SessionID: p.SessionID, Repo: e.ws.RepoPath, Asleep: p.Status != model.StatusLive,
			Main: e.mainPane == p.ID, Planner: e.plannerPane == p.ID,
		}, true
	}
	return RunnerPaneInfo{}, false
}

// SessionsChanged tells every lens that a runner session of workspace wsID
// was made, changed (woken, put to sleep, renamed) or ended, so a page that
// lists sessions reads them again. What changed is not said: the list is
// small and the rows are the truth. A session's attention has its own
// event.
func (m *Manager) SessionsChanged(wsID string) {
	m.events.publish(Event{Kind: "sessions-changed", WorkspaceID: wsID})
}

// hasRunnerPanes reports whether a workspace holds a runner session that has
// not ended. Such a workspace is being worked in whether or not its
// terminal session is open, so its git state is kept fresh (gitstatus.go).
func (m *Manager) hasRunnerPanes(wsID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	return e != nil && len(e.runners) > 0
}

// RunnerPanes is a copy of every runner pane, for the attention snapshot a
// lens gets at connect.
func (m *Manager) RunnerPanes() []model.Pane {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.Pane
	for _, e := range m.byID {
		for _, p := range e.runners {
			out = append(out, *p)
		}
	}
	return out
}

// RunnerWorkspaceFor finds the workspace a new session in cwd belongs to:
// the repo workspace (open or closed, never an agent's) whose folder is cwd
// or holds it, the deepest when several do, an open one before a closed
// one. "" when no workspace holds the folder.
func (m *Manager) RunnerWorkspaceFor(cwd string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best *entry
	for _, e := range m.byID {
		if e.ws.Agent != "" || !holdsFolder(e.ws.RepoPath, cwd) {
			continue
		}
		if best == nil || betterHome(e, best) {
			best = e
		}
	}
	if best == nil {
		return ""
	}
	return best.ws.ID
}

// holdsFolder reports whether dir is repo or a folder inside it.
func holdsFolder(repo, dir string) bool {
	if repo == "" {
		return false
	}
	return dir == repo || (len(dir) > len(repo) && dir[:len(repo)] == repo && dir[len(repo)] == '/')
}

// betterHome orders two workspaces that both hold a folder: the deeper
// repo, then the open one, then the older (so the answer does not change
// with map order).
func betterHome(e, than *entry) bool {
	if a, b := len(e.ws.RepoPath), len(than.ws.RepoPath); a != b {
		return a > b
	}
	if a, b := e.ctrl != nil, than.ctrl != nil; a != b {
		return a
	}
	return e.ws.CreatedAt < than.ws.CreatedAt
}

// RunnerEnv is what a runner session's environment says about its pane: the
// variables a tmux pane's shell gets that apply to Claude (claudeEnv), its
// pane id, and the bus shim told to poll, since a channel push never reaches
// the model of an SDK session (its messages arrive as prompts instead).
func (m *Manager) RunnerEnv(paneID string) map[string]string {
	env := map[string]string{"CCMUX_PANE_ID": paneID, "CCMUX_PEERS_CHANNEL": "0"}
	m.claudeEnv(env, paneID)
	return env
}

// llmProxyBase is where a pane's LLM traffic goes: the proxy's own service
// when it has one (LLMProxyURL), so that a daemon restart cannot cut a reply
// being streamed; this daemon otherwise. A pane keeps the address it started
// with, which is why the daemon's own /llm/pane route stays mounted either
// way.
func (m *Manager) llmProxyBase() string {
	if m.LLMProxyURL != "" {
		return m.LLMProxyURL
	}
	return m.LocalURL
}

// claudeEnv adds what Claude Code and its hooks need to know about the pane
// they run in: the daemon, the pane's LLM route through it, the hooks
// socket, the raised cap on MCP instructions, and the extras (the bus
// token). Shared by tmux panes (paneEnv) and runner sessions (RunnerEnv),
// so the two can never drift apart.
func (m *Manager) claudeEnv(env map[string]string, paneID string) {
	// Claude Code is told to keep more of an MCP server's instructions than its
	// default, which cut the bus's window lists short (see buscontext.CapEnv).
	env[buscontext.CapEnv] = buscontext.RaisedCapValue
	if m.LocalURL != "" {
		env["CCMUX_DAEMON_URL"] = m.LocalURL
		// Every pane's LLM traffic flows through the daemon's routing proxy.
		// The URL is decision-free on purpose: pane env freezes here and tmux
		// sessions outlive daemon restarts, so WHICH upstream answers is
		// resolved per request from settings (internal/llmproxy). Unrouted,
		// the proxy is a pure Anthropic pass-through — auth untouched, so a
		// Max OAuth login behaves exactly as without it. Injected even into
		// plain shells: a claude started by hand in a hosted pane should route
		// the same as one ccmux started. The trade: while the proxy is down,
		// pane LLM calls fail instead of reaching Anthropic directly. The off
		// switch is the shell itself — an `export ANTHROPIC_BASE_URL=…` in the
		// user's rc runs after tmux stamps this and wins.
		env["ANTHROPIC_BASE_URL"] = m.llmProxyBase() + "/llm/pane/" + paneID
		// Claude Code gates its model catalog and the 1M-context beta on the
		// base URL being api.anthropic.com. Pointing it at our loopback proxy
		// silently downgraded every hosted pane: a static built-in model list
		// instead of the account's real one, and a 200k window instead of 1M.
		// This flag is the CLI's own escape hatch — it short-circuits that host
		// check (verified in the 2.1.251 binary, where the three "first party"
		// reasons are this var, a first-party host, and an unset base URL).
		//
		// Set whenever the proxy URL is, and blind to which upstream answers:
		// pane env freezes here, while the upstream is resolved per request
		// from settings, so there is no route to condition on that stays true. It is honest for the default,
		// keyless pass-through to Anthropic, which is what nearly every pane
		// is. Route a pane to a non-Anthropic upstream and the claim goes
		// stale: the CLI sends first-party-only betas and asks for a catalog
		// that upstream has never heard of. Both degrade (headers ignored,
		// catalog falls back to the static list) rather than break, which is
		// the better trade than downgrading every pane all the time.
		env["_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"] = "1"
	}
	if m.HooksSocket != "" {
		env["CCMUX_HOOKS_SOCK"] = m.HooksSocket
	}
	if m.ExtraPaneEnv != nil {
		for k, v := range m.ExtraPaneEnv(paneID) {
			env[k] = v
		}
	}
}

// savePane is the one way the manager writes a pane. A runner pane's only
// stored state is its attention, on its session row; written to the panes
// table it would come back at the next start as a tmux pane, and a revive
// would give it a window.
func (m *Manager) savePane(p *model.Pane) error {
	if p.Runtime != model.RuntimeRunner {
		return m.store.SavePane(p)
	}
	return m.store.SaveRunnerAttention(p.ID, string(p.Attention), string(p.AttentionReason), p.AttentionSince)
}

// endRunnerSessions ends the runner sessions of a workspace that is being
// deleted: with their workspace gone they would run on with no pane, off
// the bus and unseen. Their transcripts stay on disk.
func (m *Manager) endRunnerSessions(panes []*model.Pane) {
	for _, p := range panes {
		m.dropRunnerPane(p.ID)
		if m.EndRunnerSession != nil {
			go func(id string) {
				if err := m.EndRunnerSession(id); err != nil {
					log.Printf("session runners: session %s of a deleted workspace was not ended: %v", id, err)
				}
			}(p.SessionID)
		}
	}
}

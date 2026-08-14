// Package manager orchestrates the daemon: it owns the tmux Server, the durable
// Store, and one live session.Controller per hosted workspace, and exposes the
// operations the API serves (create/spawn/list/attach/kill).
package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"ccmux.dev/ccmuxd/internal/autoconfirm"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/session"
	"ccmux.dev/ccmuxd/internal/shellint"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

const defaultCols, defaultRows = 80, 24

// maxCols/maxRows bound what a lens may drive a pane to. Measured on tmux 3.6b:
// `resize-window -x 20000` fails outright with "width too large" and leaves the
// size unchanged, so anything past 10000 is not a size tmux will ever hold. (The
// `new-session` path behaves differently — it clamps 20000 to 10000 and only
// errors at 65536 — which is why the limit is stated in terms of resize-window,
// the call ResizePane actually makes.) Rejecting here turns a per-resize tmux
// error into one clear rejection at the API edge.
const maxCols, maxRows = 10000, 10000

type entry struct {
	ws   *model.Workspace
	ctrl *session.Controller // nil when cold
}

// Manager is the daemon's core. Safe for concurrent use.
type Manager struct {
	server *tmux.Server
	store  store.Store
	ctx    context.Context
	events *firehose

	// LocalURL is the daemon's own loopback base URL (e.g. http://127.0.0.1:7890),
	// injected into every hosted pane's env as CCMUX_DAEMON_URL so on-host hooks
	// (the git co-author trailer) reach the daemon with zero config. Empty = omit.
	LocalURL string

	// HooksSocket is the daemon's Claude Code hooks Unix socket path, injected into
	// every hosted pane's env as CCMUX_HOOKS_SOCK so ccmux-notify.sh sends hosted
	// panes' attention to the DAEMON — not to the native app, which listens on its
	// own /tmp/ccmux-hooks.sock for local panes. This keeps the two hook streams
	// separate: without it the last binder of a shared path stole the other's
	// hooks (hosted attention flash died whenever the app ran). Empty = omit.
	HooksSocket string
	// SessionSink receives Claude session lifecycle signals once a hook has been
	// resolved to a pane (wired to the peers bus at startup). A plain func keeps
	// the manager from importing the bus for one call.
	SessionSink func(paneID, sessionID string, sig model.SessionSignal)
	// SessionLiveFn asks the peers bus whether a pane holds a running Claude
	// session, which is how a non-shell foreground is told apart: Claude at work,
	// or the pane repurposed for something else entirely.
	SessionLiveFn func(paneID string) bool

	// ExtraPaneEnv, when set, contributes additional per-pane env vars (the peers
	// bus injects its bearer token here). Set once at startup, before any pane is
	// created. Nil = no extras.
	ExtraPaneEnv func(paneID string) map[string]string

	// OnDevhostChange, when set, is invoked (on its own goroutine) after any
	// dev-hostname or dev-setting mutation so the devhost server reconciles.
	// Set once at startup. Nil = no dev serving.
	OnDevhostChange func()

	// paneTitleDefaults holds the #{pane_title} values that mean "no program set
	// a title" (the tmux host's name) — see panetitle.go.
	paneTitleDefaults map[string]bool

	mu   sync.RWMutex
	byID map[string]*entry
}

// New builds a Manager. ctx bounds the lifetime of spawned control connections.
func New(ctx context.Context, server *tmux.Server, st store.Store) *Manager {
	return &Manager{
		server: server, store: st, ctx: ctx, events: newFirehose(),
		byID: map[string]*entry{}, paneTitleDefaults: defaultPaneTitles(),
	}
}

// SubscribeEvents registers a global firehose consumer (the /v1/events endpoint).
// The returned channel is closed by UnsubscribeEvents.
func (m *Manager) SubscribeEvents() (int, <-chan Event) { return m.events.subscribe() }

// UnsubscribeEvents drops a firehose consumer.
func (m *Manager) UnsubscribeEvents(id int) { m.events.unsubscribe(id) }

// Start brings up the tmux server and reconciles the registry against live
// sessions: existing managed sessions are adopted (control connection reopened),
// the rest are marked cold and await an explicit revive.
func (m *Manager) Start() error {
	if err := m.server.EnsureStarted(); err != nil {
		return err
	}
	// The server may predate this daemon (it survives restarts/upgrades) and
	// -f only applies at spawn: re-source so config changes reach it. Non-fatal
	// — a sourcing error must not keep sessions dark.
	if err := m.server.SourceConfig(); err != nil {
		log.Printf("tmux source-file: %v", err)
	}
	saved, err := m.store.Load()
	if err != nil {
		return err
	}
	live := map[string]bool{}
	metas, _ := m.server.ListManaged()
	for _, meta := range metas {
		live[meta.WorkspaceID] = true
	}
	for _, ws := range saved {
		m.adopt(ws, live[ws.ID])
	}
	return nil
}

func (m *Manager) adopt(ws *model.Workspace, isLive bool) {
	if !isLive {
		ws.Status = model.StatusCold
		_ = m.store.SetWorkspaceStatus(ws.ID, model.StatusCold)
		m.mu.Lock()
		m.byID[ws.ID] = &entry{ws: ws}
		m.mu.Unlock()
		return
	}
	ctrl, err := session.Open(m.ctx, m.server, ws.TmuxSession, ws.ID)
	if err != nil {
		// The tmux session IS alive — we just could not attach to it. Saying
		// nothing here is how a workspace full of running Claudes disappears from
		// every lens with no trace of why; the reconciler retries it shortly.
		log.Printf("adopt: workspace %s (%s) is live in tmux but attach failed: %v",
			ws.ID, ws.TmuxSession, err)
		ws.Status = model.StatusCold
		m.mu.Lock()
		m.byID[ws.ID] = &entry{ws: ws}
		m.mu.Unlock()
		return
	}
	ws.Status = model.StatusLive
	// Persist it. Only the cold branch used to write, so a workspace that ever
	// went cold stayed cold in the registry forever — every later boot adopted it
	// as live in memory while the database kept insisting otherwise.
	_ = m.store.SetWorkspaceStatus(ws.ID, model.StatusLive)
	m.mu.Lock()
	m.byID[ws.ID] = &entry{ws: ws, ctrl: ctrl}
	m.mu.Unlock()
	go m.watch(ws.ID, ctrl)
}

// FallbackStartupCommand is what a new hosted workspace's first pane runs when
// no default has been configured: a peers-enabled claude, so ccmux-created
// sessions get live channel push out of the box (plain `claude` would load the
// peer tools but silently drop pushed messages).
//
// `env -u TMUX` is a cleanup on the clipboard path, NOT what makes copies reach
// the lens. Measured against Claude Code 2.1.224: it writes OSC 52 on every
// copy regardless of $TMUX, and `tmux load-buffer` / pbcopy / xclip are extra
// writes rather than alternatives to it. tmux forwards a pane's OSC 52 to its
// client inside %output either way, so copies already reach the lens with $TMUX
// set — verified end to end against a real hosted pane and an attached lens.
//
// What unsetting it buys: with $TMUX set, claude emits the sequence twice (once
// plain, once wrapped in a tmux DCS passthrough) and awaits `tmux load-buffer`
// for up to 4s before emitting at all. Unsetting drops both.
//
// What it costs: claude's own subprocesses no longer see $TMUX, so a `tmux`
// command one of them runs does not find the ccmux server. TMUX_PANE survives,
// so anything keyed on the pane id is unaffected. In a ccmux pane the daemon
// owns tmux, so that is a deliberate trade.
const FallbackStartupCommand = "env -u TMUX claude --dangerously-load-development-channels server:claude-peers"

const settingStartupCommand = "default_startup_command"

// DefaultStartupCommand returns the configured new-workspace startup command
// (a daemon-wide setting shared by every lens), or the fallback when unset.
func (m *Manager) DefaultStartupCommand() string {
	if v, err := m.store.GetSetting(settingStartupCommand); err == nil && v != "" {
		return v
	}
	return FallbackStartupCommand
}

// SetDefaultStartupCommand persists the setting; empty resets to the fallback.
func (m *Manager) SetDefaultStartupCommand(cmd string) error {
	return m.store.SetSetting(settingStartupCommand, cmd)
}

const settingStartupRules = "startup_rules"

// StartupRule maps a folder subtree to its own new-workspace startup command:
// a rule for ~/Work/Coding/ChartLabs covers every repo under it. Rules beat
// the global default; the longest matching prefix wins among rules.
type StartupRule struct {
	PathPrefix string `json:"pathPrefix"`
	Command    string `json:"command"`
}

// StartupRules returns the configured per-folder rules (empty when unset).
func (m *Manager) StartupRules() []StartupRule {
	raw, err := m.store.GetSetting(settingStartupRules)
	if err != nil || raw == "" {
		return []StartupRule{}
	}
	var rules []StartupRule
	if json.Unmarshal([]byte(raw), &rules) != nil {
		return []StartupRule{}
	}
	return rules
}

// SetStartupRules persists the rules, dropping rows with an empty prefix or
// command (half-filled editor rows, not meaningful rules).
func (m *Manager) SetStartupRules(rules []StartupRule) error {
	kept := make([]StartupRule, 0, len(rules))
	for _, r := range rules {
		r.PathPrefix = strings.TrimRight(strings.TrimSpace(r.PathPrefix), "/")
		r.Command = strings.TrimSpace(r.Command)
		if r.PathPrefix != "" && r.Command != "" {
			kept = append(kept, r)
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	return m.store.SetSetting(settingStartupRules, string(b))
}

// StartupCommandFor resolves the startup command for a new workspace at
// repoPath: longest matching folder rule → global default → built-in fallback.
func (m *Manager) StartupCommandFor(repoPath string) string {
	repoPath = strings.TrimRight(repoPath, "/")
	best, bestLen := "", -1
	for _, r := range m.StartupRules() {
		if len(r.PathPrefix) <= bestLen {
			continue
		}
		if repoPath == r.PathPrefix || strings.HasPrefix(repoPath, r.PathPrefix+"/") {
			best, bestLen = r.Command, len(r.PathPrefix)
		}
	}
	if bestLen >= 0 {
		return best
	}
	return m.DefaultStartupCommand()
}

// CreateWorkspace creates a new hosted workspace with an initial pane. group
// ("" = none) is the shared sidebar group it starts in — set before the
// workspace-added publish so a Mac's adoption sweep can honor it immediately.
func (m *Manager) CreateWorkspace(name, repoPath, cwd, startupCmd, createdBy, group string) (*model.Workspace, error) {
	wsID := uuid.NewString()
	sessionName := model.SessionName(model.Slug(repoPath), wsID)
	if cwd == "" {
		cwd = repoPath
	}
	pane0 := m.newPane(wsID, cwd, startupCmd, createdBy)

	if err := m.server.NewSession(sessionName, cwd, defaultCols, defaultRows, m.paneEnv(pane0.ID)); err != nil {
		return nil, err
	}
	ctrl, err := session.Open(m.ctx, m.server, sessionName, wsID)
	if err != nil {
		return nil, err
	}
	win, tmuxPane, err := ctrl.FirstWindow()
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	if err := ctrl.AdoptWindow(pane0.ID, win, tmuxPane); err != nil {
		ctrl.Close()
		return nil, err
	}
	_ = ctrl.Resize(pane0.ID, defaultCols, defaultRows)
	m.deliverStartup(ctrl, pane0.ID, pane0.StartupCommand)

	ws := &model.Workspace{
		ID: wsID, Name: name, RepoPath: repoPath, CreatedBy: createdBy,
		CreatedAt: nowMillis(), TmuxSession: sessionName, Status: model.StatusLive,
		Group: group, Panes: []*model.Pane{pane0},
	}
	if err := m.store.SaveWorkspace(ws); err != nil {
		return nil, err
	}
	_ = m.store.SavePane(pane0)

	m.mu.Lock()
	m.byID[wsID] = &entry{ws: ws, ctrl: ctrl}
	m.mu.Unlock()
	go m.watch(wsID, ctrl)
	m.events.publish(Event{Kind: "workspace-added", WorkspaceID: wsID})
	return ws, nil
}

// SpawnPane adds a pane (tmux window) to a live workspace.
func (m *Manager) SpawnPane(wsID, cwd, startupCmd, createdBy string) (*model.Pane, error) {
	return m.spawnPane(wsID, cwd, startupCmd, startupCmd, createdBy)
}

// KillPane kills one pane (SIGTERM through tmux) and drops it from the
// workspace — the generic close-a-pane path behind a hosted tab's ✕ in any
// lens. Idempotent: a pane the workspace doesn't hold is a no-op.
//
// The last pane is the exception: rather than drop it and leave a paneless
// workspace, the session is archived and the pane row stays on as its revive
// recipe. See the isLast branch below.
func (m *Manager) KillPane(wsID, paneID string) error {
	e := m.entry(wsID)
	if e == nil {
		return fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	m.mu.RLock()
	held := false
	for _, p := range e.ws.Panes {
		if p.ID == paneID {
			held = true
			break
		}
	}
	isLast := held && len(e.ws.Panes) == 1
	m.mu.RUnlock()
	if !held {
		return nil
	}
	// Closing the last pane ends the session instead of leaving a paneless
	// workspace behind. A zero-pane row cannot be revived (ReviveWorkspace reads
	// pane 0 for the session's cwd) so it lingers in every lens as a cold row
	// that does nothing when clicked. Archiving kills the tmux session and keeps
	// the recipe, which is the same end state as "Close Session" — and unlike a
	// delete it does not destroy the layout, hostnames, or dev command.
	if isLast {
		// Re-checked under the write lock: a pane may have been spawned since the
		// read above, and archiving on the stale count would kill a live session.
		// If it raced, fall through and close the pane the ordinary way.
		_, err := m.archiveIf(wsID, func(e *entry) bool {
			return len(e.ws.Panes) == 1 && e.ws.Panes[0].ID == paneID
		})
		if !errors.Is(err, errArchiveRaced) {
			return err
		}
	}
	if e.ctrl != nil {
		if err := e.ctrl.KillPane(paneID); err != nil {
			return err
		}
	}
	m.dropPane(wsID, paneID)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// SpawnEphemeralPane adds a pane whose startup command is delivered once but
// never persisted: a revive brings the pane back as a plain shell. This is the
// peers-bus teammate spawn — the birth prompt must fire exactly once.
func (m *Manager) SpawnEphemeralPane(wsID, cwd, oneShotCmd, createdBy string) error {
	_, err := m.spawnPane(wsID, cwd, "", oneShotCmd, createdBy)
	return err
}

// spawnPane creates the pane with persistCmd on record while deliverCmd is what
// actually types into the new shell — equal for normal panes, split for
// ephemeral ones.
func (m *Manager) spawnPane(wsID, cwd, persistCmd, deliverCmd, createdBy string) (*model.Pane, error) {
	e := m.entry(wsID)
	if e == nil || e.ctrl == nil {
		return nil, fmt.Errorf("workspace %s not live", wsID)
	}
	if cwd == "" {
		cwd = e.ws.RepoPath
	}
	p := m.newPane(wsID, cwd, persistCmd, createdBy)
	if err := e.ctrl.SpawnWindow(p.ID, cwd, m.paneEnv(p.ID)); err != nil {
		return nil, err
	}
	_ = e.ctrl.Resize(p.ID, defaultCols, defaultRows)
	m.deliverStartup(e.ctrl, p.ID, deliverCmd)

	m.mu.Lock()
	e.ws.Panes = append(e.ws.Panes, p)
	m.mu.Unlock()
	_ = m.store.SavePane(p)
	return p, nil
}

// ReviveWorkspace recreates a cold workspace's tmux session and replays each
// pane's startup command (panes[0] becomes the session's first window). This is
// the resurrection path: tmux died, the SQLite recipe brings it back.
func (m *Manager) ReviveWorkspace(wsID string) (*model.Workspace, error) {
	e := m.entry(wsID)
	if e == nil {
		return nil, fmt.Errorf("unknown workspace %s", wsID)
	}
	if e.ctrl != nil {
		return e.ws, nil // already live
	}
	ws := e.ws
	if len(ws.Panes) == 0 {
		return nil, fmt.Errorf("workspace %s has no panes to revive", wsID)
	}

	pane0 := ws.Panes[0]
	want := m.wantedSizes(ws)
	first := sizeFor(want, pane0.ID)
	if err := m.server.NewSession(ws.TmuxSession, pane0.CWD, first.cols, first.rows, m.paneEnv(pane0.ID)); err != nil {
		return nil, err
	}
	ctrl, err := session.Open(m.ctx, m.server, ws.TmuxSession, wsID)
	if err != nil {
		return nil, err
	}
	applied, err := m.revivePanes(ctrl, ws, want)
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	ws.Status = model.StatusLive

	m.commitRevive(e, ws, ctrl, applied)
	go m.watch(wsID, ctrl)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return ws, nil
}

// commitRevive publishes a revived workspace: the controller and the sizes tmux
// accepted go in under the lock, and the registry is brought up to date after.
//
// The pane copies are taken in that SAME locked pass. Reading the panes again
// after unlocking would hand SavePane a live pointer whose fields another
// goroutine writes under this lock — the thing the lock is here to prevent.
func (m *Manager) commitRevive(e *entry, ws *model.Workspace, ctrl *session.Controller, applied map[string]paneDims) {
	m.mu.Lock()
	e.ctrl = ctrl
	saved := make([]model.Pane, 0, len(ws.Panes))
	for _, p := range ws.Panes {
		if d, ok := applied[p.ID]; ok {
			p.Cols, p.Rows = d.cols, d.rows
		}
		saved = append(saved, *p)
	}
	m.mu.Unlock()

	if err := m.store.SaveWorkspace(ws); err != nil {
		log.Printf("revive %s: persist workspace: %v", ws.ID, err)
	}
	for i := range saved {
		// Logged, because a lost write here is the feature failing quietly: the
		// next revive falls back to 80x24 with nothing to explain why.
		if err := m.store.SavePane(&saved[i]); err != nil {
			log.Printf("revive %s: persist pane %s: %v", ws.ID, saved[i].ID, err)
		}
	}
}

// paneDims is a pane's tmux size. Named rather than a bare pair so the read side
// cannot mix up which number is which.
type paneDims struct{ cols, rows int }

// wantedSizes reads every pane's remembered size in ONE locked pass, because
// ResizePane writes those same two fields under this lock and every read of them
// belongs under it too. (The interleaving is narrow in practice — a resize that
// captured the pre-death controller and is still inside ctrl.Resize — but the
// locking rule does not depend on how narrow it is.)
func (m *Manager) wantedSizes(ws *model.Workspace) map[string]paneDims {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := make(map[string]paneDims, len(ws.Panes))
	for _, p := range ws.Panes {
		cols, rows := paneSize(p)
		want[p.ID] = paneDims{cols, rows}
	}
	return want
}

// sizeFor is the only way to read `want`, so a pane missing from it falls back to
// the default rather than yielding paneDims{}'s 0x0 — a size no tmux call accepts.
func sizeFor(want map[string]paneDims, paneID string) paneDims {
	if d, ok := want[paneID]; ok {
		return d
	}
	return paneDims{defaultCols, defaultRows}
}

// revivePanes rebuilds every pane in the freshly created session: pane0 adopts the
// session's own first window, the rest each get one. Returns the size tmux actually
// accepted per pane, so the caller records only what really happened — a size tmux
// refused must not be written to the registry as fact.
func (m *Manager) revivePanes(ctrl *session.Controller, ws *model.Workspace, want map[string]paneDims) (map[string]paneDims, error) {
	win, tmuxPane, err := ctrl.FirstWindow()
	if err != nil {
		return nil, err
	}
	pane0 := ws.Panes[0]
	if err := ctrl.AdoptWindow(pane0.ID, win, tmuxPane); err != nil {
		return nil, err
	}
	applied := map[string]paneDims{}
	if d := sizeFor(want, pane0.ID); m.sizeAndStart(ctrl, pane0, d) {
		applied[pane0.ID] = d
	}
	for _, p := range ws.Panes[1:] {
		if err := ctrl.SpawnWindow(p.ID, p.CWD, m.paneEnv(p.ID)); err != nil {
			return nil, err
		}
		if d := sizeFor(want, p.ID); m.sizeAndStart(ctrl, p, d) {
			applied[p.ID] = d
		}
	}
	return applied, nil
}

// sizeAndStart sizes a revived pane and then replays its startup command, in that
// order: the program is about to draw itself, and whatever width it draws at is
// baked into its output. Reviving at 80x24 and widening once a lens attaches
// leaves everything already printed wrapped at 80 in a pane that is no longer 80
// wide.
//
// Reports whether tmux accepted the size, so the caller records only sizes that
// really took; a pane whose resize failed keeps whatever the registry already had.
func (m *Manager) sizeAndStart(ctrl *session.Controller, p *model.Pane, d paneDims) (sized bool) {
	if err := ctrl.Resize(p.ID, d.cols, d.rows); err != nil {
		log.Printf("revive: resize pane %s to %dx%d: %v", p.ID, d.cols, d.rows, err)
	} else {
		sized = true
	}
	m.deliverStartup(ctrl, p.ID, p.StartupCommand)
	p.Status = model.StatusLive
	return sized
}

// ArchiveWorkspace kills a workspace's tmux session but KEEPS its registry
// recipe — panes with startup commands, layout blob, hostnames, dev command,
// group — so it goes cold and can be revived later with everything intact.
// Same end state as the tmux server dying ("Close Session" in the lenses).
// Idempotent: an already-cold workspace is a no-op.
func (m *Manager) ArchiveWorkspace(wsID string) (*model.Workspace, error) {
	return m.archiveIf(wsID, func(*entry) bool { return true })
}

// errArchiveRaced reports that the condition justifying an archive stopped holding
// before the write lock was taken. Callers fall back to their non-archive path.
var errArchiveRaced = errors.New("archive precondition no longer holds")

// archiveIf is ArchiveWorkspace with the decision made under the same write lock
// that performs it. A caller deciding on state read under the read lock — "this is
// the last pane" — can be overtaken by a concurrent SpawnPane, and archiving on a
// stale count would kill a session that just gained a pane.
func (m *Manager) archiveIf(wsID string, stillTrue func(*entry) bool) (*model.Workspace, error) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown workspace %s", wsID)
	}
	if !stillTrue(e) {
		m.mu.Unlock()
		return nil, errArchiveRaced
	}
	ctrl := e.ctrl
	session := e.ws.TmuxSession
	e.ctrl = nil // detach before killing so the close-triggered exit notice is a no-op
	e.ws.Status = model.StatusCold
	m.mu.Unlock()
	if ctrl == nil {
		return e.ws, nil // already cold
	}
	ctrl.Close()
	_ = m.server.KillSession(session)
	_ = m.store.SetWorkspaceStatus(wsID, model.StatusCold)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return e.ws, nil
}

// KillWorkspace tears down a workspace's tmux session and registry record. The
// entry is removed *before* the controller is closed so the exit that closing
// triggers doesn't race markCold into publishing a spurious cold-status for a
// workspace that's being deleted — a delete emits only workspace-removed.
func (m *Manager) KillWorkspace(wsID string) error {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return fmt.Errorf("unknown workspace %s", wsID)
	}
	ctrl := e.ctrl
	session := e.ws.TmuxSession
	delete(m.byID, wsID)
	m.mu.Unlock()

	if ctrl != nil {
		ctrl.Close()
	}
	_ = m.server.KillSession(session)
	m.events.publish(Event{Kind: "workspace-removed", WorkspaceID: wsID})
	return m.store.DeleteWorkspace(wsID)
}

// Controller returns the live control connection for a workspace, if any. The
// ctrl field must be read under the lock — markCold nils it from the watch
// goroutine (a race the peers-bus -race suite surfaced).
func (m *Manager) Controller(wsID string) *session.Controller {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e := m.byID[wsID]; e != nil {
		return e.ctrl
	}
	return nil
}

// SetGroup labels a workspace with its shared sidebar group (the owning Mac
// window's name — the Mac app is the source of truth and pushes it here) so
// every lens renders the same grouping. Persisted; broadcast as a
// workspace-status event, which both lenses already answer with a refetch.
func (m *Manager) SetGroup(wsID, group string) error {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return fmt.Errorf("unknown workspace %s", wsID)
	}
	if e.ws.Group == group {
		m.mu.Unlock()
		return nil
	}
	e.ws.Group = group
	m.mu.Unlock()
	if err := m.store.SetWorkspaceGroup(wsID, group); err != nil {
		return err
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// List returns a snapshot of all workspaces.
func (m *Manager) List() []*model.Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*model.Workspace, 0, len(m.byID))
	for _, e := range m.byID {
		out = append(out, e.ws)
	}
	return out
}

// Workspace returns one workspace's metadata.
func (m *Manager) Workspace(wsID string) *model.Workspace {
	if e := m.entry(wsID); e != nil {
		return e.ws
	}
	return nil
}

// ResolvePane maps a Claude Code hook to a ccmux pane id: prefer the explicit
// CCMUX_PANE_ID the hook inherited; otherwise fall back to the pane whose CWD is
// the longest prefix of the hook's cwd (port of the app's longest-repo-prefix
// resolution). Returns "" if nothing matches.
func (m *Manager) ResolvePane(paneID, cwd string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if paneID != "" {
		if _, p := m.findPaneLocked(paneID); p != nil {
			return paneID
		}
	}
	best, bestLen := "", -1
	for _, e := range m.byID {
		for _, p := range e.ws.Panes {
			if p.CWD != "" && strings.HasPrefix(cwd, p.CWD) && len(p.CWD) > bestLen {
				best, bestLen = p.ID, len(p.CWD)
			}
		}
	}
	return best
}

// ErrLayoutConflict is returned by SetLayout when the caller's baseVersion is
// stale (another lens changed the layout first). The caller re-reads and rebases.
var ErrLayoutConflict = errors.New("layout version conflict")

// LayoutUpdate is the payload of a "layout" session event: the new opaque blob
// and the version it produced, so attached lenses re-render the split arrangement.
type LayoutUpdate struct {
	Blob    string
	Version int
}

// SetLayout replaces a workspace's opaque layout blob under optimistic
// concurrency: baseVersion must equal the current version or SetLayout returns
// ErrLayoutConflict (and the current version). On success the version increments,
// the blob is persisted, and a "layout" event is broadcast to attached lenses.
// Returns the resulting version.
func (m *Manager) SetLayout(wsID, blob string, baseVersion int) (int, error) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return 0, fmt.Errorf("unknown workspace %s", wsID)
	}
	if e.ws.LayoutVersion != baseVersion {
		cur := e.ws.LayoutVersion
		m.mu.Unlock()
		return cur, ErrLayoutConflict
	}
	e.ws.LayoutJSON = blob
	e.ws.LayoutVersion++
	newV := e.ws.LayoutVersion
	ctrl := e.ctrl
	saved := *e.ws
	m.mu.Unlock()

	_ = m.store.SaveWorkspace(&saved)
	if ctrl != nil {
		ctrl.Broadcast(session.Event{Kind: "layout", Payload: LayoutUpdate{Blob: blob, Version: newV}})
	}
	return newV, nil
}

// ApplySession forwards a hook's session-lifecycle verdict for a pane to the
// peers bus, which is the only consumer that needs it: presence there must mean
// "a session will read this", and no signal the bus owns can establish that.
// A nil sink (peers disabled) makes this a no-op.
func (m *Manager) ApplySession(paneID, sessionID string, sig model.SessionSignal) {
	// Any positive is proof a Claude has run here, whoever launched it. The bit
	// is sticky for the life of the pane and is what dormancy keys off.
	if sig == model.SessionStarted || sig == model.SessionActive {
		m.markHostedClaude(paneID)
	}
	if m.SessionSink == nil {
		return
	}
	m.SessionSink(paneID, sessionID, sig)
}

// markHostedClaude records that a Claude session has run in a pane, persisting
// and broadcasting only on the transition.
func (m *Manager) markHostedClaude(paneID string) {
	m.mu.Lock()
	e, p := m.findPaneLocked(paneID)
	if p == nil || p.HostedClaude {
		m.mu.Unlock()
		return
	}
	p.HostedClaude = true
	p.Dormant = isDormant(p)
	wsID, saved := e.ws.ID, *p
	m.mu.Unlock()
	_ = m.store.SavePane(&saved)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
}

// ApplyAttention sets a pane's attention state, persists it, and broadcasts the
// change to every lens attached to its workspace.
func (m *Manager) ApplyAttention(paneID string, att model.Attention) {
	m.mu.Lock()
	e, p := m.findPaneLocked(paneID)
	if p == nil {
		m.mu.Unlock()
		return
	}
	p.Attention = att
	ctrl := e.ctrl
	wsID := e.ws.ID
	saved := *p
	m.mu.Unlock()

	if ctrl != nil {
		ctrl.Broadcast(session.Event{Kind: "attention", PaneID: paneID, Attention: att})
	}
	// Fan the same change out globally so sidebar lenses flash without holding a
	// per-workspace attach WebSocket.
	m.events.publish(Event{Kind: "attention", WorkspaceID: wsID, PaneID: paneID, Attention: att})
	_ = m.store.SavePane(&saved)
}

func (m *Manager) findPaneLocked(paneID string) (*entry, *model.Pane) {
	for _, e := range m.byID {
		for _, p := range e.ws.Panes {
			if p.ID == paneID {
				return e, p
			}
		}
	}
	return nil, nil
}

// --- helpers ---

func (m *Manager) newPane(wsID, cwd, startupCmd, createdBy string) *model.Pane {
	return &model.Pane{
		ID: uuid.NewString(), WorkspaceID: wsID, CWD: cwd, StartupCommand: startupCmd,
		CreatedBy: createdBy, CreatedAt: nowMillis(), Status: model.StatusLive,
		Attention: model.AttentionIdle,
		Title:     initialPaneTitle(startupCmd),   // refined live by tmux signals
		Cols:      defaultCols, Rows: defaultRows, // matches the initial ctrl.Resize
	}
}

// paneSize is a pane's remembered tmux size, falling back to the default for a
// pane no lens has ever sized (0 from a pre-migration registry, or a pane created
// and revived before any lens attached).
func paneSize(p *model.Pane) (cols, rows int) {
	if p.Cols > 0 && p.Rows > 0 {
		return p.Cols, p.Rows
	}
	return defaultCols, defaultRows
}

// PaneSize is the payload of a "pane-size" event: a pane's new tmux dimensions,
// broadcast so lenses know the authoritative size (a phone can then surface a
// "take over" control when another lens drove the shared pane wider than it shows).
type PaneSize struct {
	Cols int
	Rows int
}

// ResizePane sets a pane's tmux size, records it on the pane, and — only when the
// size actually changed — persists it and broadcasts the new size to every attached
// lens. This is the single resize entry point for the API so size changes are always
// announced. The reported `changed` is what tells a caller the pane has just
// reflowed: tmux only winches the inner program on a real size change, so an
// unchanged resize repaints nothing and needs no follow-up.
func (m *Manager) ResizePane(paneID string, cols, rows int) (changed bool, err error) {
	if err := validPaneSize(paneID, cols, rows); err != nil {
		return false, err
	}
	m.mu.Lock()
	e, p := m.findPaneLocked(paneID)
	if p == nil {
		m.mu.Unlock()
		return false, fmt.Errorf("unknown pane %s", paneID)
	}
	ctrl := e.ctrl
	changed = p.Cols != cols || p.Rows != rows
	m.mu.Unlock()

	if ctrl == nil {
		return false, fmt.Errorf("workspace for pane %s not live", paneID)
	}
	// Nothing is recorded until tmux has accepted the size. Recording first looks
	// harmless because the error still propagates, but it makes the failure
	// permanent: the pane would claim a size it never got, so the client's retry
	// at those same dimensions computes changed == false and then skips the
	// persist, the broadcast and the repaint forever.
	if err := ctrl.Resize(paneID, cols, rows); err != nil {
		return false, err
	}
	m.mu.Lock()
	p.Cols, p.Rows = cols, rows
	m.mu.Unlock()

	if changed {
		m.commitPaneSize(ctrl, paneID, cols, rows)
	}
	return changed, nil
}

// validPaneSize rejects what tmux will not honour. `resize-window` errors above
// 10000 (measured on tmux 3.6b, which also refuses 65536 or more outright), so
// bounding here turns a per-resize tmux error into one clear rejection at the API
// edge — and keeps an absurd value away from a field that is now persisted.
func validPaneSize(paneID string, cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > maxCols || rows > maxRows {
		return fmt.Errorf("pane %s: size %dx%d out of range", paneID, cols, rows)
	}
	return nil
}

// commitPaneSize records an applied size and tells the other lenses about it.
//
// The write is size-only (not SavePane) for two reasons: a full-row upsert would
// carry whatever else the snapshot held, and it would reinsert a row that a
// concurrent close had just deleted, leaving a phantom pane behind.
func (m *Manager) commitPaneSize(ctrl *session.Controller, paneID string, cols, rows int) {
	// Persisted so a restart or revive rebuilds the pane at the size it was last
	// drawn at. Without it the next revive re-creates it at 80x24 and the inner
	// program's output stays wrapped at a width the pane no longer has.
	if err := m.store.UpdatePaneSize(paneID, cols, rows); err != nil {
		log.Printf("resize pane %s: persist %dx%d: %v", paneID, cols, rows, err)
	}
	ctrl.Broadcast(session.Event{Kind: "pane-size", PaneID: paneID, Payload: PaneSize{Cols: cols, Rows: rows}})
}

// BroadcastClipboard pushes tmux-copied text to every lens attached to the
// workspace owning the TMUX pane (the tmux config's copy-pipe bindings POST
// it here with #{pane_id}, e.g. "%5" — tmux knows nothing of ccmux pane
// uuids). Scoped to the one owning workspace on purpose: a copy must never
// land on lenses watching other workspaces or other users' sessions. tmux
// pane ids are unique per tmux server, so at most one controller matches.
func (m *Manager) BroadcastClipboard(tmuxPane string, text []byte) error {
	m.mu.RLock()
	ctrls := make([]*session.Controller, 0, len(m.byID))
	for _, e := range m.byID {
		if e.ctrl != nil {
			ctrls = append(ctrls, e.ctrl)
		}
	}
	m.mu.RUnlock()
	for _, ctrl := range ctrls {
		if paneID := ctrl.PaneIDForTmux(tmuxPane); paneID != "" {
			ctrl.Broadcast(session.Event{Kind: "clipboard", PaneID: paneID, Data: text})
			return nil
		}
	}
	return fmt.Errorf("unknown tmux pane %s", tmuxPane)
}

func (m *Manager) paneEnv(paneID string) map[string]string {
	if err := shellint.WriteZdotdir(shellint.ZdotdirPath(paneID)); err != nil {
		// Costs this pane its command capture, so it is worth a line; the error
		// used to be dropped entirely.
		log.Printf("pane %s: shell integration not installed (%v) — command capture is off for it", paneID, err)
	}
	env := shellint.EnvForPane(paneID)
	if m.LocalURL != "" {
		env["CCMUX_DAEMON_URL"] = m.LocalURL
	}
	if m.HooksSocket != "" {
		env["CCMUX_HOOKS_SOCK"] = m.HooksSocket
	}
	if m.ExtraPaneEnv != nil {
		for k, v := range m.ExtraPaneEnv(paneID) {
			env[k] = v
		}
	}
	return env
}

// WorkspaceForPane returns the workspace id owning a pane, or "" if unknown.
func (m *Manager) WorkspaceForPane(paneID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, _ := m.findPaneLocked(paneID); e != nil {
		return e.ws.ID
	}
	return ""
}

// GroupForPane returns the sidebar group of the workspace owning a pane ("" for
// an ungrouped workspace) and whether the pane is known at all. The peers bus
// resolves a pane peer's group through this at operation time, so a window
// rename or move re-groups live sessions immediately.
func (m *Manager) GroupForPane(paneID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, _ := m.findPaneLocked(paneID); e != nil {
		return e.ws.Group, true
	}
	return "", false
}

// LiveWorkspaceForRepo finds a live workspace in the given sidebar group whose
// repo directory's basename matches name — the peers-bus native spawn target.
func (m *Manager) LiveWorkspaceForRepo(group, name string) (wsID, repoPath string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.byID {
		if e.ctrl == nil || e.ws.Group != group {
			continue
		}
		if e.ws.RepoPath != "" && baseName(e.ws.RepoPath) == name {
			return e.ws.ID, e.ws.RepoPath, true
		}
	}
	return "", "", false
}

// baseName is filepath.Base without treating "" as ".".
func baseName(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// deliverStartup sends a pane's startup command as keystrokes (the sanctioned
// startup mechanism — not mid-session injection). Sizing is already set, so no
// wrap dance is needed. Because a startup command may launch a dev-channels
// claude (possibly via a shell alias), we arm the hands-free auto-confirm
// watcher for the pane's first ~120s.
func (m *Manager) deliverStartup(ctrl *session.Controller, paneID, cmd string) {
	if cmd == "" {
		return
	}
	_ = ctrl.SendInput(paneID, []byte(cmd+"\r"))
	go autoconfirm.Watch(m.ctx, ctrl, paneID)
}

// watch consumes a controller's notices and reflects them into the registry:
// a closed window drops its pane; session exit marks the workspace cold.
func (m *Manager) watch(wsID string, ctrl *session.Controller) {
	for n := range ctrl.Notices() {
		switch n.Kind {
		case "window-close":
			m.dropPane(wsID, n.PaneID)
		case "pane-title", "pane-command":
			m.applyPaneTitleSignal(wsID, n.PaneID, n.Kind, n.Value)
		case "exit":
			m.markCold(wsID)
			return
		}
	}
}

// dropPane removes a pane from the workspace and the store — except when it is the
// last one. A workspace with no panes cannot be revived (ReviveWorkspace reads pane 0
// for the session's cwd), so the final row stays on as the recipe and the workspace
// simply goes cold. KillPane archives explicitly for this case; the floor here catches
// every other route to the same state, including a pane count that changed under a
// racing caller.
func (m *Manager) dropPane(wsID, paneID string) {
	if paneID == "" {
		return
	}
	m.mu.Lock()
	dropped := false
	if e := m.byID[wsID]; e != nil && len(e.ws.Panes) > 1 {
		kept := e.ws.Panes[:0]
		for _, p := range e.ws.Panes {
			if p.ID != paneID {
				kept = append(kept, p)
			}
		}
		dropped = len(kept) != len(e.ws.Panes)
		e.ws.Panes = kept
	}
	m.mu.Unlock()
	if dropped {
		_ = m.store.DeletePane(paneID)
	}
}

func (m *Manager) markCold(wsID string) {
	m.mu.Lock()
	e := m.byID[wsID]
	if e != nil {
		e.ws.Status = model.StatusCold
		e.ctrl = nil
	}
	m.mu.Unlock()
	if e == nil {
		return // already removed (e.g. a concurrent delete) — nothing to cool down
	}
	_ = m.store.SetWorkspaceStatus(wsID, model.StatusCold)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
}

func (m *Manager) entry(wsID string) *entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[wsID]
}

func nowMillis() int64 { return time.Now().UnixMilli() }

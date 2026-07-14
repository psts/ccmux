// Package manager orchestrates the daemon: it owns the tmux Server, the durable
// Store, and one live session.Controller per hosted workspace, and exposes the
// operations the API serves (create/spawn/list/attach/kill).
package manager

import (
	"context"
	"errors"
	"fmt"
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

	mu   sync.RWMutex
	byID map[string]*entry
}

// New builds a Manager. ctx bounds the lifetime of spawned control connections.
func New(ctx context.Context, server *tmux.Server, st store.Store) *Manager {
	return &Manager{server: server, store: st, ctx: ctx, events: newFirehose(), byID: map[string]*entry{}}
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
		ws.Status = model.StatusCold
		m.mu.Lock()
		m.byID[ws.ID] = &entry{ws: ws}
		m.mu.Unlock()
		return
	}
	ws.Status = model.StatusLive
	m.mu.Lock()
	m.byID[ws.ID] = &entry{ws: ws, ctrl: ctrl}
	m.mu.Unlock()
	go m.watch(ws.ID, ctrl)
}

// CreateWorkspace creates a new hosted workspace with an initial pane.
func (m *Manager) CreateWorkspace(name, repoPath, cwd, startupCmd, createdBy string) (*model.Workspace, error) {
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
	m.deliverStartup(ctrl, pane0)

	ws := &model.Workspace{
		ID: wsID, Name: name, RepoPath: repoPath, CreatedBy: createdBy,
		CreatedAt: nowMillis(), TmuxSession: sessionName, Status: model.StatusLive,
		Panes: []*model.Pane{pane0},
	}
	if err := m.store.SaveWorkspace(ws); err != nil {
		return nil, err
	}
	_ = m.store.SavePane(pane0)

	m.mu.Lock()
	m.byID[wsID] = &entry{ws: ws, ctrl: ctrl}
	m.mu.Unlock()
	go m.watch(wsID, ctrl)
	return ws, nil
}

// SpawnPane adds a pane (tmux window) to a live workspace.
func (m *Manager) SpawnPane(wsID, cwd, startupCmd, createdBy string) (*model.Pane, error) {
	e := m.entry(wsID)
	if e == nil || e.ctrl == nil {
		return nil, fmt.Errorf("workspace %s not live", wsID)
	}
	if cwd == "" {
		cwd = e.ws.RepoPath
	}
	p := m.newPane(wsID, cwd, startupCmd, createdBy)
	if err := e.ctrl.SpawnWindow(p.ID, cwd, m.paneEnv(p.ID)); err != nil {
		return nil, err
	}
	_ = e.ctrl.Resize(p.ID, defaultCols, defaultRows)
	m.deliverStartup(e.ctrl, p)

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
	if err := m.server.NewSession(ws.TmuxSession, pane0.CWD, defaultCols, defaultRows, m.paneEnv(pane0.ID)); err != nil {
		return nil, err
	}
	ctrl, err := session.Open(m.ctx, m.server, ws.TmuxSession, wsID)
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
	m.deliverStartup(ctrl, pane0)

	for _, p := range ws.Panes[1:] {
		if err := ctrl.SpawnWindow(p.ID, p.CWD, m.paneEnv(p.ID)); err != nil {
			ctrl.Close()
			return nil, err
		}
		_ = ctrl.Resize(p.ID, defaultCols, defaultRows)
		m.deliverStartup(ctrl, p)
		p.Status = model.StatusLive
	}
	pane0.Status = model.StatusLive
	ws.Status = model.StatusLive

	m.mu.Lock()
	e.ctrl = ctrl
	m.mu.Unlock()
	_ = m.store.SaveWorkspace(ws)
	for _, p := range ws.Panes {
		_ = m.store.SavePane(p)
	}
	go m.watch(wsID, ctrl)
	return ws, nil
}

// KillWorkspace tears down a workspace's tmux session and registry record.
func (m *Manager) KillWorkspace(wsID string) error {
	e := m.entry(wsID)
	if e == nil {
		return fmt.Errorf("unknown workspace %s", wsID)
	}
	if e.ctrl != nil {
		e.ctrl.Close()
	}
	_ = m.server.KillSession(e.ws.TmuxSession)
	m.mu.Lock()
	delete(m.byID, wsID)
	m.mu.Unlock()
	return m.store.DeleteWorkspace(wsID)
}

// Controller returns the live control connection for a workspace, if any.
func (m *Manager) Controller(wsID string) *session.Controller {
	if e := m.entry(wsID); e != nil {
		return e.ctrl
	}
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
	}
}

func (m *Manager) paneEnv(paneID string) map[string]string {
	_ = shellint.WriteZdotdir(shellint.ZdotdirPath(paneID))
	env := shellint.EnvForPane(paneID)
	if m.LocalURL != "" {
		env["CCMUX_DAEMON_URL"] = m.LocalURL
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

// deliverStartup sends a pane's startup command as keystrokes (the sanctioned
// startup mechanism — not mid-session injection). Sizing is already set, so no
// wrap dance is needed. Because a startup command may launch a dev-channels
// claude (possibly via a shell alias), we arm the hands-free auto-confirm
// watcher for the pane's first ~120s.
func (m *Manager) deliverStartup(ctrl *session.Controller, p *model.Pane) {
	if p.StartupCommand == "" {
		return
	}
	_ = ctrl.SendInput(p.ID, []byte(p.StartupCommand+"\r"))
	go autoconfirm.Watch(m.ctx, ctrl, p.ID)
}

// watch consumes a controller's notices and reflects them into the registry:
// a closed window drops its pane; session exit marks the workspace cold.
func (m *Manager) watch(wsID string, ctrl *session.Controller) {
	for n := range ctrl.Notices() {
		switch n.Kind {
		case "window-close":
			m.dropPane(wsID, n.PaneID)
		case "exit":
			m.markCold(wsID)
			return
		}
	}
}

func (m *Manager) dropPane(wsID, paneID string) {
	if paneID == "" {
		return
	}
	m.mu.Lock()
	if e := m.byID[wsID]; e != nil {
		kept := e.ws.Panes[:0]
		for _, p := range e.ws.Panes {
			if p.ID != paneID {
				kept = append(kept, p)
			}
		}
		e.ws.Panes = kept
	}
	m.mu.Unlock()
	_ = m.store.DeletePane(paneID)
}

func (m *Manager) markCold(wsID string) {
	m.mu.Lock()
	if e := m.byID[wsID]; e != nil {
		e.ws.Status = model.StatusCold
		e.ctrl = nil
	}
	m.mu.Unlock()
	_ = m.store.SetWorkspaceStatus(wsID, model.StatusCold)
}

func (m *Manager) entry(wsID string) *entry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[wsID]
}

func nowMillis() int64 { return time.Now().UnixMilli() }

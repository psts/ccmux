// Package model holds ccmuxd's durable domain types: workspaces and panes. A
// workspace maps to one tmux session; a pane maps to one tmux window (holding a
// single tmux pane — ccmux never splits inside tmux). The SplitTree layout is a
// lens-side concept the daemon stores as an opaque versioned JSON blob.
package model

// Status is a workspace/pane lifecycle state.
type Status string

const (
	// StatusLive means the backing tmux session/window currently exists.
	StatusLive Status = "live"
	// StatusCold means only the registry record survives (tmux server died or
	// host rebooted); revivable by replaying startup commands.
	StatusCold Status = "cold"
)

// Attention is a pane's activity state, driven primarily by Claude Code hooks
// and secondarily by %output flow. It powers session-list coloring in lenses.
type Attention string

const (
	AttentionRunning    Attention = "running"     // producing output
	AttentionIdle       Attention = "idle"        // quiet, nothing needed
	AttentionNeedsInput Attention = "needs_input" // blocked on the user
	AttentionDone       Attention = "done"        // finished a task
)

// Workspace is a project working context: one tmux session, N panes.
type Workspace struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	RepoPath      string   `json:"repoPath"`
	CreatedBy     string   `json:"createdBy"`
	CreatedAt     int64    `json:"createdAt"` // unix millis
	TmuxSession   string   `json:"tmuxSession"`
	Status        Status   `json:"status"`
	LayoutJSON    string   `json:"layoutJson,omitempty"`
	LayoutVersion int      `json:"layoutVersion"`
	Panes         []*Pane  `json:"panes"`
}

// PushSubscription is a stored notification target, keyed by tailnet login and
// kept transport-generic ({transport, address, prefs}) so web push today and
// APNs/Telegram later share one table. For transport "webpush", Address is the
// browser's PushSubscription.toJSON() ({endpoint, keys:{p256dh, auth}}).
type PushSubscription struct {
	ID        string `json:"id"`        // stable: sha256(endpoint), so re-subscribe replaces
	Login     string `json:"login"`     // tailnet login (email) or self-declared user
	Transport string `json:"transport"` // "webpush" | "apns" | "telegram"
	Address   string `json:"address"`   // transport-specific JSON (see above)
	Prefs     string `json:"prefs,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

// Pane is one terminal (a tmux window with a single pane).
type Pane struct {
	ID             string    `json:"id"` // stable uuid → tmux @ccmux_pane_id
	WorkspaceID    string    `json:"workspaceId"`
	TmuxWindow     string    `json:"tmuxWindow,omitempty"` // @N, runtime-only
	TmuxPane       string    `json:"tmuxPane,omitempty"`   // %N, runtime-only
	Title          string    `json:"title"`
	CWD            string    `json:"cwd"`
	StartupCommand string    `json:"startupCommand,omitempty"`
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      int64     `json:"createdAt"`
	Status         Status    `json:"status"`
	Attention      Attention `json:"attention"`
	// Cols/Rows are the pane's current tmux size (runtime-only; the daemon owns it
	// via resize-window). Lenses use it to detect when another lens drove the shared
	// pane to a different width — e.g. a phone showing a "take over" control.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

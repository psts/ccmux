// Package model holds ccmuxd's durable domain types: workspaces and panes. A
// workspace maps to one tmux session; a pane maps to one tmux window (holding a
// single tmux pane — ccmux never splits inside tmux). The SplitTree layout is a
// lens-side concept the daemon stores as an opaque versioned JSON blob.
package model

import (
	"encoding/json"

	"ccmux.dev/ccmuxd/internal/gitstatus"
)

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
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	RepoPath      string  `json:"repoPath"`
	CreatedBy     string  `json:"createdBy"`
	CreatedAt     int64   `json:"createdAt"` // unix millis
	TmuxSession   string  `json:"tmuxSession"`
	Status        Status  `json:"status"`
	LayoutJSON    string  `json:"layoutJson,omitempty"`
	LayoutVersion int     `json:"layoutVersion"`
	Panes         []*Pane `json:"panes"`
	// Git is the repo's dashboard status (branch, ahead/behind, changed files),
	// computed daemon-side by the manager's collector — the repo lives on the
	// daemon's host, so lenses can't read it locally. Runtime-only (not stored);
	// nil until the first collection.
	Git *gitstatus.Status `json:"git,omitempty"`
	// Group is the workspace's sidebar group, shared across lenses. The Mac app
	// is the source of truth: it pushes the owning window's name here so the
	// web/phone lens renders the same grouping. Empty = ungrouped.
	Group string `json:"group,omitempty"`
	// Hostnames are the workspace's dev-hostname mappings (see model.Hostname).
	Hostnames []Hostname `json:"hostnames,omitempty"`
	// DevCommand starts this workspace's dev server (the ▶ on a hostname row).
	// Empty = resolve by detection (portdetect.DetectCommand) at start time;
	// set = the user's explicit override from the Hostnames sheet.
	DevCommand string `json:"devCommand,omitempty"`
}

// Hostname is one dev-hostname mapping: https://<Name>.<suffix> over the
// tailnet reverse-proxies to 127.0.0.1:Port on the daemon host. Name is the
// bare DNS label ("chartlabs-app"); the daemon's serving mode picks the suffix
// (the configured dev domain, else per-hostname tsnet nodes on ts.net). Name
// and Port are persisted; URL and Listening are runtime-only, stamped by the
// devhost server (the resolved https URL, and whether the port currently
// accepts connections).
type Hostname struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	URL       string `json:"url,omitempty"`
	Listening bool   `json:"listening,omitempty"`
}

// MarshalHostnames serializes mappings for the registry, keeping only the
// persisted fields ("" for none — the column default). UnmarshalHostnames is
// its inverse; malformed blobs load as no mappings rather than failing the
// whole registry.
func MarshalHostnames(hs []Hostname) string {
	if len(hs) == 0 {
		return ""
	}
	type persisted struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}
	out := make([]persisted, len(hs))
	for i, h := range hs {
		out[i] = persisted{Name: h.Name, Port: h.Port}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// UnmarshalHostnames parses a registry blob written by MarshalHostnames.
func UnmarshalHostnames(raw string) []Hostname {
	if raw == "" {
		return nil
	}
	var hs []Hostname
	if json.Unmarshal([]byte(raw), &hs) != nil {
		return nil
	}
	return hs
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

// PeerEvent is one entry in the peers bus event log: a chat message between
// Claude Code peers, or a permission verdict routed to a worker whose tool
// dialog a delegator answered remotely. Events are addressed to exactly one
// peer and delivered in seq order; a per-peer cursor (highest cumulatively
// acked seq) makes replay-on-subscribe lossless and duplicate-free.
type PeerEvent struct {
	Seq         int64  `json:"seq"`
	Kind        string `json:"kind"` // "message" | "permission_verdict"
	FromID      string `json:"from_id"`
	FromName    string `json:"from_name"`
	FromSummary string `json:"from_summary"`
	FromCWD     string `json:"from_cwd"`
	ToID        string `json:"to_id"`
	ToName      string `json:"to_name"`
	// Group is the SENDER's group snapshotted at send time, so group history
	// (the read-only viewer) survives peers that have since left.
	Group string `json:"group"`
	// Text is the raw message text. For verdicts it still holds the raw reply
	// ("yes abcde") so viewers show what was actually said; the structured
	// fields below are what the worker's client consumes.
	Text      string `json:"text"`
	RequestID string `json:"request_id,omitempty"` // verdicts only
	Behavior  string `json:"behavior,omitempty"`   // verdicts only: "allow" | "deny"
	SentAt    int64  `json:"sent_at"`              // unix millis
}

// PeerEventMessage and PeerEventVerdict are the two PeerEvent kinds.
const (
	PeerEventMessage = "message"
	PeerEventVerdict = "permission_verdict"
)

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
	// DevServer marks the workspace's dev-server pane (spawned by ▶, killed by
	// ■). Its presence is the "running" signal lenses render.
	DevServer bool `json:"devServer,omitempty"`
	// Cols/Rows are the pane's current tmux size (runtime-only; the daemon owns it
	// via resize-window). Lenses use it to detect when another lens drove the shared
	// pane to a different width — e.g. a phone showing a "take over" control.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`
}

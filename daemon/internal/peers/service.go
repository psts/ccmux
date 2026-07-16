// Package peers is ccmuxd's built-in Claude-Code-to-Claude-Code messaging bus,
// the Go port of the external claude-peers broker. Peers are grouped by ccmux
// window (the owning workspace's sidebar Group, resolved live through the
// manager); sessions without a pane fall back to the old parent-directory
// grouping. Delivery is an append-only event log with one server-side cursor
// per peer: subscribing replays everything past the cursor, cumulative acks
// advance it, so reconnects are lossless and duplicate-free by construction.
package peers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// Hook is the slice of the manager the bus needs: live group resolution and
// the native teammate spawn.
type Hook interface {
	GroupForPane(paneID string) (string, bool)
	LiveWorkspaceForRepo(group, name string) (wsID, repoPath string, ok bool)
	SpawnEphemeralPane(wsID, cwd, oneShotCmd, createdBy string) error
}

// Store is the slice of the registry store the bus needs (see store.Store).
type Store interface {
	AppendPeerEvent(*model.PeerEvent) (int64, error)
	PeerEventsAfter(toID string, afterSeq int64) ([]*model.PeerEvent, error)
	PeerCursor(peerID string) (int64, error)
	AdvancePeerCursor(peerID string, seq int64) error
	RecentPeerSenders(toID string, sinceMillis int64) ([]string, error)
	PeerGroupMessages(group string, sinceMillis int64, limit int) ([]*model.PeerEvent, error)
	PrunePeerEvents(beforeMillis int64) (int64, error)
}

// Peer is one registered session. Live connection state is tracked separately
// in Service.conns so a WS drop doesn't unregister the peer.
type Peer struct {
	ID     string
	Name   string
	PaneID string // "" for sessions outside ccmux
	// LocalPaneID is the Mac app's driver-mode pane UUID (derived from
	// CCMUX_CMD_FILE) for sessions in local panes the daemon doesn't host.
	// The app pushes a live localPaneID→window-name map, giving these panes
	// window grouping too.
	LocalPaneID  string
	PID          int
	CWD          string
	GitRoot      string
	Summary      string
	RegisteredAt int64
	// GroupOverride pins a pane-less peer into a window group: set when a
	// deep-link-spawned teammate (a Mac-local ephemeral pane, invisible to the
	// daemon) registers and matches a pending spawn — without it the teammate
	// would land in the dirname fallback group and the same-group guard would
	// cut it off from its own requester.
	GroupOverride string
}

type permRequest struct {
	workerID string
	resolved bool
	at       int64
}

type pendingSpawn struct {
	name     string
	group    string
	repo     string
	requests []queuedRequest
	timer    *time.Timer
}

type queuedRequest struct {
	fromID string
	text   string
}

const (
	recentSenderWindow = 10 * time.Minute
	permRequestTTL     = 12 * time.Hour // dialogs can sit open for a long time
	eventRetention     = 30 * 24 * time.Hour
	defaultSpawnWait   = 60 * time.Second
)

// Service is the bus. Safe for concurrent use; one mutex guards all state and
// store writes, which keeps append+push atomic relative to conn attachment.
type Service struct {
	st     Store
	mgr    Hook
	secret []byte

	// SpawnTimeout is how long a spawned teammate has to register before its
	// requester gets an "unreachable" notice. Exported for tests.
	SpawnTimeout time.Duration
	// OpenCmd launches the ccmux://spawn deep link for the non-native spawn
	// fallback ("open" on macOS). Tests override; "" disables the fallback.
	OpenCmd string
	// Now is the clock (exported for tests).
	Now func() time.Time

	mu        sync.Mutex
	peers     map[string]*Peer
	conns     map[string]*peerConn
	listeners map[*listenConn]struct{}
	perms     map[string]*permRequest
	spawns    map[string]*pendingSpawn
	// localGroups maps a Mac-local pane's UUID (lowercased) to its owning
	// window's name. The Mac app is the source of truth and pushes the full map
	// on every window/ownership change, so resolution stays live for driver-mode
	// panes exactly like workspace groups do for hosted ones. Not persisted —
	// the app re-pushes shortly after either side restarts.
	localGroups map[string]string
}

// NewService builds the bus around the persisted event log, the manager hook,
// and the token secret.
func NewService(st Store, mgr Hook, secret []byte) *Service {
	return &Service{
		st: st, mgr: mgr, secret: secret,
		SpawnTimeout: defaultSpawnWait,
		OpenCmd:      "open",
		Now:          time.Now,
		peers:        map[string]*Peer{},
		conns:        map[string]*peerConn{},
		listeners:    map[*listenConn]struct{}{},
		perms:        map[string]*permRequest{},
		spawns:       map[string]*pendingSpawn{},
		localGroups:  map[string]string{},
	}
}

// Start launches the background pruner for the lifetime of ctx.
func (s *Service) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = s.st.PrunePeerEvents(s.Now().Add(-eventRetention).UnixMilli())
				s.mu.Lock()
				s.prunePermsLocked()
				s.mu.Unlock()
			}
		}
	}()
}

// PaneEnv contributes the per-pane bearer token to hosted panes' env (wired as
// manager.ExtraPaneEnv).
func (s *Service) PaneEnv(paneID string) map[string]string {
	return map[string]string{"CCMUX_PANE_TOKEN": TokenForPane(s.secret, paneID)}
}

// PanelessToken exposes the shared no-pane token for the daemon-info file.
func (s *Service) PanelessToken() string { return PanelessToken(s.secret) }

// groupOfLocked resolves a peer's group at operation time: the owning
// workspace's window group when the pane is known and grouped, then the Mac
// app's local-pane map (driver-mode panes), then a spawn override, otherwise
// the legacy parent-directory fallback (dirname of git root, or of cwd).
func (s *Service) groupOfLocked(p *Peer) string {
	if p.PaneID != "" {
		if g, ok := s.mgr.GroupForPane(p.PaneID); ok && g != "" {
			return g
		}
	}
	if p.LocalPaneID != "" {
		if g := s.localGroups[strings.ToLower(p.LocalPaneID)]; g != "" {
			return g
		}
	}
	if p.GroupOverride != "" {
		return p.GroupOverride
	}
	return fallbackGroup(p.GitRoot, p.CWD)
}

// SetLocalPaneGroups replaces the local-pane→window map (the Mac app always
// pushes its complete current view).
func (s *Service) SetLocalPaneGroups(groups map[string]string) {
	normalized := make(map[string]string, len(groups))
	for id, g := range groups {
		normalized[strings.ToLower(id)] = g
	}
	s.mu.Lock()
	s.localGroups = normalized
	s.mu.Unlock()
}

func fallbackGroup(gitRoot, cwd string) string {
	base := gitRoot
	if base == "" {
		base = cwd
	}
	if base == "" {
		return ""
	}
	return filepath.Dir(base)
}

func (s *Service) prunePermsLocked() {
	cutoff := s.Now().Add(-permRequestTTL).UnixMilli()
	for id, pr := range s.perms {
		if pr.at < cutoff {
			delete(s.perms, id)
		}
	}
}

// derivedID maps a pane id to a stable 8-char peer id, so an MCP-server
// restart inside the same pane keeps its identity by construction.
func derivedID(paneID string) string {
	sum := sha256.Sum256([]byte("peer:" + paneID))
	return hex.EncodeToString(sum[:])[:8]
}

func randomID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// isoMillis renders unix millis the way JS Date.toISOString does — the wire
// format every existing consumer (channel tags, Mac overlay, web UI) expects.
func isoMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

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
	"ccmux.dev/ccmuxd/internal/store"
)

// Hook is the slice of the manager the bus needs: live group resolution and
// the native teammate spawn.
type Hook interface {
	GroupForPane(paneID string) (string, bool)
	// PaneAtShell reports whether a pane's foreground is a bare shell RIGHT NOW.
	// Asked at decision time rather than remembered from a past signal: hooks
	// arrive out of band and can land after the observation that contradicts
	// them, and a fact we can look up has no reason to be cached.
	PaneAtShell(paneID string) bool
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
	TouchPeerMailbox(peerID, paneID string, now int64) error
	SavePaneSessions(paneID string, liveIDs []string, lastActivity int64) error
	LoadPaneSessions() (map[string]store.PaneSessionState, error)
	DeletePaneSessions(paneID string) error
	PeerMailboxes() ([]store.PeerMailbox, error)
	DeletePeerState(peerID string) error

	// Relay state that outlives the daemon: a permission dialog can sit open for
	// hours and a reply grant lasts two, so holding either only in memory meant a
	// restart silently broke a conversation already under way.
	SavePermRequest(requestID, workerID string, resolved bool, createdAt int64) error
	LoadPermRequests() (map[string]store.PermRequest, error)
	DeletePermRequest(requestID string) error
	PrunePermRequests(beforeMillis int64) error
	SaveReplyGrant(replier, sender string, expiresAt int64) error
	LoadReplyGrants() ([]store.ReplyGrant, error)
	DeleteReplyGrant(replier, sender string) error
	PruneReplyGrants(nowMillis int64) error

	// Delegation tasks: durable so a delegation outlives the sessions on both
	// ends and their restarts (see store/peertasks.go).
	SavePeerTask(store.PeerTask) error
	PeerTask(taskID string) (*store.PeerTask, error)
	OpenPeerTasksFor(peerID string, limit int) ([]store.PeerTask, error)
	DeletePeerTask(taskID string) error
	PrunePeerTasks(beforeMillis int64) error
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
	// LastSeenAt is the last moment this peer's session proved it was there:
	// registering, attaching its socket, acking, or polling. Presence is judged
	// from it, so a session that stops proving anything stops being listed.
	LastSeenAt int64
	// PollOnly marks a session that opted out of live push and collects messages
	// by polling. Such a peer never holds a socket, so "not connected" is its
	// healthy resting state and must not be reported as a broken one.
	PollOnly bool
	// AwayAt is when the session left cleanly (stdin EOF → unregister), or 0
	// while it is here. A pane peer that goes away keeps its record and its
	// queue — its id is derived from the pane, so the next session in that pane
	// re-derives it and replays — but it is NOT present, and presence is the
	// only thing a listing may report.
	AwayAt int64
	// GroupOverride pins a pane-less peer into a window group: set when a
	// deep-link-spawned teammate (a Mac-local ephemeral pane, invisible to the
	// daemon) registers and matches a pending spawn — without it the teammate
	// would land in the dirname fallback group and the same-group guard would
	// cut it off from its own requester.
	GroupOverride string
	// Host is the owning host's MagicDNS label (federation, hub mode): stamped at
	// registration from the hub's aggregated view so list_peers can distinguish
	// same-named peers on different hosts. "" for a pane on the hub itself or in
	// single-host mode.
	Host string
	// ShimVersion is what the connected shim reported at registration ("" for a
	// pre-0.3.0 shim). Diagnostic: with federation putting shims and daemon on
	// different hosts, a wire mismatch should be readable off a listing rather
	// than inferred from absent fields.
	ShimVersion string
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
	// Closed delegations are kept two weeks for the audit trail; open ones are
	// never pruned — an unanswered delegation staying visible is the point.
	taskRetention = 14 * 24 * time.Hour
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
	// missingSince records when a pane first failed to resolve, so absence has
	// to persist before the reaper erases anything that hangs off it.
	missingSince map[string]int64
	// replyGrants licenses cross-group replies, keyed "replier\x00original
	// sender" with an expiry (see reach.go).
	replyGrants map[string]int64
	// sessions is the per-pane Claude session truth fed by hooks (see
	// sessions.go) — the only signal that distinguishes a session that will
	// read a message from a process that merely has the MCP server loaded.
	sessions map[string]*paneSessions
	// localGroups maps a Mac-local pane's UUID (lowercased) to its owning
	// window's name. The Mac app is the source of truth and pushes the full map
	// on every window/ownership change, so resolution stays live for driver-mode
	// panes exactly like workspace groups do for hosted ones. Not persisted —
	// the app re-pushes shortly after either side restarts.
	localGroups map[string]string
	// globalGroups and hostForPane are the hub-mode federation resolvers, backed
	// by the aggregator's cached maps (pure reads — never I/O under s.mu).
	// globalGroups resolves a pane's window group across ALL member hosts,
	// consulted after the local manager misses (so a peer on a remote host lands
	// in its window group); hostForPane returns the pane's owning-host label,
	// stamped onto Peer.Host. Both nil off the hub — single-host is unaffected.
	globalGroups func(paneID string) (string, bool)
	hostForPane  func(paneID string) (string, bool)
	// hostForAddr names the member host a connection came FROM. A pane-less
	// peer on a member host has no pane to look hostForPane up by, and its PID
	// belongs to another machine's process table — so without this label the
	// hub's reaper would judge it with kill(0) against an unrelated local pid.
	// Resolved from the hub's own discovery, never from anything the caller says.
	hostForAddr func(ip string) (string, bool)
}

// EnableFederation wires the hub-mode resolvers (see the struct fields). Call
// once at startup, after the aggregator exists and before panes register.
func (s *Service) EnableFederation(groups, host func(string) (string, bool), hostForAddr func(string) (string, bool)) {
	s.mu.Lock()
	s.globalGroups = groups
	s.hostForPane = host
	s.hostForAddr = hostForAddr
	s.mu.Unlock()
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
		missingSince: map[string]int64{},
		replyGrants:  map[string]int64{},
		sessions:     map[string]*paneSessions{},
		localGroups:  map[string]string{},
	}
}

// Start restores persisted session truth, then launches the background pruner
// and the reaper for the lifetime of ctx.
func (s *Service) Start(ctx context.Context) {
	s.loadSessions()
	s.loadRelayState()
	go s.startReaper(ctx)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = s.st.PrunePeerEvents(s.Now().Add(-eventRetention).UnixMilli())
				_ = s.st.PrunePeerTasks(s.Now().Add(-taskRetention).UnixMilli())
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

// MintPaneToken issues a pane's bearer token over THIS daemon's secret — the
// hub-authority path (POST /v1/peers/pane-token) a member host calls so its panes
// connect to the hub's bus without any secret being distributed. See
// daemon/docs/multihost-plan.md ("Hosts hold no secret").
func (s *Service) MintPaneToken(paneID string) string { return TokenForPane(s.secret, paneID) }

// groupOfLocked resolves a peer's group at operation time: the owning
// workspace's window group when the pane is known and grouped, then the Mac
// app's local-pane map (driver-mode panes), then a spawn override, otherwise
// the legacy parent-directory fallback (dirname of git root, or of cwd).
func (s *Service) groupOfLocked(p *Peer) string {
	if p.PaneID != "" {
		if g, ok := s.mgr.GroupForPane(p.PaneID); ok && g != "" {
			return g
		}
		// Federation: the pane may live on another host, unknown to the local
		// manager — resolve its group across the whole federation (cached read).
		if s.globalGroups != nil {
			if g, ok := s.globalGroups(p.PaneID); ok && g != "" {
				return g
			}
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

// fallbackGroup names the project a session outside ccmux belongs to: the
// folder that HOLDS the repo, by name only. A repo at .../Coding/ChartLabs/backend
// gives "ChartLabs" — the same string the ccmux window group uses — so a Claude
// started in a plain terminal lands in the right project and can talk to its
// teammates without naming a group at all. The full path was correct and useless:
// it could never match a window group, so every such session was marooned.
func fallbackGroup(gitRoot, cwd string) string {
	base := gitRoot
	if base == "" {
		base = cwd
	}
	if base == "" {
		return ""
	}
	return filepath.Base(filepath.Dir(base))
}

// sameGroup compares two group names. Case is ignored so a window someone typed
// as "chartlabs" and a folder named "ChartLabs" are one project rather than two
// that cannot see each other. Names are stored and displayed exactly as written.
func sameGroup(a, b string) bool { return strings.EqualFold(a, b) }

func (s *Service) prunePermsLocked() {
	cutoff := s.Now().Add(-permRequestTTL).UnixMilli()
	for id, pr := range s.perms {
		if pr.at < cutoff {
			delete(s.perms, id)
		}
	}
	_ = s.st.PrunePermRequests(cutoff)
	_ = s.st.PruneReplyGrants(s.Now().UnixMilli())
}

// loadRelayState rebuilds the outstanding permission requests and cross-group
// reply grants from the registry at startup, dropping anything already past its
// TTL. Without this a daemon restart left a worker waiting on a dialog whose
// verdict could no longer be matched, and revoked reply licences mid-conversation.
func (s *Service) loadRelayState() {
	now := s.Now().UnixMilli()
	permCutoff := s.Now().Add(-permRequestTTL).UnixMilli()

	perms, err := s.st.LoadPermRequests()
	grants, gerr := s.st.LoadReplyGrants()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		for id, pr := range perms {
			if pr.CreatedAt < permCutoff {
				continue // expired while the daemon was down
			}
			s.perms[id] = &permRequest{workerID: pr.WorkerID, resolved: pr.Resolved, at: pr.CreatedAt}
		}
	}
	if gerr == nil {
		for _, g := range grants {
			if g.ExpiresAt <= now {
				continue
			}
			s.replyGrants[g.Replier+"\x00"+g.Sender] = g.ExpiresAt
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

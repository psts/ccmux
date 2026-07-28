package peers

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// fakeHook is a canned manager: pane→group assignments and spawnable repos.
// Mutex-guarded — the service calls SpawnEphemeralPane from a goroutine.
type fakeHook struct {
	mu     sync.Mutex
	shells map[string]bool   // paneID -> foreground is a bare shell right now
	groups map[string]string // paneID -> group ("" allowed: known but ungrouped)
	repos  map[string]string // group\x00name -> wsID:repoPath
	spawns []string          // recorded SpawnEphemeralPane calls "wsID|cwd|cmd"
}

func (f *fakeHook) GroupForPane(paneID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[paneID]
	return g, ok
}

func (f *fakeHook) PaneAtShell(paneID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shells[paneID]
}

func (f *fakeHook) setShell(paneID string, atShell bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shells[paneID] = atShell
}

func (f *fakeHook) LiveWorkspaceForRepo(group, name string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.repos[group+"\x00"+name]
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(v, ":", 2)
	return parts[0], parts[1], true
}

func (f *fakeHook) SpawnEphemeralPane(wsID, cwd, cmd, createdBy string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spawns = append(f.spawns, wsID+"|"+cwd+"|"+cmd)
	return nil
}

func (f *fakeHook) setGroup(paneID, group string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[paneID] = group
}

func (f *fakeHook) dropGroup(paneID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.groups, paneID)
}

func (f *fakeHook) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.spawns)
}

func (f *fakeHook) spawn(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns[i]
}

func newTestService(t *testing.T) (*Service, *fakeHook) {
	t.Helper()
	svc, hook, _ := newTestServiceWithStore(t)
	return svc, hook
}

// newTestServiceWithStore also hands back the store, for the presence and
// mailbox-collection tests that assert on what the database actually holds.
func newTestServiceWithStore(t *testing.T) (*Service, *fakeHook, *store.SQLite) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	hook := &fakeHook{groups: map[string]string{}, repos: map[string]string{}, shells: map[string]bool{}}
	svc := NewService(st, hook, []byte("test-secret-test-secret-test-sec"))
	svc.OpenCmd = "" // no deep links from tests
	return svc, hook, st
}

// registerPane registers a peer bound to a pane. PIDs use our own live pid so
// pane-less liveness probes pass.
func registerPane(svc *Service, paneID, cwd string) RegisterResp {
	return svc.Register(RegisterReq{PaneID: paneID, PID: os.Getpid(), CWD: cwd, GitRoot: cwd})
}

func registerPaneless(svc *Service, cwd string) RegisterResp {
	return svc.Register(RegisterReq{PID: os.Getpid(), CWD: cwd, GitRoot: cwd})
}

func TestRegister_WindowGroupAndFallback(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-1"] = "CHARTLABS"

	got := registerPane(svc, "pane-1", "/Users/x/Work/Coding/backend")
	if got.Group != "CHARTLABS" {
		t.Fatalf("pane peer group = %q, want CHARTLABS", got.Group)
	}
	if got.Name != "backend" {
		t.Fatalf("derived name = %q, want backend", got.Name)
	}

	// Pane known but workspace ungrouped → the holding folder's NAME, which is
	// what a ccmux window group is called, so the two can match.
	hook.groups["pane-2"] = ""
	if got := registerPane(svc, "pane-2", "/Users/x/Work/Coding/api"); got.Group != "Coding" {
		t.Fatalf("ungrouped pane peer group = %q, want Coding", got.Group)
	}

	// No pane at all → same rule. A session started in a plain terminal inside
	// a project lands in that project rather than being marooned by a path no
	// window group could ever equal.
	if got := registerPaneless(svc, "/Users/x/Work/Coding/cli"); got.Group != "Coding" {
		t.Fatalf("pane-less group = %q, want Coding", got.Group)
	}
}

func TestRegister_PanePeersKeepStableIDs(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-1"] = "G"
	first := registerPane(svc, "pane-1", "/r/a")
	second := registerPane(svc, "pane-1", "/r/a") // MCP server restarted
	if first.PeerID != second.PeerID {
		t.Fatalf("pane peer id changed across re-register: %s → %s", first.PeerID, second.PeerID)
	}

	// Pane-less honors requested_id.
	r1 := registerPaneless(svc, "/r/b")
	r2 := svc.Register(RegisterReq{PID: os.Getpid(), CWD: "/r/b", GitRoot: "/r/b", RequestedID: r1.PeerID})
	if r2.PeerID != r1.PeerID {
		t.Fatalf("requested_id not honored: %s → %s", r1.PeerID, r2.PeerID)
	}
}

func TestRegister_PreservesSummaryOnBlankReregister(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-1"] = "G"
	id := registerPane(svc, "pane-1", "/r/a").PeerID
	if !svc.SetSummary(id, "porting the broker") {
		t.Fatal("set summary failed")
	}
	registerPane(svc, "pane-1", "/r/a") // blank summary in re-register
	if s := svc.peers[id].Summary; s != "porting the broker" {
		t.Fatalf("summary lost on re-register: %q", s)
	}
}

func TestSend_WindowScopingAndGuards(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "MIXED"
	hook.groups["pane-b"] = "MIXED"
	hook.groups["pane-c"] = "CHARTLABS"
	a := registerPane(svc, "pane-a", "/w/ccmux").PeerID
	b := registerPane(svc, "pane-b", "/w/deck").PeerID
	c := registerPane(svc, "pane-c", "/w/app").PeerID

	if resp := svc.Send(SendReq{FromID: a, ToID: b, Text: "hi"}); !resp.OK {
		t.Fatalf("same-window send failed: %+v", resp)
	}
	// Cross-window by id with no to_group → refused, keeping the historic
	// sentence as the prefix that running sessions pattern-match on.
	if resp := svc.Send(SendReq{FromID: a, ToID: c, Text: "hi"}); resp.OK ||
		!strings.HasPrefix(resp.Error, "Cannot send messages across projects") {
		t.Fatalf("cross-window send = %+v, want guard error", resp)
	}
	// Name resolution stays inside the sender's window.
	if resp := svc.Send(SendReq{FromID: a, ToName: "app", Text: "hi"}); resp.OK ||
		!strings.Contains(resp.Error, "not found in project") {
		t.Fatalf("cross-window name resolve = %+v, want not-found", resp)
	}
	// The delivered event snapshots the sender's group.
	evs, err := svc.Poll(b)
	if err != nil || len(evs) != 1 {
		t.Fatalf("poll b: %v, %d events", err, len(evs))
	}
	if evs[0].Group != "MIXED" || evs[0].Text != "hi" || evs[0].FromID != a {
		t.Fatalf("event = %+v, want group snapshot MIXED from a", evs[0])
	}
}

func TestSend_AmbiguousNameErrors(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "G"
	hook.groups["pane-b1"] = "G"
	hook.groups["pane-b2"] = "G"
	a := registerPane(svc, "pane-a", "/w/self").PeerID
	registerPane(svc, "pane-b1", "/w/backend")
	registerPane(svc, "pane-b2", "/w/backend")

	resp := svc.Send(SendReq{FromID: a, ToName: "backend", Text: "hi"})
	if resp.OK || !strings.Contains(resp.Error, "Multiple peers named") {
		t.Fatalf("ambiguous name = %+v, want explicit ambiguity error", resp)
	}
}

func TestSend_GroupFollowsWindowRenameLive(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "OLD"
	hook.groups["pane-b"] = "OLD"
	a := registerPane(svc, "pane-a", "/w/x").PeerID
	b := registerPane(svc, "pane-b", "/w/y").PeerID

	// Window renamed (or workspace moved) between register and send: resolution
	// is live, so a and b stay in the same (new) group with no re-registration.
	hook.setGroup("pane-a", "NEW")
	hook.setGroup("pane-b", "NEW")
	if resp := svc.Send(SendReq{FromID: a, ToID: b, Text: "still works"}); !resp.OK {
		t.Fatalf("send after rename failed: %+v", resp)
	}
	// And a moved-away peer is immediately out of reach.
	hook.setGroup("pane-b", "ELSEWHERE")
	if resp := svc.Send(SendReq{FromID: a, ToID: b, Text: "now blocked"}); resp.OK {
		t.Fatal("send to moved-away peer should fail the group guard")
	}
}

func TestPoll_CursorSharedWithNoDuplicates(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "G"
	hook.groups["pane-b"] = "G"
	a := registerPane(svc, "pane-a", "/w/a").PeerID
	b := registerPane(svc, "pane-b", "/w/b").PeerID

	svc.Send(SendReq{FromID: a, ToID: b, Text: "one"})
	svc.Send(SendReq{FromID: a, ToID: b, Text: "two"})

	first, err := svc.Poll(b)
	if err != nil || len(first) != 2 {
		t.Fatalf("first poll = %d events (%v), want 2", len(first), err)
	}
	second, err := svc.Poll(b)
	if err != nil || len(second) != 0 {
		t.Fatalf("second poll = %d events, want 0 (cursor advanced)", len(second))
	}
}

func TestListPeers_ScopesAndEviction(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "MIXED"
	hook.groups["pane-b"] = "MIXED"
	hook.groups["pane-c"] = "CHARTLABS"
	a := registerPane(svc, "pane-a", "/w/a").PeerID
	registerPane(svc, "pane-b", "/w/b")
	registerPane(svc, "pane-c", "/w/c")

	if got := svc.List(a, "project", ""); len(got) != 1 || got[0].Group != "MIXED" {
		t.Fatalf("project scope = %+v, want just the MIXED sibling", got)
	}
	if got := svc.List(a, "all", ""); len(got) != 2 {
		t.Fatalf("all scope = %d peers, want 2", len(got))
	}
	// Pane disappears (workspace killed) → evicted from listings.
	hook.dropGroup("pane-b")
	if got := svc.List(a, "project", ""); len(got) != 0 {
		t.Fatalf("dead pane still listed: %+v", got)
	}
}

func TestVerdict_RoutedOnlyForOutstandingRequests(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-d"] = "G"
	worker := registerPane(svc, "pane-w", "/w/worker").PeerID
	delegator := registerPane(svc, "pane-d", "/w/delegator").PeerID

	// Delegator messages the worker → becomes a recent sender.
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "please do X"})
	svc.Poll(worker)

	relayed, err := svc.PermissionRequest(worker, "abcde", "Bash", "run tests", "go test ./...")
	if err != nil || relayed != 1 {
		t.Fatalf("relayed = %d (%v), want 1", relayed, err)
	}
	// The relay text reached the delegator with the frozen wording.
	evs, _ := svc.Poll(delegator)
	if len(evs) != 1 || !strings.HasPrefix(evs[0].Text, "[claude-peers permission relay]") {
		t.Fatalf("delegator inbox = %+v, want relay message", evs)
	}
	if !strings.Contains(evs[0].Text, `"yes abcde"`) {
		t.Fatalf("relay text missing verdict instruction: %s", evs[0].Text)
	}

	// The "yes abcde" reply becomes a structured verdict, not a chat message.
	if resp := svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "yes abcde"}); !resp.OK {
		t.Fatalf("verdict send failed: %+v", resp)
	}
	evs, _ = svc.Poll(worker)
	if len(evs) != 1 || evs[0].Kind != model.PeerEventVerdict ||
		evs[0].RequestID != "abcde" || evs[0].Behavior != "allow" {
		t.Fatalf("worker inbox = %+v, want one allow verdict", evs)
	}

	// Second verdict for the same id: dropped silently (first wins).
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no abcde"})
	if evs, _ = svc.Poll(worker); len(evs) != 0 {
		t.Fatalf("late verdict leaked through: %+v", evs)
	}

	// An id with no outstanding request is a NORMAL message (the old client
	// regex silently ate these).
	svc.Send(SendReq{FromID: delegator, ToID: worker, Text: "no qwert"})
	evs, _ = svc.Poll(worker)
	if len(evs) != 1 || evs[0].Kind != model.PeerEventMessage || evs[0].Text != "no qwert" {
		t.Fatalf("unknown-id reply = %+v, want plain message", evs)
	}
}

func TestRelay_RecentSendersWindow(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-w"] = "G"
	hook.groups["pane-old"] = "G"
	hook.groups["pane-new"] = "G"
	worker := registerPane(svc, "pane-w", "/w/w").PeerID
	old := registerPane(svc, "pane-old", "/w/old").PeerID
	recent := registerPane(svc, "pane-new", "/w/new").PeerID

	base := time.Now()
	svc.Now = func() time.Time { return base.Add(-20 * time.Minute) }
	svc.Send(SendReq{FromID: old, ToID: worker, Text: "long ago"})
	svc.Now = func() time.Time { return base }
	svc.Send(SendReq{FromID: recent, ToID: worker, Text: "just now"})

	relayed, err := svc.PermissionRequest(worker, "bcdef", "Write", "write file", "{}")
	if err != nil || relayed != 1 {
		t.Fatalf("relayed = %d (%v), want only the 10-min-recent sender", relayed, err)
	}
	if evs, _ := svc.Poll(recent); len(evs) != 1 || !strings.Contains(evs[0].Text, "permission relay") {
		t.Fatalf("recent sender inbox = %+v, want exactly the relay", evs)
	}
	if evs, _ := svc.Poll(old); len(evs) != 0 {
		t.Fatalf("stale sender got relayed: %+v", evs)
	}
}

func TestSpawn_NativeInWindowThenQueuedDelivery(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "MIXED"
	hook.repos["MIXED\x00maestro"] = "ws-9:/w/maestro"
	a := registerPane(svc, "pane-a", "/w/ccmux").PeerID

	resp := svc.Send(SendReq{FromID: a, ToName: "maestro", Text: "build the thing", SpawnIfMissing: true})
	if !resp.OK || !resp.Spawning {
		t.Fatalf("spawn send = %+v, want spawning", resp)
	}
	waitFor(t, func() bool { return hook.spawnCount() == 1 })
	spawned := hook.spawn(0)
	if !strings.HasPrefix(spawned, "ws-9|/w/maestro|") ||
		!strings.Contains(spawned, "--dangerously-load-development-channels server:claude-peers") {
		t.Fatalf("native spawn call = %q, want frozen claude launch in ws-9", spawned)
	}

	// Concurrent second request queues on the same pending spawn.
	if resp := svc.Send(SendReq{FromID: a, ToName: "maestro", Text: "also this", SpawnIfMissing: true}); !resp.Spawning {
		t.Fatalf("second spawn request = %+v, want queued", resp)
	}
	waitFor(t, func() bool { return hook.spawnCount() == 1 }) // still one spawn

	// Teammate's pane registers inside the window → queued messages delivered
	// as normal peer messages from the requester.
	hook.setGroup("pane-m", "MIXED")
	teammate := registerPane(svc, "pane-m", "/w/maestro").PeerID
	evs, _ := svc.Poll(teammate)
	if len(evs) != 2 || evs[0].Text != "build the thing" || evs[0].FromID != a {
		t.Fatalf("teammate inbox = %+v, want both queued requests from a", evs)
	}
	// ...which seeds the permission relay's recent senders.
	if n, _ := svc.PermissionRequest(teammate, "cdefg", "Bash", "x", "{}"); n != 1 {
		t.Fatalf("relay after spawn reached %d, want the requester", n)
	}
}

func TestSpawn_TimeoutNotifiesRequester(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "MIXED"
	hook.repos["MIXED\x00ghost"] = "ws-1:/w/ghost"
	a := registerPane(svc, "pane-a", "/w/ccmux").PeerID
	svc.SpawnTimeout = 30 * time.Millisecond

	if resp := svc.Send(SendReq{FromID: a, ToName: "ghost", Text: "hello?", SpawnIfMissing: true}); !resp.Spawning {
		t.Fatalf("spawn = %+v", resp)
	}
	waitFor(t, func() bool {
		evs, _ := svc.Poll(a)
		return len(evs) == 1 && strings.Contains(evs[0].Text, "could not be started") &&
			evs[0].FromID == "claude-peers"
	})
}

func TestSpawn_DeepLinkTeammateJoinsRequestersGroup(t *testing.T) {
	svc, hook := newTestService(t)
	hook.groups["pane-a"] = "MIXED"
	a := registerPane(svc, "pane-a", "/repos/ccmux").PeerID

	// No live workspace hosts "sidekick" → deep-link path with the classic
	// parent-dir repo guess. Point it at a real directory.
	repoParent := t.TempDir()
	repo := filepath.Join(repoParent, "sidekick")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	svc.peers[a].GitRoot = filepath.Join(repoParent, "ccmux")
	svc.peers[a].CWD = svc.peers[a].GitRoot
	svc.OpenCmd = "/usr/bin/true" // swallow the deep link

	if resp := svc.Send(SendReq{FromID: a, ToName: "sidekick", Text: "task", SpawnIfMissing: true}); !resp.Spawning {
		t.Fatalf("deep-link spawn = %+v", resp)
	}

	// The teammate comes up as a Mac-local ephemeral pane: pane-less, dirname
	// fallback group ≠ MIXED. Registration must pin it into MIXED via the
	// pending-spawn repo match, or requester and teammate can't talk.
	got := svc.Register(RegisterReq{PID: os.Getpid(), CWD: repo, GitRoot: repo})
	if got.Group != "MIXED" {
		t.Fatalf("deep-link teammate group = %q, want pinned MIXED", got.Group)
	}
	evs, _ := svc.Poll(got.PeerID)
	if len(evs) != 1 || evs[0].Text != "task" {
		t.Fatalf("teammate inbox = %+v, want the queued task", evs)
	}
	// Round trip works despite the teammate being pane-less.
	if resp := svc.Send(SendReq{FromID: got.PeerID, ToID: a, Text: "done"}); !resp.OK {
		t.Fatalf("teammate reply blocked: %+v", resp)
	}
}

func TestLocalPaneGroups_LiveWindowGroupingForDriverPanes(t *testing.T) {
	svc, _ := newTestService(t)

	// A Mac-local pane session registers pane-less but with its pane UUID.
	uuid := "C94A648A-D8B2-4A36-93FB-CED728437CED"
	got := svc.Register(RegisterReq{LocalPaneID: uuid, PID: os.Getpid(),
		CWD: "/w/ChartLabs/backend", GitRoot: "/w/ChartLabs/backend"})
	if got.Group != "ChartLabs" {
		t.Fatalf("before map push: group = %q, want the holding folder's name", got.Group)
	}
	// Stable id derived from the pane UUID: an MCP-server restart keeps it.
	again := svc.Register(RegisterReq{LocalPaneID: uuid, PID: os.Getpid(),
		CWD: "/w/ChartLabs/backend", GitRoot: "/w/ChartLabs/backend"})
	if again.PeerID != got.PeerID {
		t.Fatalf("local-pane peer id changed across re-register: %s → %s", got.PeerID, again.PeerID)
	}

	// The Mac app pushes its map (lowercase-insensitive) → group flips LIVE,
	// no re-registration.
	svc.SetLocalPaneGroups(map[string]string{uuid: "CHARTLABS"})
	other := svc.Register(RegisterReq{LocalPaneID: "AAAA1111-0000-0000-0000-000000000000",
		PID: os.Getpid(), CWD: "/w/ChartLabs/admin", GitRoot: "/w/ChartLabs/admin"})
	svc.SetLocalPaneGroups(map[string]string{
		uuid:                                   "CHARTLABS",
		"aaaa1111-0000-0000-0000-000000000000": "CHARTLABS",
	})
	if resp := svc.Send(SendReq{FromID: got.PeerID, ToName: "admin", Text: "hi"}); !resp.OK {
		t.Fatalf("same-window (via map) send failed: %+v", resp)
	}
	if evs, _ := svc.Poll(other.PeerID); len(evs) != 1 || evs[0].Group != "CHARTLABS" {
		t.Fatalf("event = %+v, want group snapshot CHARTLABS", evs)
	}

	// Window rename: app pushes a replacement map → both re-group instantly.
	svc.SetLocalPaneGroups(map[string]string{
		uuid:                                   "RENAMED",
		"aaaa1111-0000-0000-0000-000000000000": "ELSEWHERE",
	})
	if resp := svc.Send(SendReq{FromID: got.PeerID, ToID: other.PeerID, Text: "blocked"}); resp.OK {
		t.Fatal("send should fail after panes moved to different window groups")
	}
	// Map entry gone (pane closed / app quit) → folder-name fallback returns.
	svc.SetLocalPaneGroups(map[string]string{})
	if g := svc.groupOfLocked(svc.peers[got.PeerID]); g != "ChartLabs" {
		t.Fatalf("after map clear: group = %q, want the holding folder's name", g)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

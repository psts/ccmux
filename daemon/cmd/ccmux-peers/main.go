// Command ccmux-peers is the per-session claude-peers MCP server, thin-client
// edition: all durable state (delivery cursor, recent senders, outstanding
// permission requests, pending spawns) lives in ccmuxd — this process holds
// only its identity and connections. Wire it exactly like the old bun server:
//
//	claude mcp add --scope user --transport stdio claude-peers -- /path/to/ccmux-peers
//	claude --dangerously-load-development-channels server:claude-peers
//
// Identity: hosted panes carry CCMUX_DAEMON_URL / CCMUX_PANE_ID /
// CCMUX_PANE_TOKEN in their environment; sessions outside ccmux read the
// daemon-info file (~/Library/Application Support/ccmuxd/peers.json) and land
// in the parent-directory fallback group. CCMUX_PEERS_CHANNEL=0 opts a session
// out of live push (check_messages becomes the delivery path).
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/peers"
)

// supportedProtocolVersions are the MCP revisions this server speaks, newest
// first. The spec's rule is that client and server MAY each support several
// revisions but MUST agree on one, so the set — not a single pin — is what makes
// negotiation honest.
//
// Both entries are listed because nothing in 2025-11-25 changes what a tools-only
// stdio server must do: its breaking changes are OAuth/OIDC discovery, optional
// icons, elicitation, sampling tool-calls and experimental tasks, none of which
// this server implements or needs. It also explicitly permits stdio servers to use
// stderr for all logging, which is what logf already does.
//
// Older revisions are deliberately absent. 2025-03-26 and 2024-11-05 allowed
// batched JSON-RPC, and mcpServer.Serve unmarshals one request per line — a batch
// would be dropped as a bad frame. Claiming them would be claiming a capability
// this transport does not have.
//
// Add a revision here only alongside the code that makes it true.
var supportedProtocolVersions = []string{"2025-11-25", "2025-06-18"}

// shimVersion travels in the register payload (and MCP serverInfo) so the
// daemon — the hub, in federation — can tell what a connected shim speaks.
// Today's wire tolerates absent-field-means-old-client; this makes the next
// change diagnosable instead of inferential. 0.3.0 added delegate/update_task.
const shimVersion = "0.3.0"

// negotiateProtocolVersion picks the revision to run the session at. When the
// client asks for one this server speaks, that is the answer. Otherwise it answers
// with the newest it does speak and leaves the decision where the spec puts it:
// the client either accepts the older revision or terminates the connection.
//
// What it must never do is echo an unknown version back. That was the original
// behavior, and it amounted to claiming support for anything asked for — so a
// client offering, say, 2026-07-28, whose stateless core removes the
// server-initiated notifications this server's whole delivery path is built on,
// would get a "yes" and then proceed against a contract this process does not
// implement.
func negotiateProtocolVersion(requested string) string {
	for _, v := range supportedProtocolVersions {
		if requested == v {
			return v
		}
	}
	return supportedProtocolVersions[0]
}

type app struct {
	mcp         *mcpServer
	daemon      *daemonClient
	channelMode bool
	paneID      string
	localPaneID string // Mac driver-mode pane UUID (from CCMUX_CMD_FILE)
	name        string
	cwd         string
	gitRoot     string

	// localURL/localToken address THIS pane's own daemon, kept separate from
	// a.daemon because a.daemon may be pointed at the hub. Empty means "do not
	// ask" — a pane-less session, or an older daemon that stamped the bus into
	// env and gave us a token only that bus accepts.
	localURL   string
	localToken string

	mu     sync.Mutex
	id     string // assigned by ccmuxd on register
	grp    string
	regReq map[string]any
	// shownSeq is the highest event seq this process has actually put in front of
	// the model. It is the dedupe authority, and it has to live here rather than
	// in the daemon: the daemon's cursor only advances when this process acks,
	// and it acks only after a notification write succeeds, so between a push and
	// its ack the daemon legitimately still considers the event undelivered. A
	// concurrent check_messages would then hand back something the session has
	// already seen.
	//
	// Deduping here instead of skipping unacked events in the daemon also keeps
	// the failure direction safe: an event whose notification write FAILED never
	// reaches shownSeq, so check_messages still returns it. A daemon-side skip
	// would have swallowed exactly that case.
	shownSeq int64
}

func (a *app) peerID() string { a.mu.Lock(); defer a.mu.Unlock(); return a.id }
func (a *app) group() string  { a.mu.Lock(); defer a.mu.Unlock(); return a.grp }

// markShown records that seq reached the model, and reports whether that was
// news. Out-of-order and repeat seqs are absorbed: only a forward move counts.
func (a *app) markShown(seq int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq <= a.shownSeq {
		return false
	}
	a.shownSeq = seq
	return true
}

// alreadyShown reports whether seq has already been put in front of the model.
// A zero seq is never suppressed: it means the sender did not number the event,
// so there is nothing to dedupe on and dropping it would lose a message.
func (a *app) alreadyShown(seq int64) bool {
	if seq == 0 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return seq <= a.shownSeq
}

func main() {
	a := &app{mcp: newMCPServer(), channelMode: os.Getenv("CCMUX_PEERS_CHANNEL") != "0"}
	a.cwd, _ = os.Getwd()
	a.gitRoot = gitRoot(a.cwd)
	a.name = os.Getenv("CLAUDE_PEERS_NAME")
	a.paneID = os.Getenv("CCMUX_PANE_ID")
	a.localPaneID = localPaneID(os.Getenv("CCMUX_CMD_FILE"))

	// The local daemon is always where we START. Which bus we END UP on is asked
	// for, not inherited: resolveBus queries this daemon's live tag:ccmux-hub
	// discovery before every registration. CCMUX_PEERS_URL is only still read so
	// a new shim keeps working against an older daemon that stamps it; it is no
	// longer written, because a value frozen into pane env at session-creation
	// time cannot follow a hub that appears later.
	url := os.Getenv("CCMUX_DAEMON_URL")
	token := os.Getenv("CCMUX_PANE_TOKEN")
	a.localURL, a.localToken = url, token
	if legacy := os.Getenv("CCMUX_PEERS_URL"); legacy != "" {
		// Older daemon: it stamped the hub AND minted a hub token, so the token
		// we have is not valid against the local daemon. Use what it gave us and
		// ask nobody — clearing localURL disables resolution for this process.
		url = legacy
		a.localURL, a.localToken = "", ""
	}
	if a.paneID == "" || url == "" || token == "" {
		// Not a fully-tokened hosted pane → register pane-less (dirname fallback
		// group) with the daemon-info file's shared credentials.
		a.paneID = ""
		url, token = readDaemonInfo()
		a.localURL, a.localToken = "", "" // no pane identity to authorize a resolve with
	}
	if url == "" {
		logf("no ccmuxd found (env or daemon-info file) — tools will error until it appears")
		url, token = "http://127.0.0.1:7890", ""
	}
	a.daemon = newDaemonClient(url, token)
	logf("cwd=%s git_root=%s pane=%s daemon=%s channel=%v", a.cwd, a.gitRoot, orID(a.paneID, "(none)"), url, a.channelMode)

	a.installHandlers()

	// Join the bus in the background — never block the MCP handshake. busLoop
	// keeps us registered only while we're the pane's interactive session.
	go a.busLoop()

	// Serve stdio until the parent closes the pipe, then unregister and exit —
	// an orphaned thin client must not hold a stale registration.
	a.mcp.Serve()
	a.unregister()
	logf("stdin closed, exiting")
}

// busLoop keeps this process registered and connected to the peers bus only
// while it is the pane's interactive session (isBusOwner). Sub-agents and warm
// spares that share the pane's derived identity stay dormant here, re-checking
// so a claimed spare promotes itself; a session that loses the pane's terminal
// sheds its registration. Runs for the process lifetime.
func (a *app) busLoop() {
	const recheck = 3 * time.Second
	delay := time.Second
	for {
		if !isBusOwner() {
			a.unregister() // no-op unless we were the owner and just lost it
			time.Sleep(recheck)
			continue
		}
		a.resolveBus()
		if a.peerID() == "" {
			if err := a.register(); err != nil {
				logf("register: %v (retrying in %s)", err, delay)
				time.Sleep(delay)
				delay = min(delay*2, 15*time.Second)
				continue
			}
			delay = time.Second
			logf("registered as %s (name %s, group %s)", a.peerID(), a.name, a.group())
		}
		// Hold the inbox until the channel drops or we lose ownership; both
		// return here so we re-check and, if still owner, reconnect.
		if a.channelMode {
			a.runPushLoop()
		} else {
			a.keepRegistered()
		}
	}
}

// resolveBus asks THIS pane's own daemon which peers bus to be on, and moves if
// the answer changed. Called before every registration attempt, which is what
// makes hub membership follow tag:ccmux-hub instead of a value frozen into pane
// environment when the session was created — panes routinely outlive the daemon,
// and a hub can appear, move, or go away while one is running.
//
// Any failure is a no-op on purpose: an older daemon 404s this route, an
// unreachable one errors, and either way staying put is the safe answer.
func (a *app) resolveBus() {
	if a.localURL == "" || a.paneID == "" {
		return
	}
	local := newDaemonClient(a.localURL, a.localToken)
	var resp struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := local.post("/v1/peers/bus", map[string]any{"pane_id": a.paneID}, &resp); err != nil {
		return
	}
	url, token := resp.URL, resp.Token
	if url == "" { // no hub discovered — our own daemon is the bus
		url, token = a.localURL, a.localToken
	}
	curURL, curToken := a.daemon.target()
	if curURL == strings.TrimRight(url, "/") && curToken == token {
		return
	}
	// Unregister BEFORE moving: a.unregister posts through a.daemon, so once the
	// client is retargeted the request would go to the bus we are joining rather
	// than the one we are leaving, and the old one would keep listing us until
	// its reaper timed us out.
	a.unregister()
	a.daemon.retarget(url, token)
	logf("bus moved to %s — re-registering", url)
}

// unregister releases our registration and clears our id (so busLoop re-joins if
// we become the owner again). Safe to call when not registered.
func (a *app) unregister() {
	id := a.peerID()
	if id == "" {
		return
	}
	_ = a.daemon.post("/v1/peers/unregister", map[string]any{"peer_id": id}, nil)
	a.mu.Lock()
	a.id = ""
	a.mu.Unlock()
}

func (a *app) register() error {
	a.mu.Lock()
	req := map[string]any{
		"pane_id": a.paneID, "local_pane_id": a.localPaneID, "pid": os.Getpid(),
		"cwd": a.cwd, "git_root": a.gitRoot,
		"name": a.name, "requested_id": a.id,
		"poll_only": !a.channelMode, "shim_version": shimVersion,
	}
	a.mu.Unlock()
	var resp struct {
		PeerID string `json:"peer_id"`
		Name   string `json:"name"`
		Group  string `json:"group"`
	}
	if err := a.daemon.post("/v1/peers/register", req, &resp); err != nil {
		return err
	}
	a.mu.Lock()
	a.id, a.grp = resp.PeerID, resp.Group
	a.mu.Unlock()
	a.name = resp.Name
	return nil
}

func (a *app) installHandlers() {
	a.mcp.onRequest["initialize"] = func(params json.RawMessage) (any, *rpcError) {
		var in struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &in)
		agreed := negotiateProtocolVersion(in.ProtocolVersion)
		if in.ProtocolVersion != agreed {
			logf("client offered MCP %q; answering with %s (newest this server speaks)",
				in.ProtocolVersion, agreed)
		}
		return map[string]any{
			"protocolVersion": agreed,
			"capabilities": map[string]any{
				"tools": map[string]any{},
				"experimental": map[string]any{
					"claude/channel":            map[string]any{},
					"claude/channel/permission": map[string]any{},
				},
			},
			"serverInfo":   map[string]any{"name": "claude-peers", "version": shimVersion},
			"instructions": serverInstructions,
		}, nil
	}
	// 2026-07-28 clients probe with server/discover before falling back to the
	// legacy handshake. Answering it honestly — these legacy revisions, nothing
	// newer — turns that probe into a clean fallback instead of a method-not-found.
	// Flipping the bus to the modern era later is a supportedVersions change here,
	// made only alongside the code that implements the era's semantics.
	a.mcp.onRequest["server/discover"] = func(json.RawMessage) (any, *rpcError) {
		logf("client probed server/discover; advertising legacy revisions %v", supportedProtocolVersions)
		return map[string]any{
			"resultType":        "complete",
			"supportedVersions": supportedProtocolVersions,
			"capabilities": map[string]any{
				"tools": map[string]any{},
				"experimental": map[string]any{
					"claude/channel":            map[string]any{},
					"claude/channel/permission": map[string]any{},
				},
			},
			"instructions": serverInstructions,
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": "claude-peers", "version": shimVersion},
			},
		}, nil
	}
	a.mcp.onRequest["ping"] = func(json.RawMessage) (any, *rpcError) {
		return map[string]any{}, nil
	}
	a.mcp.onRequest["tools/list"] = func(json.RawMessage) (any, *rpcError) {
		return map[string]any{"tools": toolsList}, nil
	}
	a.mcp.onRequest["tools/call"] = func(params json.RawMessage) (any, *rpcError) {
		var in struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return a.callTool(in.Name, in.Args), nil
	}
	a.mcp.onNotify["notifications/initialized"] = func(json.RawMessage) {}
	a.mcp.onNotify["notifications/cancelled"] = func(json.RawMessage) {}

	// Claude Code opened a tool-approval dialog → hand it to ccmuxd, which
	// relays to whoever delegated work to this session recently.
	a.mcp.onNotify["notifications/claude/channel/permission_request"] = func(params json.RawMessage) {
		var in struct {
			RequestID    string `json:"request_id"`
			ToolName     string `json:"tool_name"`
			Description  string `json:"description"`
			InputPreview string `json:"input_preview"`
		}
		if err := json.Unmarshal(params, &in); err != nil || a.peerID() == "" {
			return
		}
		var resp struct {
			RelayedTo int `json:"relayed_to"`
		}
		if err := a.daemon.post("/v1/peers/permission-request", map[string]any{
			"peer_id": a.peerID(), "request_id": in.RequestID,
			"tool_name": in.ToolName, "description": in.Description,
			"input_preview": in.InputPreview,
		}, &resp); err != nil {
			logf("permission relay failed: %v", err)
			return
		}
		logf("relayed permission_request %s (%s) to %d recent sender(s)", in.RequestID, in.ToolName, resp.RelayedTo)
	}
}

// keepRegistered is the poll-only session's substitute for the push loop's
// reconnect-and-re-register: a periodic idempotent re-register, so a daemon
// restart doesn't strand the peer until its own process restarts. Returns to
// busLoop if this process stops being the pane's session.
func (a *app) keepRegistered() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		if !isBusOwner() {
			return
		}
		if err := a.register(); err != nil {
			logf("keepalive register: %v", err)
		}
	}
}

// readDaemonInfo loads the pane-less discovery file (plain terminals have no
// CCMUX_DAEMON_URL in their environment).
func readDaemonInfo() (url, token string) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", ""
	}
	info, err := peers.ReadDaemonInfo(filepath.Join(cfg, "ccmuxd", "peers.json"))
	if err != nil {
		return "", ""
	}
	return info.URL, info.Token
}

func gitRoot(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localPaneID extracts the Mac driver-mode pane UUID from a CCMUX_CMD_FILE
// path (/tmp/ccmux-cmd-<uuid>) — the identity the app's live local-pane→window
// map is keyed by. Hosted panes have the same env var, but their CCMUX_PANE_ID
// takes precedence during registration, so passing both is harmless.
func localPaneID(cmdFile string) string {
	base := filepath.Base(cmdFile)
	if id, ok := strings.CutPrefix(base, "ccmux-cmd-"); ok && id != "" {
		return id
	}
	return ""
}

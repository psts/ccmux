// Package api serves ccmuxd's REST + WebSocket surface: the wire contract shared
// by every lens (native app, web, phone).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/tailnet"
	"ccmux.dev/ccmuxd/internal/version"
	"ccmux.dev/ccmuxd/web"
)

// whoisResolver maps a connection's remote address to a verified tailnet
// identity. *tailnet.Resolver satisfies it; tests inject a fake so they never
// shell out to `tailscale whois`.
type whoisResolver interface {
	Resolve(remoteAddr string) (login, display string, ok bool)
}

// Server adapts a Manager to HTTP.
type Server struct {
	mgr      *manager.Manager
	presence *presenceHub
	identity whoisResolver
	upgrader websocket.Upgrader

	// Push notifications, wired by EnablePush; nil when push is disabled (the
	// /v1/push/* handlers then answer 503 and no notifier runs).
	sender    pushSender
	pushStore pushStore

	// projectsRoot is the one folder whose direct subdirectories are offered as
	// hosted-workspace locations (GET /v1/projects). Empty disables the listing
	// (503) — main always sets it.
	projectsRoot string

	// peersSvc is the built-in peers messaging bus, wired by EnablePeers; nil
	// when disabled (the /v1/peers/* handlers then answer 503).
	peersSvc *peers.Service

	// devStatus reports the dev-hostname wildcard-cert lifecycle for the
	// settings UI, wired by SetDevhostStatus; nil when dev serving is off.
	devStatus func() string

	// hub, when set by EnableHub, makes this the federation hub: it aggregates
	// every member host's workspaces and reverse-proxies host-scoped routes to
	// the owning host. nil in host-only mode.
	hub *hubMode
}

func NewServer(mgr *manager.Manager) *Server {
	return &Server{
		mgr:      mgr,
		presence: newPresenceHub(mgr),
		identity: tailnet.NewResolver(),
		// Same-origin default; the web lens is served from this daemon, and
		// tailnet identity gates access. Loosened checks come with auth.
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
}

// SetIdentityResolver swaps the identity backend. main injects a tsnet
// LocalClient-backed resolver once the daemon's own tailnet node is up; the
// default (NewServer) is the `tailscale whois` CLI resolver for a direct-tailnet
// or dev deployment.
func (s *Server) SetIdentityResolver(r whoisResolver) { s.identity = r }

// SetProjectsRoot sets the folder GET /v1/projects lists (see projectsRoot).
func (s *Server) SetProjectsRoot(root string) { s.projectsRoot = root }

// SetDevhostStatus wires the devhost server's cert-status reporter (see devStatus).
func (s *Server) SetDevhostStatus(f func() string) { s.devStatus = f }

// EnablePush wires Web Push: it stores the sender + subscription store the
// /v1/push/* handlers use, and starts a notifier that pushes on attention (with
// per-dev suppression) for the lifetime of ctx. Idempotent-safe to call once at
// startup; the notifier's firehose subscription is released when ctx is cancelled.
func (s *Server) EnablePush(ctx context.Context, sender pushSender, ps pushStore) {
	s.sender = sender
	s.pushStore = ps
	n := &notifier{sender: sender, subs: ps, focus: s.presence, names: s.mgr}
	if s.hub != nil {
		// Hub owns push: notify on attention across ALL member hosts (merged
		// events) with merged presence suppression, so it never over-notifies a
		// user watching a remote-host session directly. Member hosts' own
		// notifiers stay inert (subscriptions live at the hub).
		n.focus = s.newFederatedFocus(ctx)
		go n.run(ctx, s.mergedNotifierEvents(ctx))
		return
	}
	id, ch := s.mgr.SubscribeEvents()
	go func() {
		defer s.mgr.UnsubscribeEvents(id)
		n.run(ctx, ch)
	}()
}

// Handler builds the routed HTTP handler (Go 1.22+ method+wildcard patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/projects", s.listProjects)
	mux.HandleFunc("GET /v1/settings", s.getSettings)
	mux.HandleFunc("PUT /v1/settings", s.putSettings)
	// GET /v1/workspaces is the aggregated list in hub mode, local otherwise.
	// The hub also exposes the registry and explicit per-host create/projects.
	if s.hub != nil {
		mux.HandleFunc("GET /v1/hosts", s.hub.listHosts)
		mux.HandleFunc("GET /v1/workspaces", s.hub.listWorkspaces)
		mux.HandleFunc("GET /v1/hosts/{host}/projects", s.hub.hostScoped(s.listProjects, "/v1/projects"))
		mux.HandleFunc("POST /v1/hosts/{host}/workspaces", s.hub.hostScoped(s.createWorkspace, "/v1/workspaces"))
		// Per-host settings: the lens configures any member host's startup command,
		// dev domain, tokens, etc. through the hub (self runs local).
		mux.HandleFunc("GET /v1/hosts/{host}/settings", s.hub.hostScoped(s.getSettings, "/v1/settings"))
		mux.HandleFunc("PUT /v1/hosts/{host}/settings", s.hub.hostScoped(s.putSettings, "/v1/settings"))
	} else {
		mux.HandleFunc("GET /v1/workspaces", s.listWorkspaces)
	}
	mux.HandleFunc("POST /v1/workspaces", s.createWorkspace) // bare = create on this node
	// Workspace/pane-scoped routes: s.scoped proxies them to the owning host in
	// hub mode (self runs local), and is a no-op in host-only mode.
	mux.HandleFunc("DELETE /v1/workspaces/{id}", s.scoped(s.deleteWorkspace))
	mux.HandleFunc("POST /v1/workspaces/{id}/panes", s.scoped(s.spawnPane))
	mux.HandleFunc("DELETE /v1/workspaces/{id}/panes/{paneId}", s.scoped(s.killPane))
	mux.HandleFunc("POST /v1/workspaces/{id}/archive", s.scoped(s.archiveWorkspace))
	mux.HandleFunc("POST /v1/workspaces/{id}/revive", s.scoped(s.reviveWorkspace))
	mux.HandleFunc("PUT /v1/workspaces/{id}/layout", s.scoped(s.putLayout))
	mux.HandleFunc("PUT /v1/workspaces/{id}/group", s.scoped(s.putGroup))
	mux.HandleFunc("PUT /v1/workspaces/{id}/hostnames", s.hostnamesRoute(s.putHostnames))
	mux.HandleFunc("GET /v1/workspaces/{id}/port-suggestions", s.scoped(s.portSuggestions))
	mux.HandleFunc("POST /v1/workspaces/{id}/dev-server", s.scoped(s.devServer))
	mux.HandleFunc("GET /v1/panes/{id}/snapshot", s.scoped(s.paneSnapshot))
	mux.HandleFunc("GET /v1/panes/{id}/driver", s.scoped(s.paneDriver))
	mux.HandleFunc("GET /v1/push/vapid", s.pushVAPID)
	mux.HandleFunc("GET /v1/push/subscriptions", s.listSubscriptions)
	mux.HandleFunc("POST /v1/push/subscriptions", s.createSubscription)
	mux.HandleFunc("DELETE /v1/push/subscriptions", s.deleteSubscription)
	mux.HandleFunc("GET /v1/attach", s.attach)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("GET /v1/presence", s.presenceOwners) // hub polls members to union push suppression
	mux.HandleFunc("POST /v1/peers/register", s.peersRegister)
	mux.HandleFunc("POST /v1/peers/pane-token", s.peersMintPaneToken)
	mux.HandleFunc("POST /v1/peers/send", s.peersSend)
	mux.HandleFunc("POST /v1/peers/list", s.peersList)
	mux.HandleFunc("POST /v1/peers/summary", s.peersSummary)
	mux.HandleFunc("POST /v1/peers/unregister", s.peersUnregister)
	mux.HandleFunc("POST /v1/peers/poll", s.peersPoll)
	mux.HandleFunc("POST /v1/peers/permission-request", s.peersPermissionRequest)
	mux.HandleFunc("PUT /v1/peers/local-groups", s.peersLocalGroups)
	mux.HandleFunc("GET /v1/peers/ws", s.peersWS)
	mux.HandleFunc("GET /v1/peers/messages", s.peersGroupMessages)
	mux.HandleFunc("GET /v1/peers", s.peersGroupPeers)
	// The web lens (served from the embedded bundle) catches everything not
	// matched by a more specific /v1 pattern.
	mux.Handle("GET /", http.FileServerFS(web.Files))
	return mux
}

// health reports liveness plus the federation handshake fields: the informational
// build string and the wire-contract integer the hub gates host compatibility on
// (see internal/version, daemon/docs/multihost-plan.md).
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"version":  version.Build,
		"contract": version.Contract,
	})
}

func (s *Server) listWorkspaces(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

// getSettings/putSettings expose the daemon-wide lens settings: the global
// new-workspace startup command plus per-folder rules. Setting the command to
// "" resets to the built-in default, which GET always reports resolved. An
// optional ?repoPath= adds resolvedStartupCommand — what a workspace created
// there would actually run — for creation-time previews in the pickers.
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"startupCommand": s.mgr.DefaultStartupCommand(),
		"startupRules":   s.mgr.StartupRules(),
		// Dev hostnames: secrets are write-only — GET reports presence, never values.
		"devDomain":           s.mgr.DevDomain(),
		"cloudflareTokenSet":  s.mgr.CloudflareToken() != "",
		"tailscaleAuthKeySet": s.mgr.TailscaleAuthKey() != "",
		"devCertStatus":       s.devCertStatus(),
		// Which self-declared names are aliased, never what they map to. The
		// values are verified logins — emails — and this handler is unauthenticated,
		// so it follows the same write-only rule as the secrets above. The names
		// alone answer the question you'd read this for: is my lens aliased or not.
		"identityAliasNames": aliasNames(s.mgr.IdentityAliases()),
	}
	if repo := r.URL.Query().Get("repoPath"); repo != "" {
		resp["resolvedStartupCommand"] = s.mgr.StartupCommandFor(repo)
	}
	writeJSON(w, http.StatusOK, resp)
}

// devCertStatus reports the wildcard-cert lifecycle for the settings UI. The
// devhost server injects the live reporter; without one (tests, -tsnet off)
// only the unset/unknown distinction is available.
func (s *Server) devCertStatus() string {
	if s.devStatus != nil {
		return s.devStatus()
	}
	if s.mgr.DevDomain() == "" {
		return "unset"
	}
	return "unknown"
}

// settingsRequest is the PUT body. Every field is a pointer so an absent key
// means "leave this alone" rather than "set it to the zero value".
type settingsRequest struct {
	StartupCommand   *string                `json:"startupCommand"`
	StartupRules     *[]manager.StartupRule `json:"startupRules"`
	DevDomain        *string                `json:"devDomain"`
	CloudflareToken  *string                `json:"cloudflareToken"`
	TailscaleAuthKey *string                `json:"tailscaleAuthKey"`
	// Replaces the whole alias map rather than merging: an alias you couldn't
	// remove by sending the map without it would be a trap.
	IdentityAliases *map[string]string `json:"identityAliases"`
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg, ok := s.rejectSettings(req); !ok {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if err := s.applySettings(req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.getSettings(w, r)
}

// rejectSettings validates the request against the state it would produce,
// before anything is persisted. It returns the message to send and ok=false when
// the request must be refused.
func (s *Server) rejectSettings(req settingsRequest) (string, bool) {
	// Custom-domain mode is unusable without a token to issue certs with.
	domain, token := s.mgr.DevDomain(), s.mgr.CloudflareToken()
	if req.DevDomain != nil {
		domain = strings.TrimSpace(*req.DevDomain)
	}
	if req.CloudflareToken != nil {
		token = strings.TrimSpace(*req.CloudflareToken)
	}
	if domain != "" && token == "" {
		return "devDomain requires a cloudflareToken for DNS-01 certs", false
	}
	// An alias row missing either side is the caller's mistake, not something to
	// silently drop and then answer 200 to.
	if req.IdentityAliases != nil {
		for name, login := range *req.IdentityAliases {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(login) == "" {
				return manager.ErrIncompleteAlias.Error(), false
			}
		}
	}
	return "", true
}

// applySettings persists every field the request carried. Each setter takes the
// dereferenced value; a nil field is skipped.
func (s *Server) applySettings(req settingsRequest) error {
	strs := []struct {
		val *string
		set func(string) error
	}{
		{req.DevDomain, s.mgr.SetDevDomain},
		{req.CloudflareToken, s.mgr.SetCloudflareToken},
		{req.TailscaleAuthKey, s.mgr.SetTailscaleAuthKey},
		{req.StartupCommand, func(v string) error { return s.mgr.SetDefaultStartupCommand(strings.TrimSpace(v)) }},
	}
	for _, f := range strs {
		if f.val == nil {
			continue
		}
		if err := f.set(*f.val); err != nil {
			return err
		}
	}
	if req.IdentityAliases != nil {
		if err := s.mgr.SetIdentityAliases(*req.IdentityAliases); err != nil {
			return err
		}
	}
	if req.StartupRules != nil {
		return s.mgr.SetStartupRules(*req.StartupRules)
	}
	return nil
}

// aliasNames lists the aliased names, dropping the logins they map to. Sorted so
// the settings response is stable between reads.
func aliasNames(aliases map[string]string) []string {
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// putHostnames replaces a workspace's dev-hostname mappings ({name, port}
// rows from the app's Hostnames sheet) and, when present, its dev-server
// command override. Validation and the tailnet-wide uniqueness check live in
// the manager; success returns the updated workspace.
func (s *Server) putHostnames(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostnames  []model.Hostname `json:"hostnames"`
		DevCommand *string          `json:"devCommand"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	ws, err := s.mgr.SetHostnames(r.PathValue("id"), req.Hostnames)
	if err == nil && req.DevCommand != nil {
		err = s.mgr.SetDevCommand(r.PathValue("id"), *req.DevCommand)
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, manager.ErrUnknownWorkspace) {
			code = http.StatusNotFound
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// devServer starts or stops the workspace's dev-server pane.
func (s *Server) devServer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	var ws *model.Workspace
	var err error
	switch req.Action {
	case "start":
		ws, err = s.mgr.StartDevServer(r.PathValue("id"))
	case "stop":
		ws, err = s.mgr.StopDevServer(r.PathValue("id"))
	default:
		writeError(w, http.StatusBadRequest, "action must be start or stop")
		return
	}
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, manager.ErrUnknownWorkspace) {
			code = http.StatusNotFound
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// portSuggestions proposes {name, port, source} rows plus the resolved dev
// command for the Hostnames sheet, detected from the workspace repo's config
// files (never executed).
func (s *Server) portSuggestions(w http.ResponseWriter, r *http.Request) {
	suggestions, err := s.mgr.PortSuggestions(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	command, source, _ := s.mgr.ResolveDevCommand(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{
		"suggestions":      suggestions,
		"devCommand":       command,
		"devCommandSource": source,
	})
}

type createWorkspaceReq struct {
	Name     string `json:"name"`
	RepoPath string `json:"repoPath"`
	CWD      string `json:"cwd"`
	// StartupCommand is a pointer so lenses can OMIT it to get the daemon's
	// configured default; an explicit "" still means "no command, bare shell".
	StartupCommand *string `json:"startupCommand"`
	CreatedBy      string  `json:"createdBy"`
	// Group is the shared sidebar group ("window") the workspace starts in;
	// "" lets the first Mac window adopt it.
	Group string `json:"group"`
}

func (s *Server) createWorkspace(w http.ResponseWriter, r *http.Request) {
	var req createWorkspaceReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RepoPath == "" {
		writeError(w, http.StatusBadRequest, "repoPath required")
		return
	}
	startupCmd := s.mgr.StartupCommandFor(req.RepoPath)
	if req.StartupCommand != nil {
		startupCmd = *req.StartupCommand
	}
	ws, err := s.mgr.CreateWorkspace(req.Name, req.RepoPath, req.CWD, startupCmd, req.CreatedBy, req.Group)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ws)
}

func (s *Server) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.KillWorkspace(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type spawnPaneReq struct {
	CWD            string `json:"cwd"`
	StartupCommand string `json:"startupCommand"`
	CreatedBy      string `json:"createdBy"`
}

func (s *Server) spawnPane(w http.ResponseWriter, r *http.Request) {
	var req spawnPaneReq
	if !decodeJSON(w, r, &req) {
		return
	}
	p, err := s.mgr.SpawnPane(r.PathValue("id"), req.CWD, req.StartupCommand, req.CreatedBy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// killPane force-closes one pane — a hosted tab's ✕ in some lens. 204 whether
// or not the pane still existed (close is idempotent).
func (s *Server) killPane(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.KillPane(r.PathValue("id"), r.PathValue("paneId")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// archiveWorkspace is the lenses' "Close Session": the tmux session dies but
// the recipe stays cold and revivable — the non-destructive sibling of DELETE.
func (s *Server) archiveWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.mgr.ArchiveWorkspace(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

func (s *Server) reviveWorkspace(w http.ResponseWriter, r *http.Request) {
	ws, err := s.mgr.ReviveWorkspace(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ws)
}

// putGroup sets a workspace's shared sidebar group (the owning Mac window's
// name); the change is broadcast so every lens re-groups its list.
func (s *Server) putGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Group string `json:"group"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.mgr.SetGroup(r.PathValue("id"), req.Group); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type layoutReq struct {
	Blob        string `json:"blob"`
	BaseVersion int    `json:"baseVersion"`
}

// putLayout stores a workspace's opaque layout blob under optimistic concurrency.
// A stale baseVersion returns 409 with the current {version, blob} so the client
// can rebase; success returns {version} and broadcasts the change to other lenses.
func (s *Server) putLayout(w http.ResponseWriter, r *http.Request) {
	var req layoutReq
	if !decodeJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	newV, err := s.mgr.SetLayout(id, req.Blob, req.BaseVersion)
	switch {
	case errors.Is(err, manager.ErrLayoutConflict):
		cur := s.mgr.Workspace(id)
		blob := ""
		if cur != nil {
			blob = cur.LayoutJSON
		}
		writeJSON(w, http.StatusConflict, map[string]any{"version": newV, "blob": blob})
	case err != nil:
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"version": newV})
	}
}

// paneSnapshot returns a pane's current screen as escape-preserving bytes
// (base64 in "data"), the same seed an attach delivers — for a lens that wants a
// preview without opening an attach WebSocket. An optional ?history=N prepends N
// lines of scrollback. 404 if the pane is unknown; 409 if its workspace is cold
// (no live tmux to capture).
func (s *Server) paneSnapshot(w http.ResponseWriter, r *http.Request) {
	paneID := r.PathValue("id")
	wsID := s.mgr.WorkspaceForPane(paneID)
	if wsID == "" {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	ctrl := s.mgr.Controller(wsID)
	if ctrl == nil {
		writeError(w, http.StatusConflict, "workspace not live")
		return
	}
	history := 0
	if h := r.URL.Query().Get("history"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 {
			history = n
		}
	}
	b, err := ctrl.Capture(paneID, history)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"pane": paneID, "data": b64(b)})
}

// paneDriver reports the human currently driving a pane's workspace, for the git
// co-author trailer. 404 if the pane is unknown; 204 when nobody is driving (a
// solo/unattended session), so the hook simply adds no trailer.
func (s *Server) paneDriver(w http.ResponseWriter, r *http.Request) {
	wsID := s.mgr.WorkspaceForPane(r.PathValue("id"))
	if wsID == "" {
		writeError(w, http.StatusNotFound, "unknown pane")
		return
	}
	driver, ok := s.presence.Driver(wsID)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, driver)
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

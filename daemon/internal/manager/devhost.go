package manager

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/portdetect"
)

// Dev-hostname state: per-workspace {name, port} mappings plus the daemon-wide
// dev-serving settings. The devhost server reads this state to build its
// routing table and is poked through OnDevhostChange after every mutation; it
// stamps the runtime URL/Listening fields back via StampHostnameRuntime.

const (
	settingDevDomain        = "dev_domain"
	settingCloudflareToken  = "cloudflare_token"
	settingTailscaleAuthKey = "tailscale_authkey"
	settingLensHostname     = "lens_hostname"
)

// ErrUnknownWorkspace marks lookups of a workspace id the registry doesn't
// hold, so the API can answer 404 rather than 400.
var ErrUnknownWorkspace = errors.New("unknown workspace")

// hostnameLabel is one valid lowercase DNS label (RFC 1123, 63 chars max).
var hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// DevDomain returns the configured dev-serving domain (e.g. "dev.sanlabs.io");
// empty means ts.net fallback mode.
func (m *Manager) DevDomain() string { return m.getSetting(settingDevDomain) }

// SetDevDomain persists the dev domain ("" switches to ts.net fallback mode).
func (m *Manager) SetDevDomain(v string) error {
	return m.setDevSetting(settingDevDomain, strings.ToLower(strings.TrimSpace(v)))
}

// LensHostname returns the reserved label serving the ccmux web lens itself
// under the dev domain (e.g. "ccmux" → https://ccmux.dev.sanlabs.io); "" = off.
// Only meaningful in custom-domain mode — in ts.net fallback the lens is
// already at the daemon node's own name.
func (m *Manager) LensHostname() string { return m.getSetting(settingLensHostname) }

// ValidateLensHostname reports why v can't be the lens label ("" = it can).
// Split from the setter so the API can answer 400, not 500, on a bad value.
func (m *Manager) ValidateLensHostname(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	if !hostnameLabel.MatchString(v) {
		return fmt.Sprintf("invalid lens hostname %q: must be a DNS label (a-z, 0-9, inner hyphens)", v)
	}
	if _, taken := m.AllHostnames()[v]; taken {
		return fmt.Sprintf("hostname %q is already mapped by a workspace", v)
	}
	return ""
}

// SetLensHostname persists the lens label ("" clears it).
func (m *Manager) SetLensHostname(v string) error {
	v = strings.ToLower(strings.TrimSpace(v))
	if msg := m.ValidateLensHostname(v); msg != "" {
		return errors.New(msg)
	}
	return m.setDevSetting(settingLensHostname, v)
}

// CloudflareToken returns the DNS-edit API token for the dev domain's zone.
func (m *Manager) CloudflareToken() string { return m.getSetting(settingCloudflareToken) }

// SetCloudflareToken persists the token ("" clears it).
func (m *Manager) SetCloudflareToken(v string) error {
	return m.setDevSetting(settingCloudflareToken, strings.TrimSpace(v))
}

// TailscaleAuthKey returns the auth key used to register fallback-mode tsnet
// nodes silently; empty means first-run auth URLs land in the daemon log.
func (m *Manager) TailscaleAuthKey() string { return m.getSetting(settingTailscaleAuthKey) }

// SetTailscaleAuthKey persists the key ("" clears it).
func (m *Manager) SetTailscaleAuthKey(v string) error {
	return m.setDevSetting(settingTailscaleAuthKey, strings.TrimSpace(v))
}

func (m *Manager) getSetting(key string) string {
	v, _ := m.store.GetSetting(key)
	return v
}

func (m *Manager) setDevSetting(key, value string) error {
	if err := m.store.SetSetting(key, value); err != nil {
		return err
	}
	m.notifyDevhost()
	return nil
}

// notifyDevhost pokes the devhost server (async — reconciling may dial the
// Tailscale control plane or an ACME endpoint; mutations must not wait on it).
func (m *Manager) notifyDevhost() {
	if m.OnDevhostChange != nil {
		go m.OnDevhostChange()
	}
}

// SetHostnames replaces a workspace's dev-hostname mappings. Names are
// normalized to lowercase labels and validated; a name in use by another
// workspace is rejected — hostnames are one flat tailnet-wide namespace.
// Persisted, broadcast as workspace-status, and the devhost server is poked.
func (m *Manager) SetHostnames(wsID string, hs []model.Hostname) (*model.Workspace, error) {
	for i := range hs {
		hs[i].Name = strings.ToLower(strings.TrimSpace(hs[i].Name))
		hs[i].URL, hs[i].Listening = "", false
		if !hostnameLabel.MatchString(hs[i].Name) {
			return nil, fmt.Errorf("invalid hostname %q: must be a DNS label (a-z, 0-9, inner hyphens)", hs[i].Name)
		}
		if lens := m.LensHostname(); lens != "" && hs[i].Name == lens {
			return nil, fmt.Errorf("hostname %q is reserved for the ccmux web lens", lens)
		}
		if hs[i].Port < 1 || hs[i].Port > 65535 {
			return nil, fmt.Errorf("invalid port %d for %q", hs[i].Port, hs[i].Name)
		}
		for _, prev := range hs[:i] {
			if prev.Name == hs[i].Name {
				return nil, fmt.Errorf("hostname %q listed twice", hs[i].Name)
			}
		}
	}

	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	seen := map[string]string{} // name → owning workspace name, for the error
	for _, other := range m.byID {
		if other.ws.ID == wsID {
			continue
		}
		for _, h := range other.ws.Hostnames {
			seen[h.Name] = other.ws.Name
		}
	}
	for _, h := range hs {
		if owner, taken := seen[h.Name]; taken {
			m.mu.Unlock()
			return nil, fmt.Errorf("hostname %q already used by workspace %q", h.Name, owner)
		}
	}
	e.ws.Hostnames = hs
	ws := e.ws
	m.mu.Unlock()

	if err := m.store.SetWorkspaceHostnames(wsID, model.MarshalHostnames(hs)); err != nil {
		return nil, err
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	m.notifyDevhost()
	return ws, nil
}

// AllHostnames snapshots every mapping (name → port) across workspaces — the
// devhost server's routing-table source.
func (m *Manager) AllHostnames() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]int{}
	for _, e := range m.byID {
		for _, h := range e.ws.Hostnames {
			out[h.Name] = h.Port
		}
	}
	return out
}

// PortSuggestions proposes hostname mappings for a workspace by scanning its
// repo config (compose / package.json / Dockerfile — see internal/portdetect).
// Detected service labels merge with the repo slug ("api" in repo "admin" →
// "admin-api"; the main service is just "admin"), and anything colliding with
// an existing mapping's name or the workspace's already-mapped ports is
// dropped — the sheet only sees rows it could actually save.
func (m *Manager) PortSuggestions(wsID string) ([]portdetect.Suggestion, error) {
	ws := m.Workspace(wsID)
	if ws == nil {
		return nil, fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	usedName := map[string]bool{}
	for name := range m.AllHostnames() {
		usedName[name] = true
	}
	usedPort := map[int]bool{}
	for _, h := range ws.Hostnames {
		usedPort[h.Port] = true
	}

	slug := model.Slug(ws.RepoPath)
	out := []portdetect.Suggestion{}
	for _, s := range portdetect.Detect(ws.RepoPath) {
		if usedPort[s.Port] {
			continue
		}
		name := suggestionLabel(slug, s.Name)
		if usedName[name] {
			name = name + "-" + strconv.Itoa(s.Port)
		}
		if usedName[name] || !hostnameLabel.MatchString(name) {
			continue
		}
		usedName[name], usedPort[s.Port] = true, true
		out = append(out, portdetect.Suggestion{Name: name, Port: s.Port, Source: s.Source})
	}
	return out, nil
}

// suggestionLabel merges a detected service label into the repo slug: the
// repo's "main" service keeps the bare slug, secondary services suffix it.
// The empty check comes first — model.Slug's "repo" fallback for degenerate
// input must not leak into labels ("backend-repo").
func suggestionLabel(slug, service string) string {
	if service == "" {
		return slug
	}
	service = strings.Trim(model.Slug(service), "-")
	if service == "" || service == "web" || service == "app" || service == slug {
		return slug
	}
	return slug + "-" + service
}

// SetDevCommand persists the workspace's dev-server command override ("" =
// back to detection) and broadcasts so lenses refresh.
func (m *Manager) SetDevCommand(wsID, cmd string) error {
	m.mu.Lock()
	e := m.byID[wsID]
	if e == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	cmd = strings.TrimSpace(cmd)
	if e.ws.DevCommand == cmd {
		m.mu.Unlock()
		return nil
	}
	e.ws.DevCommand = cmd
	m.mu.Unlock()
	if err := m.store.SetWorkspaceDevCommand(wsID, cmd); err != nil {
		return err
	}
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return nil
}

// ResolveDevCommand returns what ▶ would run: the stored override, else
// detection from the repo's config files. source explains the choice for the
// sheet ("" when nothing was found).
func (m *Manager) ResolveDevCommand(wsID string) (command, source string, err error) {
	ws := m.Workspace(wsID)
	if ws == nil {
		return "", "", fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	if ws.DevCommand != "" {
		return ws.DevCommand, "workspace setting", nil
	}
	command, source = portdetect.DetectCommand(ws.RepoPath)
	return command, source, nil
}

// StartDevServer spawns the workspace's dev-server pane running the resolved
// command. The pane IS the log surface: stdout/colors/interactivity render in
// every lens, tmux keeps it across daemon restarts, and revive replays it.
func (m *Manager) StartDevServer(wsID string) (*model.Workspace, error) {
	if pane := m.devPane(wsID); pane != nil {
		return m.Workspace(wsID), nil // already running — idempotent
	}
	command, _, err := m.ResolveDevCommand(wsID)
	if err != nil {
		return nil, err
	}
	if command == "" {
		return nil, fmt.Errorf("no dev command: none stored and none detected in the repo")
	}
	pane, err := m.SpawnPane(wsID, "", command, "devhost")
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	pane.DevServer = true
	pane.Title = "dev ▸ " + command
	m.mu.Unlock()
	_ = m.store.SavePane(pane)
	m.events.publish(Event{Kind: "workspace-status", WorkspaceID: wsID})
	return m.Workspace(wsID), nil
}

// StopDevServer kills the dev-server pane (SIGTERM through tmux — compose and
// friends shut down cleanly). Idempotent: no pane, no error.
func (m *Manager) StopDevServer(wsID string) (*model.Workspace, error) {
	e := m.entry(wsID)
	if e == nil {
		return nil, fmt.Errorf("%w %s", ErrUnknownWorkspace, wsID)
	}
	pane := m.devPane(wsID)
	if pane == nil {
		return e.ws, nil
	}
	if err := m.KillPane(wsID, pane.ID); err != nil {
		return nil, err
	}
	return m.Workspace(wsID), nil
}

// devPane returns the workspace's dev-server pane, nil when none is running.
func (m *Manager) devPane(wsID string) *model.Pane {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[wsID]
	if e == nil {
		return nil
	}
	for _, p := range e.ws.Panes {
		if p.DevServer {
			return p
		}
	}
	return nil
}

// StampHostnameRuntime fills the runtime URL/Listening fields on every mapping
// (mirrors the git collector: runtime data lives on the model under m.mu, and a
// change broadcasts so lenses refetch). urlFor maps a bare name to its https
// URL under the active serving mode; listeningFor probes a local port.
func (m *Manager) StampHostnameRuntime(urlFor func(name string) string, listeningFor func(port int) bool) {
	changed := map[string]bool{}
	m.mu.Lock()
	for id, e := range m.byID {
		for i, h := range e.ws.Hostnames {
			url, listening := urlFor(h.Name), listeningFor(h.Port)
			if h.URL != url || h.Listening != listening {
				e.ws.Hostnames[i].URL, e.ws.Hostnames[i].Listening = url, listening
				changed[id] = true
			}
		}
	}
	m.mu.Unlock()
	for id := range changed {
		m.events.publish(Event{Kind: "workspace-status", WorkspaceID: id})
	}
}

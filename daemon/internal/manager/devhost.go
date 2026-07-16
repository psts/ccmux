package manager

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
)

// Dev-hostname state: per-workspace {name, port} mappings plus the daemon-wide
// dev-serving settings. The devhost server reads this state to build its
// routing table and is poked through OnDevhostChange after every mutation; it
// stamps the runtime URL/Listening fields back via StampHostnameRuntime.

const (
	settingDevDomain        = "dev_domain"
	settingCloudflareToken  = "cloudflare_token"
	settingTailscaleAuthKey = "tailscale_authkey"
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

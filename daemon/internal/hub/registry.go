// Package hub holds the ccmuxd hub-role machinery: the registry of federated
// member hosts (discovered over the tailnet and health-probed), and — built on
// top of it — the workspace/event aggregation and reverse-proxy the lens talks
// to. See daemon/docs/multihost-plan.md.
//
// The registry is pure: discovery and probing are injected, so its tests never
// dial the Tailscale control plane or a real host (mirrors internal/tailnet and
// internal/devhost).
package hub

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/version"
)

// Compat classifies how the hub treats a member host given its wire contract.
const (
	CompatOK          = "ok"          // same contract → full surface proxied
	CompatDegraded    = "degraded"    // within the floor → list-only + attach
	CompatUnsupported = "unsupported" // beyond the floor → listed, not proxied
	CompatUnreachable = "unreachable" // health probe failed
)

// DefaultFloor is how many contract versions apart a host may be from the hub
// and still be served degraded rather than unsupported (plan: "exactly one").
const DefaultFloor = 1

// Node is a tailnet peer discovered as a ccmux host: its MagicDNS label (the
// stable id that lands on Workspace.Host) and its dialable authority.
type Node struct {
	ID   string // MagicDNS label, e.g. "hostb"
	Addr string // dialable authority, e.g. "hostb.tailb9053d.ts.net"
}

// Health is what a host's GET /v1/health reports for the handshake.
type Health struct {
	Version  string `json:"version"`
	Contract int    `json:"contract"`
}

// Host is one member node as the hub and lenses see it. Addr is hub-internal
// (probe + proxy + the lens's direct-attach target); the rest is lens-facing.
type Host struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	Healthy  bool   `json:"healthy"`
	Self     bool   `json:"self,omitempty"` // the hub node itself (also a host)
	Version  string `json:"version,omitempty"`
	Contract int    `json:"contract,omitempty"`
	Compat   string `json:"compat"`
	Reason   string `json:"reason,omitempty"`
	LastSeen int64  `json:"lastSeen"`
}

// Registry tracks the federation's member hosts. Refresh rediscovers and
// re-probes; List/Get read the last snapshot. Safe for concurrent use.
type Registry struct {
	selfID   string
	floor    int
	discover func() ([]Node, error)
	probe    func(baseURL string) (Health, error)
	now      func() int64

	mu    sync.RWMutex
	hosts map[string]Host
}

// NewRegistry builds a registry. selfID is the hub node's own MagicDNS label, so
// its own entry is marked Self. discover enumerates tag:ccmux tailnet peers;
// probe fetches a host's /v1/health; now supplies the LastSeen clock. floor ≤ 0
// falls back to DefaultFloor.
func NewRegistry(selfID string, floor int, discover func() ([]Node, error), probe func(baseURL string) (Health, error), now func() int64) *Registry {
	if floor <= 0 {
		floor = DefaultFloor
	}
	return &Registry{
		selfID: selfID, floor: floor, discover: discover, probe: probe, now: now,
		hosts: map[string]Host{},
	}
}

// Refresh rediscovers the member set and probes each host's health, rebuilding
// the snapshot. A discovery error leaves the previous snapshot intact (a flaky
// control-plane read shouldn't blank the whole federation).
func (r *Registry) Refresh() {
	nodes, err := r.discover()
	if err != nil {
		return
	}
	next := make(map[string]Host, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n Node) {
			defer wg.Done()
			h := r.probeOne(n)
			mu.Lock()
			next[h.ID] = h
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	r.mu.Lock()
	r.hosts = next
	r.mu.Unlock()
}

// probeOne health-probes one node and classifies it.
func (r *Registry) probeOne(n Node) Host {
	h := Host{ID: n.ID, Addr: n.Addr, Self: n.ID == r.selfID, LastSeen: r.now()}
	hp, err := r.probe("https://" + n.Addr)
	if err != nil {
		h.Healthy = false
		h.Compat = CompatUnreachable
		h.Reason = err.Error()
		return h
	}
	h.Healthy = true
	h.Version = hp.Version
	h.Contract = hp.Contract
	h.Compat, h.Reason = classify(hp.Contract, version.Contract, r.floor)
	return h
}

// classify decides the hub's treatment of a host from its contract vs the hub's.
func classify(hostContract, hubContract, floor int) (compat, reason string) {
	switch {
	case hostContract == hubContract:
		return CompatOK, ""
	case abs(hostContract-hubContract) <= floor:
		if hostContract < hubContract {
			return CompatDegraded, fmt.Sprintf("host contract %d is behind the hub's %d — upgrade the host", hostContract, hubContract)
		}
		return CompatDegraded, fmt.Sprintf("host contract %d is ahead of the hub's %d — upgrade the hub", hostContract, hubContract)
	default:
		return CompatUnsupported, fmt.Sprintf("host contract %d vs the hub's %d exceeds the supported range (±%d)", hostContract, hubContract, floor)
	}
}

// Serves reports whether the hub proxies the full surface to a host. Degraded
// and unsupported hosts still list and (for degraded) attach, but the hub
// refuses to proxy their mutating/feature endpoints.
func (h Host) Serves() bool { return h.Healthy && h.Compat == CompatOK }

// List returns the member hosts, self first then alphabetical, for GET /v1/hosts.
func (r *Registry) List() []Host {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Host, 0, len(r.hosts))
	for _, h := range r.hosts {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// StartProbe seeds the registry immediately, then re-discovers + re-probes on
// an interval for the lifetime of ctx (mirrors devhost.StartProbe).
func (r *Registry) StartProbe(ctx context.Context, interval time.Duration) {
	r.Refresh()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Refresh()
			}
		}
	}()
}

// Get returns a host by id.
func (r *Registry) Get(id string) (Host, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.hosts[id]
	return h, ok
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

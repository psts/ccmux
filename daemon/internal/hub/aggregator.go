package hub

import (
	"context"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/model"
)

// LocalLister is the hub's own workspace source (its local manager).
type LocalLister interface {
	List() []*model.Workspace
}

// RemoteFetcher fetches a member host's workspaces over the tailnet.
type RemoteFetcher func(ctx context.Context, host Host) ([]*model.Workspace, error)

// Aggregator merges the hub's local workspaces with every listing remote host's,
// stamping Host on each, and maintains the workspaceID/paneID → hostID routing
// index the reverse proxy uses. Local workspaces are always included regardless
// of registry state, so the hub never hides its own sessions.
type Aggregator struct {
	selfID string
	reg    *Registry
	local  LocalLister
	fetch  RemoteFetcher

	mu        sync.RWMutex
	owner     map[string]string      // workspace id + pane id → owning host
	groups    map[string]string      // pane id → owning workspace's group (for peers federation)
	hostnames map[string]hostnameLoc // dev-hostname label → owning host + workspace (global registrar)
}

// hostnameLoc is which host + workspace currently owns a dev-hostname label.
type hostnameLoc struct {
	Host      string
	Workspace string
}

// NewAggregator builds an aggregator. selfID is the hub node's MagicDNS label.
func NewAggregator(selfID string, reg *Registry, local LocalLister, fetch RemoteFetcher) *Aggregator {
	return &Aggregator{
		selfID: selfID, reg: reg, local: local, fetch: fetch,
		owner:     map[string]string{},
		groups:    map[string]string{},
		hostnames: map[string]hostnameLoc{},
	}
}

// StartRefresh re-aggregates on an interval so the ownership + pane→group indexes
// stay fresh for the reverse proxy and the peers group resolver (which read the
// cached maps — never triggering I/O under the bus lock). For the lifetime of ctx.
func (a *Aggregator) StartRefresh(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.Aggregate(ctx)
			}
		}
	}()
}

// Aggregate returns the merged, host-stamped workspace list and refreshes the
// ownership index. Remote hosts are fetched concurrently; a fetch error drops
// that host's workspaces for this cycle (it stays visible in GET /v1/hosts).
func (a *Aggregator) Aggregate(ctx context.Context) []*model.Workspace {
	all := stampAll(a.local.List(), a.selfID) // local always included
	owner := map[string]string{}
	groups := map[string]string{}
	hostnames := map[string]hostnameLoc{}
	indexInto(owner, groups, hostnames, all, a.selfID)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range a.reg.List() {
		if h.Self || !h.Lists() {
			continue
		}
		wg.Add(1)
		go func(h Host) {
			defer wg.Done()
			wss, err := a.fetch(ctx, h)
			if err != nil {
				return
			}
			stamped := stampAll(wss, h.ID)
			mu.Lock()
			all = append(all, stamped...)
			indexInto(owner, groups, hostnames, stamped, h.ID)
			mu.Unlock()
		}(h)
	}
	wg.Wait()

	a.mu.Lock()
	a.owner = owner
	a.groups = groups
	a.hostnames = hostnames
	a.mu.Unlock()
	// Members land in completion order, which differs on every poll; unsorted,
	// the merged list reshuffles every lens's sidebar every few seconds.
	model.SortByName(all)
	return all
}

// HostnameOwner returns which host + workspace currently claims a dev-hostname
// label, from the last aggregation — the hub's global registrar for enforcing
// uniqueness across hosts. ok is false for an unclaimed label.
func (a *Aggregator) HostnameOwner(name string) (host, workspace string, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	loc, ok := a.hostnames[name]
	return loc.Host, loc.Workspace, ok
}

// GroupForPane returns the window group of a pane's owning workspace, from the
// last aggregation — the hub's global group resolver for peers federation. ok is
// false for an unknown pane (or before the first aggregation).
func (a *Aggregator) GroupForPane(paneID string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	g, ok := a.groups[paneID]
	return g, ok
}

// Owner returns the host that owns a workspace or pane id, from the last
// Aggregate. ok is false for an unknown id (or before the first Aggregate).
func (a *Aggregator) Owner(id string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	h, ok := a.owner[id]
	return h, ok
}

// OwnerOrRefresh looks up an id's owning host, re-aggregating once on a miss so a
// just-created workspace routes without waiting for the next periodic cycle.
func (a *Aggregator) OwnerOrRefresh(ctx context.Context, id string) (string, bool) {
	if h, ok := a.Owner(id); ok {
		return h, true
	}
	a.Aggregate(ctx)
	return a.Owner(id)
}

// indexInto records each workspace's and pane's owning host, each pane's
// owning-workspace group (peers resolver), and each dev-hostname's owner (registrar).
func indexInto(owner, groups map[string]string, hostnames map[string]hostnameLoc, wss []*model.Workspace, hostID string) {
	for _, ws := range wss {
		owner[ws.ID] = hostID
		for _, hn := range ws.Hostnames {
			if hn.Name != "" {
				hostnames[hn.Name] = hostnameLoc{Host: hostID, Workspace: ws.ID}
			}
		}
		for _, p := range ws.Panes {
			owner[p.ID] = hostID
			groups[p.ID] = ws.Group
		}
	}
}

// stampAll returns host-stamped copies of every workspace.
func stampAll(wss []*model.Workspace, host string) []*model.Workspace {
	out := make([]*model.Workspace, len(wss))
	for i, ws := range wss {
		out[i] = stampHost(ws, host)
	}
	return out
}

// stampHost returns a shallow copy of ws (and its panes) with Host set — never
// mutating the manager's shared, live workspace/pane pointers.
func stampHost(ws *model.Workspace, host string) *model.Workspace {
	cp := *ws
	cp.Host = host
	cp.Panes = make([]*model.Pane, len(ws.Panes))
	for i, p := range ws.Panes {
		pc := *p
		pc.Host = host
		cp.Panes[i] = &pc
	}
	return &cp
}

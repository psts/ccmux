package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
)

// presenceOwners serves GET /v1/presence: the identity logins with a focused lens
// on THIS daemon right now (presenceHub.ActiveOwners). The hub polls each member's
// endpoint and unions the results, so unified push suppression stays correct when
// a user watches a remote-host session directly (its focus is known only to that
// host). Tailnet-gated like the rest of the API; returns only logins.
func (s *Server) presenceOwners(w http.ResponseWriter, _ *http.Request) {
	owners := make([]string, 0)
	if s.presence != nil {
		for login := range s.presence.ActiveOwners() {
			owners = append(owners, login)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"owners": owners})
}

// federatedFocus is the hub's push focusOracle: the union of the hub's own active
// owners with a periodically-polled snapshot of every member host's, so a user at
// a screen anywhere in the federation suppresses their phone pushes.
type federatedFocus struct {
	local  focusOracle
	hub    *hubMode
	client *http.Client

	mu     sync.RWMutex
	remote map[string]bool
}

// newFederatedFocus builds the union oracle and starts polling members.
func (s *Server) newFederatedFocus(ctx context.Context) *federatedFocus {
	f := &federatedFocus{
		local:  s.presence,
		hub:    s.hub,
		client: &http.Client{Transport: s.hub.client.Transport(), Timeout: 3 * time.Second},
		remote: map[string]bool{},
	}
	go f.refreshLoop(ctx, 3*time.Second)
	return f
}

// ActiveOwners unions the hub's own focused owners with the last member poll,
// into a fresh map (never mutating the local oracle's return).
func (f *federatedFocus) ActiveOwners() map[string]bool {
	out := map[string]bool{}
	for login := range f.local.ActiveOwners() {
		out[login] = true
	}
	f.mu.RLock()
	for login := range f.remote {
		out[login] = true
	}
	f.mu.RUnlock()
	return out
}

func (f *federatedFocus) refreshLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		f.poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll fetches every healthy remote member's active owners into a fresh union.
func (f *federatedFocus) poll(ctx context.Context) {
	union := map[string]bool{}
	for _, h := range f.hub.reg.List() {
		if h.Self || !h.Healthy {
			continue
		}
		for _, login := range f.fetchOwners(ctx, "https://"+h.Addr) {
			union[login] = true
		}
	}
	f.mu.Lock()
	f.remote = union
	f.mu.Unlock()
}

func (f *federatedFocus) fetchOwners(ctx context.Context, baseURL string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/presence", nil)
	if err != nil {
		return nil
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Owners []string `json:"owners"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out.Owners
}

// mergedNotifierEvents is the hub notifier's event stream: the local manager
// firehose plus every member host's attention (their /v1/events attention frames
// converted to manager.Event), so the hub pushes for attention on ANY host. A
// member host's own notifier stays inert — subscriptions live at the hub — so
// there's no double-push.
func (s *Server) mergedNotifierEvents(ctx context.Context) <-chan manager.Event {
	out := make(chan manager.Event, 256)

	id, ch := s.mgr.SubscribeEvents()
	go func() {
		defer s.mgr.UnsubscribeEvents(id)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	up := &eventUpstreams{hub: s.hub, frames: make(chan []byte, 128), connected: map[string]bool{}}
	up.dialAll(ctx) // ignore the hello snapshots — the notifier only pushes on transitions
	go up.reconnectLoop(ctx, 10*time.Second)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-up.frames:
				if ev, ok := attentionEventFromFrame(raw); ok {
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out
}

// attentionEventFromFrame converts a remote firehose attention frame into the
// manager.Event the notifier consumes; ok is false for non-attention frames.
func attentionEventFromFrame(raw []byte) (manager.Event, bool) {
	var m firehoseMsg
	if json.Unmarshal(raw, &m) != nil || m.T != "attention" {
		return manager.Event{}, false
	}
	return manager.Event{Kind: "attention", WorkspaceID: m.Workspace, PaneID: m.Pane, Attention: m.State}, true
}

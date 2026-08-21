package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/manager"
)

// presenceDrivers serves GET /v1/presence/drivers: this host's per-workspace
// driver map (who typed last, and when), for the hub's notification routing.
// The response is an object so it can grow; the owners list at /v1/presence is
// a bare array and cannot.
func (s *Server) presenceDrivers(w http.ResponseWriter, _ *http.Request) {
	drivers := map[string]DriverStamp{}
	if s.presence != nil {
		drivers = s.presence.AllDriverLogins()
	}
	writeJSON(w, http.StatusOK, map[string]any{"drivers": drivers})
}

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

// presenceStaleAfter bounds how long a member's last-known watchers survive
// failed polls. Short outages (a tailnet blip, a restart) keep their answer so
// alerts and suppression stay steady; a member that stays silent past this is a
// machine that hibernated or died, and the person its answer named is NOT at
// that screen anymore. One minute: long enough to ride out a blip, short enough
// that a closed laptop hands notifications to the phone promptly.
const presenceStaleAfter = 60 * time.Second

// memberOwners is one member host's last successful presence answer and when it
// was given, so a failing host's retained answer can expire (see poll).
type memberOwners struct {
	owners map[string]bool
	// drivers is the member's per-workspace driver map (who typed last, and
	// when) — attaches go direct to the owning host, so only that host knows.
	// Empty from members too old to serve /v1/presence/drivers.
	drivers map[string]DriverStamp
	asOf    time.Time
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
	// byHost is the same data before the union, so one member failing to answer
	// costs only its own entry rather than every member's (see poll).
	byHost map[string]memberOwners
	// failing latches which hosts have already reported a poll failure, so the
	// 3s loop logs the fault once and the recovery once.
	failing map[string]bool
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

// poll refreshes every healthy remote member's active owners, keyed by host so a
// member that fails to answer keeps its LAST KNOWN owners — for a while.
//
// Both directions of that trade are load-bearing. When only push read this, a
// failed poll was fail-safe: it under-counted watchers and sent the phone a
// notification nobody needed. The alert flag reads it too now, and there the
// same miss inverts — no owners means "nobody is at a screen", so the lens is
// told not to raise a notification at all. A 3s tailnet hiccup would silently
// reproduce the exact bug this path exists to fix, so a brief failure keeps the
// last answer. But keeping it FOREVER is the opposite bug: a Mac that
// hibernates stops answering with "its person is here" as its last word, and an
// immortal retention of that answer suppresses their phone pushes exactly when
// the phone is the only screen left. retainOrExpire draws the line.
func (f *federatedFocus) poll(ctx context.Context) {
	f.mu.RLock()
	previous := f.byHost
	f.mu.RUnlock()

	now := time.Now()
	fresh := map[string]memberOwners{}
	for _, h := range f.hub.reg.List() {
		if h.Self {
			continue
		}
		if !h.Healthy {
			// A host the registry has already failed gets the SAME grace as one
			// that failed only our fetch. The health probe flips on a single
			// missed /v1/health on a 5s cycle, so without this branch a short
			// blip skipped retainOrExpire entirely and dropped the host's
			// watchers instantly — while the log below promised a minute.
			retainOrExpire(fresh, previous, h.ID, now)
			f.noteFailure(h.ID, fmt.Errorf("registry reports it unhealthy"))
			continue
		}
		owners, err := f.fetchOwners(ctx, "https://"+h.Addr)
		if err != nil {
			retainOrExpire(fresh, previous, h.ID, now)
			f.noteFailure(h.ID, err)
			continue
		}
		f.noteRecovery(h.ID)
		set := map[string]bool{}
		for _, login := range owners {
			set[login] = true
		}
		// A failed drivers fetch is a real fault (fetchDrivers already treats
		// a 404 from an old member as empty-and-fine): logged latched like the
		// owners fetch, and the last-known map is retained within the same
		// staleness bound — dropping it instantly would treat the driver tier
		// as lapsed on every blip, and if the driver does not also hold the
		// window open, their own needs-input would route away from them.
		drivers, derr := f.fetchDrivers(ctx, "https://"+h.Addr)
		if derr != nil {
			f.noteFailure(h.ID+" (drivers)", derr)
			drivers = lastDrivers(previous, h.ID, now)
		} else {
			f.noteRecovery(h.ID + " (drivers)")
		}
		fresh[h.ID] = memberOwners{owners: set, drivers: drivers, asOf: now}
	}

	union := map[string]bool{}
	for _, mo := range fresh {
		for login := range mo.owners {
			union[login] = true
		}
	}
	f.mu.Lock()
	f.byHost, f.remote = fresh, union
	f.mu.Unlock()
}

// lastDrivers carries a member's last-known driver map through a failed
// drivers fetch, within the same staleness bound retainOrExpire applies to
// owners. Past it (or with no previous answer): empty, and routing widens to
// the window holders.
func lastDrivers(previous map[string]memberOwners, hostID string, now time.Time) map[string]DriverStamp {
	last, ok := previous[hostID]
	if !ok || now.Sub(last.asOf) > presenceStaleAfter || last.drivers == nil {
		return map[string]DriverStamp{}
	}
	return last.drivers
}

// retainOrExpire carries a failing member's last successful answer forward,
// but only within presenceStaleAfter of when it was given. Past that the entry
// is dropped — absent wins — and the drop is logged; it logs exactly once per
// outage because the next poll finds no previous entry to expire.
func retainOrExpire(fresh, previous map[string]memberOwners, hostID string, now time.Time) {
	last, ok := previous[hostID]
	if !ok {
		return
	}
	if now.Sub(last.asOf) <= presenceStaleAfter {
		fresh[hostID] = last
		return
	}
	log.Printf("hub focus: %s has not answered a presence poll in %s — treating its watchers as absent",
		hostID, presenceStaleAfter)
}

// noteFailure logs a member's first failed presence poll and stays quiet until
// it recovers — the loop runs every 3s, and an unlatched line would bury the log
// while telling a reader nothing the first one did not.
func (f *federatedFocus) noteFailure(host string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing == nil {
		f.failing = map[string]bool{}
	}
	if !f.failing[host] {
		log.Printf("hub focus: %s presence poll failed (%v) — keeping its last known watchers for up to %s", host, err, presenceStaleAfter)
	}
	f.failing[host] = true
}

func (f *federatedFocus) noteRecovery(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing[host] {
		log.Printf("hub focus: %s presence poll recovered", host)
		delete(f.failing, host)
	}
}

// fetchOwners reports a member's focused logins, or an error. The error is the
// point: an empty list and an unreachable host mean opposite things to the alert
// flag, and returning nil for both is what let a blip read as "nobody is here".
// DriverLogin answers who is driving a workspace, hub-wide: the hub's own
// presence first (workspaces attached through it), then the polled member
// maps — a workspace lives on exactly one host, so the first hit is the one.
func (f *federatedFocus) DriverLogin(wsID string) (string, int64, bool) {
	if login, at, ok := f.local.DriverLogin(wsID); ok {
		return login, at, ok
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, mo := range f.byHost {
		if d, ok := mo.drivers[wsID]; ok {
			return d.Login, d.AtMillis, true
		}
	}
	return "", 0, false
}

// fetchDrivers reads one member's per-workspace driver map. A 404 is an old
// member with nothing to say, not an error.
func (f *federatedFocus) fetchDrivers(ctx context.Context, baseURL string) (map[string]DriverStamp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/presence/drivers", nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return map[string]DriverStamp{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presence drivers HTTP %d", resp.StatusCode)
	}
	var out struct {
		Drivers map[string]DriverStamp `json:"drivers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Drivers == nil {
		out.Drivers = map[string]DriverStamp{}
	}
	return out.Drivers, nil
}

func (f *federatedFocus) fetchOwners(ctx context.Context, baseURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/presence", nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("presence HTTP %d", resp.StatusCode)
	}
	var out struct {
		Owners []string `json:"owners"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Owners, nil
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

	up := &eventUpstreams{hub: s.hub, frames: make(chan []byte, 128), ka: s.ka, connected: map[string]bool{}}
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

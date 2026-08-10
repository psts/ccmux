// Forwarding this host's driver-mode pane→window map to the hub.
//
// The Mac app pushes that map to THIS daemon, the only ccmuxd it talks to. Once
// the sessions in those panes register on the hub, the map has to follow them or
// the hub falls back to naming their group after the directory, and they land in
// a project nobody is looking at.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
)

// localGroupsForwarder serializes the onward push. One worker, one pending slot,
// latest-wins.
//
// Serialized because the map is a FULL REPLACEMENT of this host's slice: two
// pushes in flight at once can arrive at the hub out of order, leaving it acting
// on a map the app has already superseded until the next re-push a minute later.
// Latest-wins because a superseded map has no value — the newest one describes
// the windows as they are now, and delivering the older one first would be work
// done to be immediately undone.
type localGroupsForwarder struct {
	client  *http.Client
	hubURL  func() string
	cred    *hubHostCredential
	pending chan map[string]string

	// lastErr latches the most recent failure MESSAGE so a hub that stays
	// unreachable logs once rather than once a minute forever, and so the
	// recovery is reported when it comes.
	lastErr string
}

func newLocalGroupsForwarder(client *http.Client, hubURL func() string, cred *hubHostCredential) *localGroupsForwarder {
	f := &localGroupsForwarder{
		client:  client,
		hubURL:  hubURL,
		cred:    cred,
		pending: make(chan map[string]string, 1),
	}
	go f.run()
	return f
}

// Submit queues a map, replacing any still-unsent one. Never blocks: the caller
// is an HTTP handler, and the app re-pushes unconditionally on a timer, so a
// dropped map self-heals within about a minute.
func (f *localGroupsForwarder) Submit(groups map[string]string) {
	for {
		select {
		case f.pending <- groups:
			return
		default:
		}
		select {
		case <-f.pending: // discard the superseded map and retry
		default:
		}
	}
}

func (f *localGroupsForwarder) run() {
	for groups := range f.pending {
		f.pushOnce(groups)
	}
}

func (f *localGroupsForwarder) pushOnce(groups map[string]string) {
	u := f.hubURL()
	if u == "" {
		return
	}
	err := f.forward(u, groups)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	switch {
	case err != nil && msg != f.lastErr:
		log.Printf("peers: local-pane groups not forwarded to the hub (%v) — driver-mode sessions there will group by directory", err)
	case err == nil && f.lastErr != "":
		// Silence means both "still broken" and "fixed itself ten minutes ago".
		// Saying so is what stops someone chasing a fault that already cleared.
		log.Printf("peers: local-pane groups forwarding to the hub recovered")
	}
	f.lastErr = msg
}

// forward pushes the map under the hub's own shared credential. The hub keys
// what arrives by the member it came from, so this replaces THIS host's slice
// and touches no other member's.
func (f *localGroupsForwarder) forward(hubURL string, groups map[string]string) error {
	token, err := f.cred.fetch()
	if err != nil {
		return fmt.Errorf("hub host credential: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"groups": groups})
	req, err := http.NewRequest(http.MethodPut, hubURL+"/v1/peers/local-groups", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local-groups HTTP %d", resp.StatusCode)
	}
	return nil
}

// hubURLReader adapts the discovery pointer to the func the forwarder wants.
func hubURLReader(p *atomic.Pointer[string]) func() string {
	return func() string { return *p.Load() }
}

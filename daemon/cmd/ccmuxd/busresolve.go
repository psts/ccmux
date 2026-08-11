// Answering a session's "which peers bus should I join, and with what token?"
// on a member host. See daemon/docs/multihost-plan.md ("Hosts hold no secret").
package main

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// busResolve holds what answering that question needs. Split out of
// enableHostFederation so the wiring there stays wiring: the decision has four
// outcomes across two independent axes (pane vs pane-less, relay vs no relay)
// and does not read as a side note to a constructor call.
type busResolve struct {
	client        *http.Client
	hubURL        *atomic.Pointer[string]
	cred          *hubHostCredential
	relayURL      string
	localPaneless string
}

// Resolve reports the bus URL and the credential to present, or ("", "", nil)
// meaning "no hub — stay on the daemon you asked".
//
// An ERROR is never the same as an empty answer. The caller reads empty as "this
// daemon is the bus" and would pull a session off a hub that is merely
// restarting, so every failure to obtain a credential is reported as one.
func (b *busResolve) Resolve(paneID string) (string, string, error) {
	hub := *b.hubURL.Load()
	if hub == "" {
		return "", "", nil
	}
	if paneID == "" {
		// A pane-less session has no hub credential of its own, so without a
		// relay to swap one in for it, it stays where it is.
		if b.relayURL == "" {
			return "", "", nil
		}
		return b.panelessViaRelay()
	}
	token, err := mintHubPaneToken(b.client, hub, paneID)
	if err != nil {
		return "", "", fmt.Errorf("hub mint for pane %s: %w", paneID, err)
	}
	// No relay to offer means the pane dials the hub itself, which works only
	// where the pane process shares this daemon's tailnet identity.
	if b.relayURL == "" {
		return hub, token, nil
	}
	return b.relayURL, token, nil
}

// panelessViaRelay keeps a pane-less session on THIS host's shared token and
// lets the relay swap it for the hub's, so nothing hub-minted is handed to a
// local process and the session needs no pane identity to join.
func (b *busResolve) panelessViaRelay() (string, string, error) {
	if b.localPaneless == "" {
		return "", "", nil
	}
	if _, err := b.cred.fetch(); err != nil {
		return "", "", fmt.Errorf("hub host credential: %w", err)
	}
	return b.relayURL, b.localPaneless, nil
}

// upstreamTranslator maps the bearer a local caller presented to the one the hub
// should see. Only the pane-less credential is translated: a pane already holds
// a hub-minted token, and anything else is left alone for the hub to reject — a
// relay that invented credentials for unknown callers would be the open door
// this whole path exists to avoid.
func upstreamTranslator(localPaneless string, cred *hubHostCredential) func(string) (string, error) {
	return func(inbound string) (string, error) {
		if localPaneless == "" || inbound != localPaneless {
			return "", nil
		}
		return cred.fetch()
	}
}

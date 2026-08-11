package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func hubPtr(url string) *atomic.Pointer[string] {
	p := &atomic.Pointer[string]{}
	p.Store(&url)
	return p
}

// mintingHub answers the pane-token mint and the host-token fetch, or fails both.
func mintingHub(t *testing.T, ok bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"token":"hub-minted"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resolver(t *testing.T, hubURL, relayURL, paneless string, hubOK bool) *busResolve {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	if hubURL == "" {
		return &busResolve{client: client, hubURL: hubPtr(""), cred: &hubHostCredential{client: client, hubURL: hubPtr("")},
			relayURL: relayURL, localPaneless: paneless}
	}
	srv := mintingHub(t, hubOK)
	p := hubPtr(srv.URL)
	return &busResolve{client: client, hubURL: p, cred: &hubHostCredential{client: client, hubURL: p},
		relayURL: relayURL, localPaneless: paneless}
}

// The four outcomes across two axes, plus the rule the whole type exists for:
// an ERROR is never an empty answer. A caller reads empty as "this daemon is the
// bus" and would pull a live session off a hub that is merely restarting.
func TestBusResolve_Outcomes(t *testing.T) {
	t.Run("no hub discovered", func(t *testing.T) {
		url, tok, err := resolver(t, "", "http://127.0.0.1:7900/v1/hubbus", "local-tok", true).Resolve("pane-1")
		if url != "" || tok != "" || err != nil {
			t.Errorf("got (%q,%q,%v), want the stay-put answer", url, tok, err)
		}
	})

	t.Run("relay + pane mints a hub token", func(t *testing.T) {
		url, tok, err := resolver(t, "hub", "relay", "local-tok", true).Resolve("pane-1")
		if err != nil || url != "relay" || tok != "hub-minted" {
			t.Errorf("got (%q,%q,%v), want relay + a hub-minted pane token", url, tok, err)
		}
	})

	t.Run("relay + pane-less keeps THIS host's token", func(t *testing.T) {
		url, tok, err := resolver(t, "hub", "relay", "local-tok", true).Resolve("")
		if err != nil || url != "relay" || tok != "local-tok" {
			t.Errorf("got (%q,%q,%v) — the relay swaps it, so nothing hub-minted reaches a local process", url, tok, err)
		}
	})

	t.Run("no relay + pane dials the hub directly", func(t *testing.T) {
		r := resolver(t, "hub", "", "local-tok", true)
		url, tok, err := r.Resolve("pane-1")
		if err != nil || url != *r.hubURL.Load() || tok != "hub-minted" {
			t.Errorf("got (%q,%q,%v), want the hub URL itself", url, tok, err)
		}
	})

	t.Run("no relay + pane-less stays put", func(t *testing.T) {
		url, tok, err := resolver(t, "hub", "", "local-tok", true).Resolve("")
		if url != "" || tok != "" || err != nil {
			t.Errorf("got (%q,%q,%v) — a pane-less session has no hub credential of its own", url, tok, err)
		}
	})

	t.Run("no pane-less token configured", func(t *testing.T) {
		url, _, err := resolver(t, "hub", "relay", "", true).Resolve("")
		if url != "" || err != nil {
			t.Errorf("got (%q,%v), want the stay-put answer", url, err)
		}
	})
}

// Both failure paths must report, never answer empty.
func TestBusResolve_FailureIsNeverAnEmptyAnswer(t *testing.T) {
	t.Run("pane mint failure", func(t *testing.T) {
		url, tok, err := resolver(t, "hub", "relay", "local-tok", false).Resolve("pane-1")
		if err == nil {
			t.Fatalf("got (%q,%q,nil) — an unreachable hub must not read as 'no hub'", url, tok)
		}
	})

	t.Run("pane-less credential failure", func(t *testing.T) {
		url, tok, err := resolver(t, "hub", "relay", "local-tok", false).Resolve("")
		if err == nil {
			t.Fatalf("got (%q,%q,nil) — a failed host credential must not read as 'no hub'", url, tok)
		}
	})
}

// The translator is the relay's one credential-substitution rule. Tested here
// because the relay's own tests build a stand-in, so this could regress to
// handing the hub credential to any local caller with the suite still green.
func TestUpstreamTranslator_OnlySwapsThisHostsPanelessToken(t *testing.T) {
	srv := mintingHub(t, true)
	cred := &hubHostCredential{client: &http.Client{Timeout: 2 * time.Second}, hubURL: hubPtr(srv.URL)}
	translate := upstreamTranslator("local-tok", cred)

	if got, err := translate("local-tok"); err != nil || got != "hub-minted" {
		t.Errorf("matching token → (%q,%v), want the hub credential", got, err)
	}
	for _, inbound := range []string{"", "some-pane-token", "guessed"} {
		if got, err := translate(inbound); err != nil || got != "" {
			t.Errorf("%q → (%q,%v), want no substitution", inbound, got, err)
		}
	}
	// With no local credential configured there is nothing to match, so nothing
	// may be swapped — least of all for an empty bearer.
	none := upstreamTranslator("", cred)
	if got, err := none(""); err != nil || got != "" {
		t.Errorf("unconfigured → (%q,%v), want no substitution", got, err)
	}
}

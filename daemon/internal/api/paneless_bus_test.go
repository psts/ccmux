package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
)

// busAskPaneless posts a bus-resolution request with NO pane id — a Claude
// started in a plain terminal, authenticating with the daemon-info file's
// shared credential.
func busAskPaneless(t *testing.T, s *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/peers/bus", strings.NewReader(`{"pane_id":""}`))
	req.RemoteAddr = "127.0.0.1:5000"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.peersBus(rec, req)
	return rec
}

// A pane-less session gets a real answer. Refusing it for having no pane is what
// left every plain-terminal Claude alone on its host's local bus while the panes
// beside it — same project, same group — were on the hub's.
func TestPeersBus_PanelessIsAnswered(t *testing.T) {
	s, _ := busServer(t)
	var askedFor string
	seen := false
	s.SetBusResolver(func(paneID string) (string, string, error) {
		askedFor, seen = paneID, true
		return "http://127.0.0.1:7900/v1/hubbus", "host-shared-token", nil
	})

	rec := busAskPaneless(t, s, peers.PanelessToken(testSecret))

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if !seen {
		t.Fatal("the resolver was never consulted for a pane-less session")
	}
	if askedFor != "" {
		t.Errorf("resolver asked about pane %q, want the empty pane-less id", askedFor)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["url"] != "http://127.0.0.1:7900/v1/hubbus" || got["token"] != "host-shared-token" {
		t.Errorf("answer = %v, want the relay and its token", got)
	}
}

// The pane-less path is credentialed, not open: the shared token is what proves
// the caller can already read the daemon-info file.
func TestPeersBus_PanelessRejectsAWrongToken(t *testing.T) {
	s, _ := busServer(t)
	s.SetBusResolver(func(string) (string, string, error) {
		t.Error("the resolver ran for an unauthorized caller")
		return "", "", nil
	})
	for _, token := range []string{"", "not-the-token", peers.TokenForPane(testSecret, "pane-1")} {
		rec := busAskPaneless(t, s, token)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", token, rec.Code)
		}
	}
}

// A pane's own token still only answers for that pane — the pane-less branch
// must not have widened the pane branch.
func TestPeersBus_PaneTokenStillScopedToItsPane(t *testing.T) {
	s, _ := busServer(t)
	s.SetBusResolver(func(string) (string, string, error) { return "u", "t", nil })
	rec := busAsk(t, s, "pane-2", peers.TokenForPane(testSecret, "pane-1"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for another pane's token", rec.Code)
	}
}

// The relay swaps the caller's local shared token for the hub's, so the hub's
// credential stays inside the daemon instead of being handed to every local
// process that can read the daemon-info file.
func TestHubBusRelay_SwapsThePanelessCredential(t *testing.T) {
	var gotAuth string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
	}))
	defer hubSrv.Close()

	s := &Server{}
	s.SetHubBus(func() string { return hubSrv.URL }, http.DefaultTransport, func(inbound string) string {
		if inbound == "local-shared" {
			return "hub-shared"
		}
		return ""
	})
	member := httptest.NewServer(s.Handler())
	defer member.Close()

	for _, tc := range []struct{ name, send, want string }{
		{"pane-less: swapped", "local-shared", "Bearer hub-shared"},
		{"a pane's hub-minted token: untouched", "pane-token", "Bearer pane-token"},
		{"anything else: untouched, for the hub to reject", "garbage", "Bearer garbage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("POST", member.URL+HubBusPrefix+"/v1/peers/register", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tc.send)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if gotAuth != tc.want {
				t.Errorf("hub saw %q, want %q", gotAuth, tc.want)
			}
		})
	}
}

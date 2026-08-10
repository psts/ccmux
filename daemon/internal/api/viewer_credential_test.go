package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
)

func viewerCredServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""
	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func getViewer(t *testing.T, url, token string) (*http.Response, map[string]string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url+"/v1/peers/viewer", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	out := map[string]string{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

// The token unlocks a fleet-wide read, so it goes only to a caller that proved
// same-user with the 0600 file's credential. Which bus to read is not a secret
// and is answered either way, so a browser — which can hold no credential — can
// still tell whether the list it renders is the whole picture.
func TestPeersViewerCredential_TokenNeedsSameUserProof(t *testing.T) {
	ts, _ := viewerCredServer(t)

	resp, out := getViewer(t, ts.URL, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d without a token, want 200 — the bus answer is not gated", resp.StatusCode)
	}
	if out["token"] != "" {
		t.Error("a credential was handed to an unauthenticated caller")
	}

	resp, out = getViewer(t, ts.URL, peers.PanelessToken(testSecret))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d with the local token, want 200", resp.StatusCode)
	}
	if out["token"] != peers.ViewerToken(testSecret) {
		t.Error("the served credential is not the read-only viewer token")
	}
	if out["token"] == peers.PanelessToken(testSecret) {
		t.Error("the pane-less token was served — a lens must not be able to register peers")
	}
}

// The address a request arrives from is not identity. On a shared host an
// unprivileged local account can reach this daemon's own tsnet node through the
// machine's tailscaled, arriving NOT from loopback — so a rule that trusted
// non-loopback callers would hand that account the token it must never have.
func TestPeersViewerCredential_RemoteCallerGetsNoTokenEither(t *testing.T) {
	_, srv := viewerCredServer(t)
	handler := srv.Handler()

	req := httptest.NewRequest("GET", "/v1/peers/viewer", nil)
	req.RemoteAddr = "100.80.201.127:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	out := map[string]string{}
	_ = json.NewDecoder(rec.Body).Decode(&out)
	if out["token"] != "" {
		t.Error("a tailnet caller with no credential was handed the viewer token")
	}
}

// Where to read is the same answer the sessions got, or the lens and the bus
// disagree about who is present.
func TestPeersViewerCredential_NamesTheBus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver func(string) (string, string, error)
		want     string
	}{
		{"no resolver (single host)", nil, ""},
		{"resolver says no hub", func(string) (string, string, error) { return "", "", nil }, ""},
		{"hub federated", func(string) (string, string, error) { return "http://127.0.0.1:7900/v1/hubbus", "t", nil }, HubBusPrefix},
		// A hub this daemon knows about but cannot reach right now. Answering ""
		// would send the lens to a local registry holding nobody, which draws as
		// "no one is here" instead of as the fault it is.
		{"hub unreachable", func(string) (string, string, error) { return "", "", errors.New("bus unavailable") }, HubBusPrefix},
	} {
		ts, s := viewerCredServer(t)
		if tc.resolver != nil {
			s.SetBusResolver(tc.resolver)
		}
		resp, out := getViewer(t, ts.URL, peers.PanelessToken(testSecret))
		// Asserted, not discarded: without this the rows expecting "" pass on any
		// failure at all, because a 401 or an empty body decodes to an empty map
		// and compares equal. Half the table could not fail.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tc.name, resp.StatusCode)
			continue
		}
		if out["bus"] != tc.want {
			t.Errorf("%s: bus = %q, want %q", tc.name, out["bus"], tc.want)
		}
	}
}

// The credential the endpoint serves must actually open the relay it is for.
func TestHubBusRelay_AcceptsTheViewerToken(t *testing.T) {
	reached := false
	member, _ := viewerRelayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		writeJSON(w, http.StatusOK, []map[string]string{})
	}))
	req, _ := http.NewRequest("GET", member.URL+HubBusPrefix+"/v1/peers?group=ChartLabs", nil)
	req.Header.Set("Authorization", "Bearer "+peers.ViewerToken(testSecret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !reached {
		t.Fatalf("status = %d, reached hub = %v — the viewer token must open the read surface", resp.StatusCode, reached)
	}
}

// …and nothing else. Bus traffic is forwarded for the hub to judge, so what the
// relay must never do is upgrade a lens credential on the way: the hub has to
// see the viewer token as-is and refuse it, not a host credential that works.
func TestHubBusRelay_DoesNotUpgradeAViewerToken(t *testing.T) {
	var gotAuth string
	member, _ := viewerRelayServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeError(w, http.StatusUnauthorized, "invalid peer token")
	}))
	for _, path := range []string{"/v1/peers/register", "/v1/peers/send"} {
		req, _ := http.NewRequest("POST", member.URL+HubBusPrefix+path, nil)
		req.Header.Set("Authorization", "Bearer "+peers.ViewerToken(testSecret))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if gotAuth != "Bearer "+peers.ViewerToken(testSecret) {
			t.Errorf("%s: hub saw auth %q — a lens credential was upgraded into a bus one", path, gotAuth)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want the hub's 401 to reach the caller", path, resp.StatusCode)
		}
	}
}

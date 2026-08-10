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

// A loopback caller is any local account on a shared host — the exact reader the
// relay's credential exists to exclude. It has to prove same-user first, with
// the token out of the 0600 daemon-info file.
func TestPeersViewerCredential_LoopbackMustProveSameUser(t *testing.T) {
	ts, _ := viewerCredServer(t)

	resp, out := getViewer(t, ts.URL, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d without a token, want 401", resp.StatusCode)
	}
	if out["token"] != "" {
		t.Error("a credential was handed to an unauthenticated loopback caller")
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
		_, out := getViewer(t, ts.URL, peers.PanelessToken(testSecret))
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

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/store"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

type nullHook struct{}

func (nullHook) GroupForPane(string) (string, bool)                         { return "", false }
func (nullHook) PaneAtShell(string) bool                                    { return false }
func (nullHook) LiveWorkspaceForRepo(string, string) (string, string, bool) { return "", "", false }
func (nullHook) SpawnEphemeralPane(string, string, string, string) error    { return nil }

// newPeersTestServer stands up the peers surface over a real HTTP server —
// pane-less peers only (nullHook), which exercises the dirname fallback group.
func newPeersTestServer(t *testing.T) (*httptest.Server, *peers.Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := peers.NewService(st, nullHook{}, testSecret)
	svc.OpenCmd = ""
	s := &Server{peersSvc: svc, upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, svc
}

func postJSON(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

var nextFakePID = 1 << 20 // above macOS's pid range; distinct per peer because
// registration evicts stale same-pid records (one MCP server per process).

func registerPeer(t *testing.T, ts *httptest.Server, cwd string) string {
	t.Helper()
	nextFakePID++
	resp := postJSON(t, ts.URL+"/v1/peers/register", peers.PanelessToken(testSecret),
		map[string]any{"pid": nextFakePID, "cwd": cwd, "git_root": cwd})
	if resp.StatusCode != 200 {
		t.Fatalf("register: status %d", resp.StatusCode)
	}
	var reg struct {
		PeerID string `json:"peer_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		t.Fatal(err)
	}
	return reg.PeerID
}

func TestPeersAPI_TokenRequired(t *testing.T) {
	ts, _ := newPeersTestServer(t)

	// Register without a token → 401.
	if resp := postJSON(t, ts.URL+"/v1/peers/register", "", map[string]any{"pid": 1, "cwd": "/x/a"}); resp.StatusCode != 401 {
		t.Fatalf("tokenless register = %d, want 401", resp.StatusCode)
	}
	// The old broker accepted this exact call — the port must not.
	a := registerPeer(t, ts, "/x/a")
	b := registerPeer(t, ts, "/x/b")
	if resp := postJSON(t, ts.URL+"/v1/peers/send", "", map[string]any{
		"from_id": a, "to_id": b, "text": "injected"}); resp.StatusCode != 401 {
		t.Fatalf("tokenless send = %d, want 401", resp.StatusCode)
	}
	// A valid token cannot act as someone else's from_id when identities differ.
	// (Both pane-less peers share the pane-less token, so impersonation within
	// that class is possible by design; pane peers each get their own token.)
	if resp := postJSON(t, ts.URL+"/v1/peers/send", "wrong-token", map[string]any{
		"from_id": a, "to_id": b, "text": "injected"}); resp.StatusCode != 401 {
		t.Fatalf("bad-token send = %d, want 401", resp.StatusCode)
	}
}

func TestPeersAPI_MutationsAreLoopbackOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "peers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{peersSvc: peers.NewService(st, nullHook{}, testSecret)}

	body := bytes.NewReader([]byte(`{"pid":1,"cwd":"/x/a"}`))
	req := httptest.NewRequest("POST", "/v1/peers/register", body)
	req.Header.Set("Authorization", "Bearer "+peers.PanelessToken(testSecret))
	req.RemoteAddr = "100.64.1.5:44321" // a tailnet peer, not loopback
	rec := httptest.NewRecorder()
	s.peersRegister(rec, req)
	if rec.Code != 403 {
		t.Fatalf("tailnet register = %d, want 403", rec.Code)
	}
}

func dialPeerWS(t *testing.T, ts *httptest.Server, peerID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/peers/ws?peer_id=" + peerID
	h := http.Header{"Authorization": {"Bearer " + peers.PanelessToken(testSecret)}}
	conn, _, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		t.Fatalf("dial peer ws: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var frame map[string]any
	if err := conn.ReadJSON(&frame); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

func TestPeersAPI_PushAckReplayCycle(t *testing.T) {
	ts, _ := newPeersTestServer(t)
	a := registerPeer(t, ts, "/x/a")
	b := registerPeer(t, ts, "/x/b")
	token := peers.PanelessToken(testSecret)

	// Message sent BEFORE b subscribes → covered by replay-on-subscribe.
	if resp := postJSON(t, ts.URL+"/v1/peers/send", token, map[string]any{
		"from_id": a, "to_id": b, "text": "early"}); resp.StatusCode != 200 {
		t.Fatalf("send = %d", resp.StatusCode)
	}

	conn := dialPeerWS(t, ts, b)
	frame := readFrame(t, conn)
	if frame["type"] != "message" || frame["text"] != "early" {
		t.Fatalf("replayed frame = %v", frame)
	}
	seq := frame["seq"].(float64)

	// Reconnect WITHOUT acking → the same event replays (no loss)...
	conn.Close()
	conn2 := dialPeerWS(t, ts, b)
	frame2 := readFrame(t, conn2)
	if frame2["text"] != "early" || frame2["seq"].(float64) != seq {
		t.Fatalf("unacked event not replayed: %v", frame2)
	}
	// ...then ack it and reconnect → silence (no duplicates).
	if err := conn2.WriteJSON(map[string]any{"type": "ack", "seq": seq}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the ack land before dropping the socket
	conn2.Close()

	// After the ack, a reconnect must NOT replay "early". Send "live" and check
	// the FIRST frame on the fresh socket is "live" — a duplicate would arrive
	// ahead of it in seq order.
	conn3 := dialPeerWS(t, ts, b)
	if resp := postJSON(t, ts.URL+"/v1/peers/send", token, map[string]any{
		"from_id": a, "to_id": b, "text": "live"}); resp.StatusCode != 200 {
		t.Fatalf("send = %d", resp.StatusCode)
	}
	if frame := readFrame(t, conn3); frame["text"] != "live" {
		t.Fatalf("first frame after ack = %v, want live (no early replay)", frame)
	}

	// check_messages semantics: the pushed-but-unacked "live" event is still
	// pending, so poll returns it once and advances the shared cursor.
	resp := postJSON(t, ts.URL+"/v1/peers/poll", token, map[string]any{"peer_id": b})
	var poll struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&poll); err != nil {
		t.Fatal(err)
	}
	if len(poll.Events) != 1 || poll.Events[0]["text"] != "live" {
		t.Fatalf("poll = %+v, want the unacked live event", poll.Events)
	}
}

func TestPeersAPI_ListenerStreamAndHistory(t *testing.T) {
	ts, _ := newPeersTestServer(t)
	a := registerPeer(t, ts, "/proj/a")
	b := registerPeer(t, ts, "/proj/b")
	token := peers.PanelessToken(testSecret)

	// Listener needs no token (read-only viewer surface). Group is the holding
	// folder's name for these pane-less peers.
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/peers/ws?mode=listen&group=proj"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	defer conn.Close()

	if resp := postJSON(t, ts.URL+"/v1/peers/send", token, map[string]any{
		"from_id": a, "to_id": b, "text": "visible to viewers"}); resp.StatusCode != 200 {
		t.Fatalf("send = %d", resp.StatusCode)
	}
	frame := readFrame(t, conn)
	if frame["type"] != "message" || frame["text"] != "visible to viewers" {
		t.Fatalf("listener frame = %v", frame)
	}

	// REST history for the same group.
	resp, err := http.Get(ts.URL + "/v1/peers/messages?group=proj")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msgs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0]["text"] != "visible to viewers" || msgs[0]["from_id"] != a {
		t.Fatalf("history = %+v", msgs)
	}
}

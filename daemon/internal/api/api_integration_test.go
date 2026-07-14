package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/hooks"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// TestAPI_CreateAttachInput drives the whole daemon stack over HTTP+WS against a
// real tmux server: create a workspace, attach via WebSocket, receive the seed
// snapshot, send a keystroke, and receive the live echoed output.
func TestAPI_CreateAttachInput(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-api-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}

	hs := httptest.NewServer(NewServer(mgr).Handler())
	defer hs.Close()

	// Create a workspace.
	body := `{"name":"t","repoPath":"/tmp","createdBy":"tester"}`
	resp, err := http.Post(hs.URL+"/v1/workspaces", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var ws struct {
		ID    string `json:"id"`
		Panes []struct {
			ID string `json:"id"`
		} `json:"panes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if ws.ID == "" || len(ws.Panes) != 1 {
		t.Fatalf("unexpected workspace: %+v", ws)
	}
	pane0 := ws.Panes[0].ID

	// Attach via WebSocket.
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/attach?workspace=" + ws.ID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// Expect hello, then a snapshot for the pane.
	if m := readMsg(t, conn); m.T != "hello" {
		t.Fatalf("first frame = %q, want hello", m.T)
	}

	// Send a keystroke that will echo a unique marker.
	marker := "API_MARKER_8842"
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0, Data: base64.StdEncoding.EncodeToString([]byte("printf " + marker + "\n"))})

	// Read frames until the marker appears in some pane's output/snapshot.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var acc strings.Builder
	found := false
	for i := 0; i < 200 && !found; i++ {
		m := readMsg(t, conn)
		if m.T == "output" || m.T == "snapshot" {
			if b, err := base64.StdEncoding.DecodeString(m.Data); err == nil {
				acc.Write(b)
				if strings.Contains(acc.String(), marker) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("marker %q not seen over WS attach", marker)
	}
}

// TestAPI_HookAttentionReachesLens proves the Phase 2 loop end-to-end: a Claude
// Code hook message arriving on the Unix socket updates a pane's attention and
// is broadcast to an already-attached WebSocket lens.
func TestAPI_HookAttentionReachesLens(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-hookattn-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	hookSock := "/tmp/ccmux-hookattn.sock"
	_ = os.Remove(hookSock)
	hl, err := hooks.Listen(hookSock, mgr)
	if err != nil {
		t.Fatalf("hooks listen: %v", err)
	}
	defer hl.Close()

	hs := httptest.NewServer(NewServer(mgr).Handler())
	defer hs.Close()

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	// Attach a lens.
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/attach?workspace=" + ws.ID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	if m := readMsg(t, conn); m.T != "hello" {
		t.Fatalf("first frame = %q, want hello", m.T)
	}

	// Fire a permission_request hook (exact pane via pane_id).
	hookConn, err := net.Dial("unix", hookSock)
	if err != nil {
		t.Fatalf("hook dial: %v", err)
	}
	_, _ = hookConn.Write([]byte(`{"type":"permission_request","cwd":"/tmp","pane_id":"` + pane0 + `"}`))
	hookConn.Close()

	// Expect an attention=needs_input frame for pane0.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 200; i++ {
		m := readMsg(t, conn)
		if m.T == "attention" && m.Pane == pane0 {
			if string(m.State) != "needs_input" {
				t.Fatalf("attention state = %q, want needs_input", m.State)
			}
			return // success
		}
	}
	t.Fatal("did not receive attention frame for pane0")
}

// TestAPI_EventsFirehose proves the /v1/events firehose: a lens that never
// attaches to the workspace still receives its attention change, tagged with the
// workspace id so a sidebar can flash the right row. The opening hello also seeds
// the pane's current (idle) attention.
func TestAPI_EventsFirehose(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-events-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	hookSock := "/tmp/ccmux-events.sock"
	_ = os.Remove(hookSock)
	hl, err := hooks.Listen(hookSock, mgr)
	if err != nil {
		t.Fatalf("hooks listen: %v", err)
	}
	defer hl.Close()

	hs := httptest.NewServer(NewServer(mgr).Handler())
	defer hs.Close()

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	// Connect to the firehose only — deliberately NOT /v1/attach.
	evURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/events"
	conn, _, err := websocket.DefaultDialer.Dial(evURL, nil)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	// hello must carry the pane's current attention for the live workspace.
	hello := readFirehose(t, conn)
	if hello.T != "hello" {
		t.Fatalf("first frame = %q, want hello", hello.T)
	}
	seen := false
	for _, e := range hello.Attention {
		if e.Workspace == ws.ID && e.Pane == pane0 {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("hello did not seed pane0 attention: %+v", hello.Attention)
	}

	// Fire a permission_request hook; expect a workspace-tagged attention frame.
	hookConn, err := net.Dial("unix", hookSock)
	if err != nil {
		t.Fatalf("hook dial: %v", err)
	}
	_, _ = hookConn.Write([]byte(`{"type":"permission_request","cwd":"/tmp","pane_id":"` + pane0 + `"}`))
	hookConn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 200; i++ {
		m := readFirehose(t, conn)
		if m.T == "attention" && m.Pane == pane0 {
			if m.Workspace != ws.ID {
				t.Fatalf("attention workspace = %q, want %q", m.Workspace, ws.ID)
			}
			if string(m.State) != "needs_input" {
				t.Fatalf("attention state = %q, want needs_input", m.State)
			}
			return // success
		}
	}
	t.Fatal("did not receive firehose attention frame for pane0")
}

// TestAPI_PaneDriver drives the git-attribution path: with nobody attached the
// driver endpoint is empty (204 → a solo commit gets no trailer); once a lens
// attaches and types, GET /v1/panes/{id}/driver names that typist.
func TestAPI_PaneDriver(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	const socket = "ccmux-driver-itest"
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	hs := httptest.NewServer(NewServer(mgr).Handler())
	defer hs.Close()

	ws, err := mgr.CreateWorkspace("t", "/tmp", "/tmp", "", "tester")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pane0 := ws.Panes[0].ID

	// Unknown pane → 404.
	if code := getStatus(t, hs.URL+"/v1/panes/nope/driver"); code != http.StatusNotFound {
		t.Fatalf("unknown pane status = %d, want 404", code)
	}
	// No one attached yet → 204 (no driver).
	if code := getStatus(t, hs.URL+"/v1/panes/"+pane0+"/driver"); code != http.StatusNoContent {
		t.Fatalf("no-driver status = %d, want 204", code)
	}

	// Attach as Alice and type, making her the driver.
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http") + "/v1/attach?workspace=" + ws.ID + "&user=Alice&device=laptop"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	if m := readMsg(t, conn); m.T != "hello" {
		t.Fatalf("first frame = %q, want hello", m.T)
	}
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0, Data: base64.StdEncoding.EncodeToString([]byte("x"))})

	// Poll the driver endpoint until Alice shows up (input is applied async).
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := getJSON(t, hs.URL+"/v1/panes/"+pane0+"/driver")
		if code == http.StatusOK {
			if body["user"] != "Alice" {
				t.Fatalf("driver user = %v, want Alice", body["user"])
			}
			if body["device"] != "laptop" {
				t.Fatalf("driver device = %v, want laptop", body["device"])
			}
			return // success
		}
		if time.Now().After(deadline) {
			t.Fatalf("driver never became Alice (last status %d)", code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode driver: %v", err)
	}
	return resp.StatusCode, body
}

func readMsg(t *testing.T, conn *websocket.Conn) wsMsg {
	t.Helper()
	var m wsMsg
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

func readFirehose(t *testing.T, conn *websocket.Conn) firehoseMsg {
	t.Helper()
	var m firehoseMsg
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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

func readMsg(t *testing.T, conn *websocket.Conn) wsMsg {
	t.Helper()
	var m wsMsg
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}

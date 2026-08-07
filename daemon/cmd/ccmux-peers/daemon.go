// ccmuxd client half: HTTP RPCs plus the push WebSocket (replay + live, with
// cumulative acks sent only after the corresponding MCP notification is out).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsReconnectBase = time.Second
	wsReconnectMax  = 15 * time.Second
)

// daemonClient is RE-TARGETABLE on purpose. Which bus this process belongs to is
// not knowable once at startup: a hub can appear, disappear, or move while the
// pane lives, and panes outlive daemons. The target is therefore re-resolved on
// every reconnect (see app.resolveBus) and swapped here, so the pointer held by
// the tool handlers never changes and no call site needs to care.
type daemonClient struct {
	mu      sync.Mutex
	baseURL string
	token   string
	http    *http.Client
}

func newDaemonClient(baseURL, token string) *daemonClient {
	return &daemonClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// target reports the current base URL and token as one consistent pair.
func (d *daemonClient) target() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.baseURL, d.token
}

// retarget points this client at a different bus. Reports whether anything
// changed, so the caller can re-register only when it actually moved.
func (d *daemonClient) retarget(baseURL, token string) bool {
	baseURL = strings.TrimRight(baseURL, "/")
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.baseURL == baseURL && d.token == token {
		return false
	}
	d.baseURL, d.token = baseURL, token
	return true
}

func (d *daemonClient) post(path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	baseURL, token := d.target()
	req, err := http.NewRequest("POST", baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&apiErr)
		if apiErr.Error == "" {
			apiErr.Error = resp.Status
		}
		return fmt.Errorf("ccmuxd (%s): %s", path, apiErr.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// wireEvent is a push/poll frame — kind discriminated by Type.
type wireEvent struct {
	Type        string `json:"type"`
	Seq         int64  `json:"seq"`
	FromID      string `json:"from_id"`
	FromName    string `json:"from_name"`
	FromSummary string `json:"from_summary"`
	FromCWD     string `json:"from_cwd"`
	ToID        string `json:"to_id"`
	ToName      string `json:"to_name"`
	Text        string `json:"text"`
	SentAt      string `json:"sent_at"`
	RequestID   string `json:"request_id"`
	Behavior    string `json:"behavior"`
}

// runPushLoop keeps the peer WebSocket alive for the process lifetime:
// dial → (server replays past the cursor) → dispatch each frame → ack.
// On any drop it re-registers (idempotent; the daemon may have restarted)
// and redials with backoff — cursor replay makes reconnects lossless.
func (a *app) runPushLoop() {
	delay := wsReconnectBase
	for isBusOwner() { // yield to busLoop if this process is no longer the session
		if err := a.pushOnce(); err != nil {
			logf("push channel: %v (reconnecting in %s)", err, delay)
		}
		time.Sleep(delay)
		delay = min(delay*2, wsReconnectMax)
		if err := a.register(); err != nil {
			logf("re-register failed: %v", err)
			continue
		}
		delay = wsReconnectBase
	}
}

func (a *app) pushOnce() error {
	baseURL, token := a.daemon.target()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") +
		"/v1/peers/ws?peer_id=" + a.peerID()
	header := http.Header{"Authorization": {"Bearer " + token}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		return err
	}
	defer conn.Close()
	a.setConn(conn)
	defer a.dropConn()
	logf("push channel connected")

	conn.SetPingHandler(func(payload string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(payload), time.Now().Add(10*time.Second))
	})
	for {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		var ev wireEvent
		if err := conn.ReadJSON(&ev); err != nil {
			return err
		}
		if !a.dispatchEvent(ev) {
			continue // notification failed — leave unacked so it replays
		}
		if err := conn.WriteJSON(map[string]any{"type": "ack", "seq": ev.Seq}); err != nil {
			return err
		}
	}
}

// dispatchEvent forwards one bus event into Claude Code. Returns true when the
// MCP notification was written (ack-worthy).
//
// A successful write records the seq as shown, which is what stops a concurrent
// check_messages from rendering the same message a second time while this one is
// still waiting to be acked.
func (a *app) dispatchEvent(ev wireEvent) bool {
	if a.alreadyShown(ev.Seq) {
		return true // already in front of the model; ack it away
	}
	switch ev.Type {
	case "permission_verdict":
		err := a.mcp.Notify("notifications/claude/channel/permission", map[string]any{
			"request_id": ev.RequestID,
			"behavior":   ev.Behavior,
		})
		if err == nil {
			a.markShown(ev.Seq)
			logf("relayed verdict %s %s from %s", ev.Behavior, ev.RequestID, ev.FromID)
		}
		return err == nil
	case "message":
		if !a.channelMode {
			return false // poll-only session: check_messages is the delivery path
		}
		err := a.mcp.Notify("notifications/claude/channel", map[string]any{
			"content": ev.Text,
			"meta": map[string]any{
				"from_name":    ev.FromName,
				"from_id":      ev.FromID,
				"from_summary": ev.FromSummary,
				"from_cwd":     ev.FromCWD,
				"sent_at":      ev.SentAt,
			},
		})
		if err == nil {
			a.markShown(ev.Seq)
			logf("pushed message from %s: %.80s", orID(ev.FromName, ev.FromID), ev.Text)
		}
		return err == nil
	default:
		return true // unknown frame kinds are acked away harmlessly
	}
}

func orID(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

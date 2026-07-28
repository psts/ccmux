package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
)

// floodStack spins up a real daemon (tmux + manager + HTTP/WS server) for a
// flood/interrupt test and returns the base URL. Cleanup is registered on t.
func floodStack(t *testing.T, socket string) (*manager.Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	tsrv := &tmux.Server{Socket: socket, ConfigPath: "../../config/tmux.conf"}
	_ = tsrv.KillServer()
	t.Cleanup(func() { _ = tsrv.KillServer() })

	st, err := store.Open(t.TempDir() + "/reg.db")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mgr := manager.New(ctx, tsrv, st)
	if err := mgr.Start(); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	hs := httptest.NewServer(NewServer(mgr).Handler())
	t.Cleanup(hs.Close)
	return mgr, hs.URL
}

// wsResult is the decoded shape of a created workspace used by these tests.
type wsResult struct {
	ID    string `json:"id"`
	Panes []struct {
		ID string `json:"id"`
	} `json:"panes"`
}

// createWS POSTs a workspace at /tmp and returns its decoded id + panes.
//
// startupCommand is explicitly "" — a BARE SHELL. Omitting it takes the daemon's
// configured default, which falls back to launching a real Claude session, and
// these tests then type their flood command into Claude's prompt instead of a
// shell: nothing floods, the subscriber never lags, and the reseed path under
// test is never reached. An empty string is the documented way to ask for no
// startup command, and it keeps the test hermetic — no dependency on whether
// claude is installed or how it renders.
func createWS(t *testing.T, base string) wsResult {
	t.Helper()
	body := `{"name":"flood","repoPath":"/tmp","createdBy":"tester","startupCommand":""}`
	resp, err := http.Post(base+"/v1/workspaces", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	var ws wsResult
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		t.Fatalf("decode ws: %v", err)
	}
	if ws.ID == "" || len(ws.Panes) == 0 {
		t.Fatalf("unexpected workspace: %+v", ws)
	}
	return ws
}

var seqRe = regexp.MustCompile(`SEQ(\d{8})`)

// seqRange returns the min and max SEQ number embedded in b, and whether any was
// found.
func seqRange(b []byte) (min, max int, found bool) {
	for _, m := range seqRe.FindAllSubmatch(b, -1) {
		n, _ := strconv.Atoi(string(m[1]))
		if !found || n < min {
			min = n
		}
		if !found || n > max {
			max = n
		}
		found = true
	}
	return min, max, found
}

// TestAPI_FloodReseedNoStaleReplay is the regression guard for the
// reseed-after-lag corruption. A lens that stops reading during a flood lags; the
// server drops the stale backlog and reseeds from a fresh capture. The bug was
// that the stale buffered output was replayed AFTER the reseed snapshot, layering
// old lines (low SEQ) on top of the current screen (high SEQ). We assert that once
// a reseed snapshot shows a high SEQ, no later output frame carries a wildly stale
// SEQ.
func TestAPI_FloodReseedNoStaleReplay(t *testing.T) {
	_, base := floodStack(t, "ccmux-flood-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/attach?workspace=" + ws.ID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	if m := readMsg(t, conn); m.T != "hello" {
		t.Fatalf("first frame = %q, want hello", m.T)
	}

	// An UNBOUNDED, strictly-increasing flood. The client below reads SLOWLY, so
	// the server's writeLoop stays blocked on the socket while the subscriber buffer
	// overflows and drops — the sub is continually flagged lagged, firing the
	// drain+reseed path mid-stream (with live output still arriving after each
	// reseed). This is deterministic regardless of OS socket-buffer sizing, unlike a
	// bounded burst that can land whole in the socket.
	flood := `awk 'BEGIN{for(i=1;;i++)printf "SEQ%08d\n",i}'` + "\n"
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0, Data: base64.StdEncoding.EncodeToString([]byte(flood))})

	// Read slowly for a bounded window. A "reseed" is a snapshot that arrives AFTER
	// we've already seen live output — it reflects the current screen up to SEQ
	// snapMax. Correct behavior: every later output is strictly NEW (SEQ > snapMax).
	// The bug replayed the stale buffered backlog on top of the reseed, so a
	// post-reseed output frame would carry a SEQ below the floor — old lines that
	// already scrolled off.
	floor := 0
	reseedSeen := false
	outputsSeen := 0
	snapsSeen := 0
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			break
		}
		time.Sleep(4 * time.Millisecond) // stay behind the producer → sustained lag
		b, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			continue
		}
		lo, hi, found := seqRange(b)
		switch m.T {
		case "snapshot":
			snapsSeen++
			if outputsSeen > 0 {
				reseedSeen = true // a snapshot after live output = a lag-triggered reseed
			}
			if found && hi > floor {
				floor = hi
			}
		case "output":
			outputsSeen++
			// A frame whose MAX seq sits well below the reseed floor is wholly stale
			// — it replays lines that already scrolled off the current screen. The
			// margin tolerates the benign one-frame snapshot/stream boundary overlap
			// (hi ≈ floor); it is far smaller than the buffered backlog the bug
			// replayed (tens of thousands of SEQ below floor).
			const staleMargin = 10000
			if reseedSeen && found && hi < floor-staleMargin {
				t.Fatalf("stale output replayed after reseed: frame SEQ[%d..%d] wholly below reseed floor %d — the reseed corruption regressed",
					lo, hi, floor)
			}
		}
	}
	// Stop the infinite flood so teardown is clean.
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0, Data: base64.StdEncoding.EncodeToString([]byte{0x03})})
	t.Logf("frames: outputs=%d snaps=%d reseedSeen=%v floor=%d", outputsSeen, snapsSeen, reseedSeen, floor)
	if !reseedSeen {
		t.Fatal("flood never triggered a lag reseed — the repro no longer exercises the drain+reseed path (check buffer sizing / timing)")
	}
}

// TestAPI_CtrlCInterruptLatency proves Ctrl-C interrupts a flooding pane fast
// (Phase 8 target < 200ms), measured end-to-end over the WS. A fast-draining
// reader keeps buffers near-empty, so client-observed quiescence tracks the real
// interrupt latency rather than a drain backlog.
func TestAPI_CtrlCInterruptLatency(t *testing.T) {
	_, base := floodStack(t, "ccmux-ctrlc-itest")

	ws := createWS(t, base)
	pane0 := ws.Panes[0].ID

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/attach?workspace=" + ws.ID
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	var lastOutput atomic.Int64 // unix-nanos of the most recent output frame
	var floodActive atomic.Bool
	var mu sync.Mutex
	readErr := error(nil)
	go func() {
		for {
			var m wsMsg
			if err := conn.ReadJSON(&m); err != nil {
				mu.Lock()
				readErr = err
				mu.Unlock()
				return
			}
			if m.T == "output" || m.T == "snapshot" {
				b, _ := base64.StdEncoding.DecodeString(m.Data)
				if strings.Contains(string(b), "CTRLC_FLOOD") {
					floodActive.Store(true)
				}
				lastOutput.Store(time.Now().UnixNano())
			}
		}
	}()

	// Unbounded flood so it is definitely still running when we interrupt.
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0,
		Data: base64.StdEncoding.EncodeToString([]byte("yes CTRLC_FLOOD_TOKEN\n"))})

	// Wait until the flood is actually reaching the client.
	deadline := time.Now().Add(5 * time.Second)
	for !floodActive.Load() {
		if time.Now().After(deadline) {
			t.Fatal("flood never reached the client")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // let it run steadily

	// Interrupt.
	t0 := time.Now()
	_ = conn.WriteJSON(wsMsg{T: "input", Pane: pane0,
		Data: base64.StdEncoding.EncodeToString([]byte{0x03})})

	// Quiescence = no new output for 150ms. Interrupt latency = last output ts - t0.
	for {
		time.Sleep(15 * time.Millisecond)
		last := time.Unix(0, lastOutput.Load())
		if time.Since(last) >= 150*time.Millisecond {
			latency := last.Sub(t0)
			if latency < 0 {
				latency = 0 // last output preceded the interrupt (already quiet)
			}
			t.Logf("Ctrl-C interrupt latency (end-to-end over WS): %v", latency)
			if latency > 200*time.Millisecond {
				t.Fatalf("Ctrl-C latency %v exceeds 200ms target", latency)
			}
			break
		}
		if time.Since(t0) > 5*time.Second {
			t.Fatal("pane never quiesced after Ctrl-C")
		}
	}
	mu.Lock()
	_ = readErr
	mu.Unlock()
}

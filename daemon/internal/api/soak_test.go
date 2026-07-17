package api

import (
	"encoding/base64"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAPI_Soak_TwoClientsFourPanes runs the Phase 8 soak: two lenses on one
// workspace of four panes under sustained I/O, plus connection churn. It guards
// against the failure modes daily multi-dev use would expose:
//   - goroutine leaks: every attach spawns a read loop that must exit on
//     disconnect; after churn the goroutine count must return to baseline.
//   - deadlock/drift: after the load, every pane must still round-trip a fresh
//     marker (the fan-out is not wedged and no pane fell behind permanently).
func TestAPI_Soak_TwoClientsFourPanes(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped in -short mode")
	}
	mgr, base := floodStack(t, "ccmux-soak-itest")

	ws, err := mgr.CreateWorkspace("soak", "/tmp", "/tmp", "", "tester", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	panes := []string{ws.Panes[0].ID}
	for i := 0; i < 3; i++ {
		p, err := mgr.SpawnPane(ws.ID, "/tmp", "", "tester")
		if err != nil {
			t.Fatalf("spawn pane %d: %v", i, err)
		}
		panes = append(panes, p.ID)
	}
	if len(panes) != 4 {
		t.Fatalf("want 4 panes, got %d", len(panes))
	}

	// Warm up to steady-state goroutines (first attach lazily creates some), then
	// snapshot the baseline.
	for i := 0; i < 3; i++ {
		c := attachAndHello(t, base, ws.ID)
		_ = c.Close()
	}
	time.Sleep(400 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// --- sustained load: two clients, each draining continuously while bursting
	//     output on all four panes for a couple of seconds ---
	var wg sync.WaitGroup
	loadUntil := time.Now().Add(2500 * time.Millisecond)
	for client := 0; client < 2; client++ {
		wg.Add(1)
		go func(cn int) {
			defer wg.Done()
			conn := attachAndHello(t, base, ws.ID)
			defer conn.Close()
			// Reader goroutine drains frames so the fan-out never blocks.
			stop := make(chan struct{})
			var rwg sync.WaitGroup
			rwg.Add(1)
			go func() {
				defer rwg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
					var m wsMsg
					if err := conn.ReadJSON(&m); err != nil {
						if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
							continue
						}
						return
					}
				}
			}()
			// Burst output on every pane, repeatedly.
			for time.Now().Before(loadUntil) {
				for _, p := range panes {
					_ = conn.WriteJSON(wsMsg{T: "input", Pane: p,
						Data: base64.StdEncoding.EncodeToString([]byte("seq 1 500 >/dev/null; echo tick\n"))})
				}
				time.Sleep(60 * time.Millisecond)
			}
			close(stop)
			rwg.Wait()
		}(client)
	}
	wg.Wait()

	// --- connection churn: rapid attach/detach to exercise sub + presence teardown ---
	for i := 0; i < 12; i++ {
		c := attachAndHello(t, base, ws.ID)
		// send one input so the readLoop path is exercised, then drop.
		_ = c.WriteJSON(wsMsg{T: "input", Pane: panes[i%4],
			Data: base64.StdEncoding.EncodeToString([]byte("true\n"))})
		_ = c.Close()
	}

	// Let all per-attach goroutines unwind, then compare against baseline.
	time.Sleep(1 * time.Second)
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > baseline+8 {
		t.Fatalf("goroutine leak: baseline=%d after churn=%d (+%d) — per-attach goroutines not reaped",
			baseline, after, after-baseline)
	}
	t.Logf("goroutines baseline=%d after=%d (delta %+d)", baseline, after, after-baseline)

	// --- responsiveness / no-drift: every pane must still round-trip a fresh marker ---
	conn := attachAndHello(t, base, ws.ID)
	defer conn.Close()
	for i, p := range panes {
		marker := "SOAK_FINAL_" + string(rune('A'+i))
		_ = conn.WriteJSON(wsMsg{T: "input", Pane: p,
			Data: base64.StdEncoding.EncodeToString([]byte("echo " + marker + "\n"))})
	}
	// Collect all four markers over the shared stream.
	want := map[string]bool{}
	for i := range panes {
		want["SOAK_FINAL_"+string(rune('A'+i))] = true
	}
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	for len(want) > 0 {
		var m wsMsg
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("post-soak read (still missing %v): %v", keys(want), err)
		}
		if m.T != "output" && m.T != "snapshot" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(m.Data)
		if err != nil {
			continue
		}
		for k := range want {
			if strings.Contains(string(b), k) {
				delete(want, k)
			}
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

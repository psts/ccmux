package runner

// The stream row of the live table: a real session followed over
// GET /v1/sessions/{id}/ws by a client that does what the thread page does
// (remember the runner process and the last event number, dial again with
// them), through a daemon restart and a runner kill, both mid-turn. What it
// holds at the end must be the conversation the daemon serves, and every
// event must have arrived once and in order. Same switch and needs as
// live_test.go.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// follower is the page's side of the stream.
type follower struct {
	base, id string
	mu       sync.Mutex
	boot     string
	seq      int64
	text     map[string]string // text part id → its text as the frames built it
	streamed map[string]bool   // parts whose text came (also) by deltas
	boots    []string          // the runner process of each snapshot, in order
	problems []string
	done     chan struct{}
}

type liveFrame struct {
	T    string `json:"t"`
	Boot string `json:"boot"`
	Seq  int64  `json:"seq"`
	Data struct {
		Type       string `json:"type"`
		Properties struct {
			Part struct {
				ID, Type, Text string
			} `json:"part"`
			PartID string `json:"partID"`
			Delta  string `json:"delta"`
		} `json:"properties"`
		Messages []struct {
			Parts []struct{ ID, Type, Text string } `json:"parts"`
		} `json:"messages"`
	} `json:"data"`
}

func follow(base, id string) *follower {
	f := &follower{base: base, id: id, text: map[string]string{}, streamed: map[string]bool{}, done: make(chan struct{})}
	go f.run()
	return f
}

// run reads the stream until told to stop, dialling again from its place
// whenever the connection ends, as the page does.
func (f *follower) run() {
	for {
		select {
		case <-f.done:
			return
		default:
		}
		f.mu.Lock()
		url := fmt.Sprintf("ws%s/v1/sessions/%s/ws?boot=%s&after=%d", strings.TrimPrefix(f.base, "http"), f.id, f.boot, f.seq)
		f.mu.Unlock()
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		for {
			var fr liveFrame
			if err := conn.ReadJSON(&fr); err != nil {
				break
			}
			f.apply(fr)
		}
		conn.Close()
	}
}

func (f *follower) apply(fr liveFrame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch fr.T {
	case "snapshot":
		f.boot, f.seq = fr.Boot, fr.Seq
		f.boots = append(f.boots, fr.Boot)
		f.text, f.streamed = map[string]string{}, map[string]bool{}
		for _, m := range fr.Data.Messages {
			for _, p := range m.Parts {
				if p.Type == "text" {
					f.text[p.ID] = p.Text
				}
			}
		}
	case "event":
		if fr.Seq != f.seq+1 {
			f.problems = append(f.problems, fmt.Sprintf("event %d came after event %d", fr.Seq, f.seq))
		}
		f.seq = fr.Seq
		f.fold(fr)
	}
}

// fold applies a transcript event. A part settled with text that is not
// what its deltas added up to means a delta was lost, repeated or reordered.
func (f *follower) fold(fr liveFrame) {
	p := fr.Data.Properties
	switch fr.Data.Type {
	case "message.part.delta":
		f.text[p.PartID] += p.Delta
		f.streamed[p.PartID] = true
	case "message.part.updated":
		if p.Part.Type != "text" {
			return
		}
		if had := f.text[p.Part.ID]; f.streamed[p.Part.ID] && p.Part.Text != "" && had != p.Part.Text {
			f.problems = append(f.problems, fmt.Sprintf("part %s: its deltas added up to %d chars, it settled at %d", p.Part.ID, len(had), len(p.Part.Text)))
		}
		f.text[p.Part.ID] = p.Part.Text
	}
}

func (f *follower) has(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.text {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

func (f *follower) snapshots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.boots)
}

// differsFrom compares what the follower holds with the conversation the
// daemon serves, once it has every event that conversation has.
func (f *follower) differsFrom(b *busLive) string {
	var served liveFrame
	b.call("GET", "/v1/sessions/"+f.id+"/messages", "", nil, &served)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boot != served.Boot || f.seq != served.Seq {
		return fmt.Sprintf("the follower stands at %s/%d, the runner at %s/%d", short(f.boot), f.seq, short(served.Boot), served.Seq)
	}
	n := 0
	for _, m := range served.Data.Messages {
		for _, p := range m.Parts {
			if p.Type != "text" {
				continue
			}
			n++
			if f.text[p.ID] != p.Text {
				return fmt.Sprintf("part %s: the follower has %d chars, the runner %d", p.ID, len(f.text[p.ID]), len(p.Text))
			}
		}
	}
	if n != len(f.text) {
		return fmt.Sprintf("the follower holds %d text parts, the runner %d", len(f.text), n)
	}
	return ""
}

func (f *follower) check(t *testing.T, b *busLive, when string) {
	t.Helper()
	b.wait("the follower to hold the conversation "+when, time.Minute, func() bool { return f.differsFrom(b) == "" })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.problems) > 0 {
		t.Fatalf("%s: events out of order or lost: %v", when, f.problems)
	}
}

func liveStreamRow(t *testing.T) {
	b := startBusLive(t)
	var sess busSession
	if code := b.call("POST", "/v1/sessions", "", map[string]string{"cwd": b.repo, "model": "haiku", "kind": "lead"}, &sess); code != 201 {
		t.Fatalf("making the session: HTTP %d", code)
	}
	settled := func() bool { s, _ := b.session(sess.ID); return s.Attention == "done" }
	prompt := func(text string) {
		if code := b.call("POST", "/v1/sessions/"+sess.ID+"/prompt", "", map[string]string{"text": text}, nil); code != 202 {
			t.Fatalf("prompt: HTTP %d", code)
		}
	}
	f := follow(b.base, sess.ID)
	defer close(f.done)

	// A daemon restart in the middle of a reply: the follower dials again
	// with its numbers and goes on, with no second snapshot.
	prompt("Without using any tools, count from 1 to 400, one number per line. After the last number write the word FINISHED.")
	b.wait("the count to be streaming", 3*time.Minute, func() bool { return f.has("\n30\n") })
	b.stopDaemon()
	b.startDaemon()
	b.wait("the end of the count", 4*time.Minute, func() bool { return f.has("\n400\n") || f.has("\n400") && settled() })
	b.wait("the turn to end", 2*time.Minute, settled)
	f.check(t, b, "after the daemon restart")
	if n := f.snapshots(); n != 1 {
		t.Errorf("a daemon restart cost the follower %d snapshots, want the one it started with", n)
	}

	// The runner killed in the middle of a reply: a new process, so a new
	// snapshot, and then its events from one again.
	prompt("Without using any tools, write out every number from 1 to 500 in words, one per line. After the last one write the word FINISHED.")
	b.wait("the words to be streaming", 3*time.Minute, func() bool { return f.has("twenty-five") || f.has("twenty five") })
	unit := busPrefix + "@" + sess.ID + ".service"
	pid := mainPID(unit)
	exec.Command("systemctl", "--user", "kill", "-s", "KILL", unit).Run()
	b.wait("systemd to restart the runner", time.Minute, func() bool { p := mainPID(unit); return p != pid && p != "0" })
	b.wait("a snapshot from the new runner", 2*time.Minute, func() bool { return f.snapshots() >= 2 })
	b.wait("the interruption notice to be answered", 5*time.Minute, func() bool { return f.has("[ccmux] This session was restarted") && settled() })
	f.check(t, b, "after the runner was killed")
	f.mu.Lock()
	boots := append([]string(nil), f.boots...)
	f.mu.Unlock()
	if len(boots) < 2 || boots[0] == boots[len(boots)-1] {
		t.Errorf("the killed runner's successor is another process, so another snapshot: %v", boots)
	}
	t.Logf("STREAM followed the session through a daemon restart and a runner kill: %d snapshots, %d text parts, the same as the runner serves", len(boots), len(f.text))
	var served json.RawMessage
	if code := b.call("GET", "/v1/sessions/"+sess.ID+"/messages", "", nil, &served); code != 200 {
		t.Fatalf("messages: HTTP %d", code)
	}
}

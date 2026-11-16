package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// streamRunner is the part of a runner's socket the thread page's relay
// reads: a snapshot, numbered events, and the routes a card's answer and
// Stop go to.
type streamRunner struct {
	boot   string
	seq    int64
	events []string // SSE text written after the opening line, as the sidecar writes it
	gap    bool
	hold   chan struct{} // closed to end the event stream
	posts  []string      // "<path> <body>"
	gone   map[string]bool
}

func (r *streamRunner) serve(t *testing.T) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "rs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "r.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"boot":%q,"seq":%d,"messages":[{"info":{"id":"m1"}}]}`, r.boot, r.seq)
	})
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"server.connected\",\"properties\":{\"gap\":%v,\"seq\":%d,\"boot\":%q}}\n\n", r.gap, r.seq, r.boot)
		for _, ev := range r.events {
			io.WriteString(w, ev)
		}
		w.(http.Flusher).Flush()
		if r.hold != nil {
			<-r.hold
		}
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.posts = append(r.posts, req.URL.Path+" "+string(body))
		if r.gone[req.URL.Path] {
			w.WriteHeader(http.StatusNotFound)
		}
		w.Write([]byte("{}"))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return Dial(sock)
}

func sse(seq int64, typ string) string {
	return fmt.Sprintf("id: %d\ndata: {\"type\":%q,\"properties\":{\"n\":%d}}\n\n", seq, typ, seq)
}

func TestSnapshotSaysWhereTheEventLogStands(t *testing.T) {
	c := (&streamRunner{boot: "b1", seq: 42}).serve(t)
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Boot != "b1" || snap.Seq != 42 || !strings.Contains(string(snap.Body), `"messages"`) {
		t.Fatalf("snapshot: %+v %s", snap, snap.Body)
	}
	if _, err := (&streamRunner{}).serve(t).Snapshot(context.Background()); err == nil {
		t.Fatal("a snapshot that names no runner process must not be passed on")
	}
}

func TestEventsArriveNumberedAndInOrder(t *testing.T) {
	r := &streamRunner{boot: "b1", seq: 12, events: []string{sse(11, "a"), sse(12, "b")}}
	var got []string
	err := r.serve(t).Events(context.Background(), "b1", 10, func(ev Event) error {
		var body struct{ Type string }
		json.Unmarshal(ev.Data, &body)
		got = append(got, fmt.Sprintf("%d %s", ev.Seq, body.Type))
		return nil
	})
	if err != nil {
		t.Fatalf("a stream the runner closed is no error: %v", err)
	}
	if strings.Join(got, ",") != "11 a,12 b" {
		t.Fatalf("events: %v", got)
	}
}

func TestEventsRefuseAStreamThatCannotContinue(t *testing.T) {
	none := func(Event) error { return errors.New("no event may arrive") }
	for name, tc := range map[string]struct {
		r     *streamRunner
		boot  string
		after int64
	}{
		"the runner dropped events":           {&streamRunner{boot: "b1", seq: 9000, gap: true, events: []string{sse(8000, "x")}}, "b1", 10},
		"another runner process":              {&streamRunner{boot: "b2", seq: 50, events: []string{sse(11, "x")}}, "b1", 10},
		"a log that stands before the reader": {&streamRunner{boot: "b1", seq: 3, events: []string{sse(11, "x")}}, "b1", 10},
	} {
		if err := tc.r.serve(t).Events(context.Background(), tc.boot, tc.after, none); !errors.Is(err, ErrGap) {
			t.Errorf("%s: %v, want ErrGap", name, err)
		}
	}
}

func TestEventsEndWhenTheReaderFailsOrLeaves(t *testing.T) {
	stop := errors.New("the browser left")
	r := &streamRunner{boot: "b1", seq: 3, events: []string{sse(1, "a"), sse(2, "b")}, hold: make(chan struct{})}
	t.Cleanup(func() { close(r.hold) })
	c := r.serve(t)
	seen := 0
	err := c.Events(context.Background(), "b1", 0, func(Event) error { seen++; return stop })
	if !errors.Is(err, stop) || seen != 1 {
		t.Fatalf("the reader's error ends the stream at once: %v after %d", err, seen)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := c.Events(ctx, "b1", 2, func(Event) error { return nil }); err == nil {
		t.Fatal("a stream cut by its context ends with that")
	}
}

func TestACardAnswerGoesToItsRoute(t *testing.T) {
	r := &streamRunner{gone: map[string]bool{"/permission/gone1/reply": true}}
	c := r.serve(t)
	ctx := context.Background()
	for _, a := range []Answer{
		{Kind: "permission", ID: "abcde", Reply: "always"},
		{Kind: "question", ID: "qqqqq", Answers: [][]string{{"Red"}, {"A", "B"}}},
		{Kind: "question", ID: "rrrrr", Reject: true},
		{Kind: "question", ID: "empty"},
	} {
		if ok, err := c.AnswerCard(ctx, a); err != nil || !ok {
			t.Fatalf("%+v: %v %v", a, ok, err)
		}
	}
	want := []string{
		`/permission/abcde/reply {"reply":"always"}`,
		`/question/qqqqq/reply {"answers":[["Red"],["A","B"]]}`,
		`/question/rrrrr/reject {}`,
		`/question/empty/reply {"answers":[]}`,
	}
	if strings.Join(r.posts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("posts:\n%s", strings.Join(r.posts, "\n"))
	}
	if ok, err := c.AnswerCard(ctx, Answer{Kind: "permission", ID: "gone1", Reply: "once"}); ok || err != nil {
		t.Fatalf("a card no longer pending is not an error and not answered: %v %v", ok, err)
	}
	sent := len(r.posts)
	for _, bad := range []Answer{{Kind: "permission", ID: "abcde", Reply: "maybe"}, {Kind: "permission", Reply: "once"}, {Kind: "other", ID: "abcde"}} {
		if _, err := c.AnswerCard(ctx, bad); !errors.Is(err, ErrBadAnswer) {
			t.Errorf("%+v: %v, want ErrBadAnswer", bad, err)
		}
	}
	if len(r.posts) != sent {
		t.Fatal("an answer no card takes must not reach the runner")
	}
	if err := c.Abort(ctx, "ses1"); err != nil || r.posts[len(r.posts)-1] != "/session/ses1/abort {}" {
		t.Fatalf("stop: %v %v", err, r.posts)
	}
}

func TestTheSidecarSpeaksThisProtocol(t *testing.T) {
	// Two constants in two languages: a daemon that raised its own and
	// shipped a sidecar still saying the old one would restart every runner
	// on every pass.
	src, err := os.ReadFile("../agent/claudeserve/runnerstate.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("export const RUNNER_PROTOCOL = %d;", Protocol); !strings.Contains(string(src), want) {
		t.Fatalf("runnerstate.mjs does not say %q", want)
	}
}

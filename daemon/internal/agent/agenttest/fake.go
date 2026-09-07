// Package agenttest is a fake opencode server for tests of everything that
// talks to a running instance: enough of GET /session, the message list,
// prompt_async, abort, the permission list and reply, and the event stream.
package agenttest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// SampleMessages is one user turn and one assistant turn with a reasoning
// part, a tool call and text, in opencode's GET /session/{id}/message shape.
const SampleMessages = `[
 {"info":{"id":"msg_u1","sessionID":"ses_1","role":"user","time":{"created":1000}},
  "parts":[{"id":"prt_1","messageID":"msg_u1","sessionID":"ses_1","type":"text","text":"where is X?"}]},
 {"info":{"id":"msg_a1","sessionID":"ses_1","role":"assistant","time":{"created":1001},"error":{"name":"MessageAbortedError","data":{"message":"aborted"}}},
  "parts":[
   {"id":"prt_2","messageID":"msg_a1","type":"step-start"},
   {"id":"prt_3","messageID":"msg_a1","type":"reasoning","text":"thinking"},
   {"id":"prt_4","messageID":"msg_a1","type":"tool","tool":"grep","callID":"c1","state":{"status":"completed","input":{"pattern":"X"},"output":"a.go:1","title":"grep X"}},
   {"id":"prt_5","messageID":"msg_a1","type":"text","text":"It is in a.go:1"},
   {"id":"prt_6","messageID":"msg_a1","type":"step-finish","reason":"stop"}
  ]}
]`

// FakeOpencode is enough of the opencode server for the client: one
// session, a message list, prompt/abort/permission recording, and an event
// stream fed by the test.
type FakeOpencode struct {
	mu      sync.Mutex
	prompts []string
	aborts  int
	replies map[string]string
	answers map[string][][]string
	// Events is fed by the test: each string is one SSE data payload.
	Events   chan string
	Server   *httptest.Server
	Sessions string
}

func NewFakeOpencode() *FakeOpencode {
	f := &FakeOpencode{replies: map[string]string{}, answers: map[string][][]string{}, Events: make(chan string, 16)}
	f.Sessions = `[{"id":"ses_old","title":"old","directory":"/inst","time":{"updated":1}},{"id":"ses_1","title":"new","directory":"/inst","time":{"updated":9}}]`
	mux := http.NewServeMux()
	f.sessionRoutes(mux)
	f.requestRoutes(mux)
	f.eventRoute(mux)
	f.Server = httptest.NewServer(mux)
	return f
}

// sessionRoutes: the session list, a session's messages, prompt and abort.
func (f *FakeOpencode) sessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, f.Sessions) })
	mux.HandleFunc("GET /session/{id}/message", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, SampleMessages) })
	mux.HandleFunc("POST /session/{id}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []struct{ Type, Text string } `json:"parts"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.prompts = append(f.prompts, r.PathValue("id")+":"+body.Parts[0].Text)
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /session/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.aborts++
		f.mu.Unlock()
		w.WriteHeader(200)
	})
}

// requestRoutes: permission and question listing and replies.
func (f *FakeOpencode) requestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /permission", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"per_1","sessionID":"ses_1","permission":"bash","patterns":["ls *"],"metadata":{},"always":["*"]}]`)
	})
	mux.HandleFunc("POST /permission/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Reply string }
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.replies[r.PathValue("id")] = body.Reply
		f.mu.Unlock()
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /question", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":"que_1","sessionID":"ses_1","questions":[{"question":"Pick one.","header":"Demo","options":[{"label":"Tea","description":"calm"},{"label":"Coffee","description":"alert"}]}]}]`)
	})
	mux.HandleFunc("POST /question/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Answers [][]string `json:"answers"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.answers[r.PathValue("id")] = body.Answers
		f.mu.Unlock()
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /question/{id}/reject", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.answers[r.PathValue("id")] = nil
		f.mu.Unlock()
		w.WriteHeader(200)
	})
}

// eventRoute: the SSE stream, fed from Events.
func (f *FakeOpencode) eventRoute(mux *http.ServeMux) {
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-f.Events:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", ev)
				fl.Flush()
			}
		}
	})
}

// Port is the fake's listening port, for a startup line's --port.
func (f *FakeOpencode) Port() int {
	_, p, _ := net.SplitHostPort(strings.TrimPrefix(f.Server.URL, "http://"))
	n, _ := strconv.Atoi(p)
	return n
}

// Recorded is what the client sent: prompts as "session:text", the abort
// count, and permission replies by request id.
func (f *FakeOpencode) Recorded() (prompts []string, aborts int, replies map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	replies = map[string]string{}
	for k, v := range f.replies {
		replies[k] = v
	}
	return append([]string(nil), f.prompts...), f.aborts, replies
}

// Answers is what the client answered per question request (nil = rejected).
func (f *FakeOpencode) Answers() map[string][][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][][]string{}
	for k, v := range f.answers {
		out[k] = v
	}
	return out
}

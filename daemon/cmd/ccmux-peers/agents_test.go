package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The rendering lives in internal/buscontext and is tested there; this pins
// that the shim carries both lists and says nothing outside a window.
func TestWindowParagraphAndSectionWithoutADaemon(t *testing.T) {
	a := &app{}
	if a.windowParagraph() != "" || a.windowSection() != "" {
		t.Fatal("no daemon: nothing appended")
	}
	if !strings.Contains(strings.ToLower(serverInstructions), "peer") {
		t.Fatal("frozen instructions missing")
	}
}

// The daemon says who is reading; the shim must hand that on to the text.
// A role agent gets the repo folders and may read there; a repo session
// gets neither, sees itself marked, and is told to hand the work over.
func TestWindowParagraphFollowsTheReader(t *testing.T) {
	reader := `{"agent":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers/sessions":
			fmt.Fprintf(w, `{"sessions":[{"name":"backend","repoPath":"/r/backend","owns":"API","status":"live"},`+
				`{"name":"frontend","repoPath":"/r/frontend","status":"live"}],"shared":"","reader":%s}`, reader)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()
	a := &app{daemon: newDaemonClient(srv.URL, "t"), id: "p1"}

	for _, text := range []string{a.windowParagraph(), a.windowSection()} {
		if !strings.Contains(text, "/r/backend") || !strings.Contains(text, "owns API") {
			t.Fatalf("a role agent gets the folders and the owns lines:\n%s", text)
		}
	}
	if !strings.Contains(a.windowParagraph(), "read there") {
		t.Fatal("a role agent may read the repos it reaches")
	}

	reader = `{"agent":false,"self":"frontend"}`
	for _, text := range []string{a.windowParagraph(), a.windowSection()} {
		if strings.Contains(text, "/r/") || !strings.Contains(text, "frontend (you)") || !strings.Contains(text, "delegate") {
			t.Fatalf("a repo session gets no folders, itself marked, and the hand-over rule:\n%s", text)
		}
	}
}

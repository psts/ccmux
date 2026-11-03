package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

func scratchReq(t *testing.T, s *Server, method, wsID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, "/v1/workspaces/"+wsID+"/scratchpad", strings.NewReader(body))
	r.SetPathValue("id", wsID)
	if method == http.MethodPut {
		s.putScratchpad(rec, r)
	} else {
		s.getScratchpad(rec, r)
	}
	return rec
}

func scratchBody(t *testing.T, rec *httptest.ResponseRecorder) model.Scratchpad {
	t.Helper()
	var pad model.Scratchpad
	if err := json.Unmarshal(rec.Body.Bytes(), &pad); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
	return pad
}

// One copy per workspace, the same for every caller, and the saver named.
func TestScratchpad_SharedAcrossCallers(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true},
		&model.Workspace{ID: "w1"}, &model.Workspace{ID: "w2"})

	if got := scratchBody(t, scratchReq(t, s, http.MethodGet, "w1", "")); got.Content != "" {
		t.Fatalf("unwritten pad reads %q, want empty", got.Content)
	}
	if rec := scratchReq(t, s, http.MethodPut, "w1", `{"content":"to do: ship"}`); rec.Code != http.StatusOK {
		t.Fatalf("put = %d (%s)", rec.Code, rec.Body)
	}
	s.identity = fakeResolver{login: "dasha@x.com", ok: true}
	got := scratchBody(t, scratchReq(t, s, http.MethodGet, "w1", ""))
	if got.Content != "to do: ship" || got.UpdatedBy != "patric@x.com" || got.UpdatedAt == 0 {
		t.Fatalf("dasha reads %+v, want patric's text and name", got)
	}
	if other := scratchBody(t, scratchReq(t, s, http.MethodGet, "w2", "")); other.Content != "" {
		t.Fatalf("w2 reads %q: one workspace's notes leaked into another", other.Content)
	}
}

func TestScratchpad_RejectsWithoutWriting(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, &model.Workspace{ID: "w1"})
	if rec := scratchReq(t, s, http.MethodPut, "w1", `{"content":"keep me"}`); rec.Code != http.StatusOK {
		t.Fatalf("seed put = %d (%s)", rec.Code, rec.Body)
	}
	cases := []struct {
		name, ws, body string
		code           int
	}{
		{"unknown workspace", "nope", `{"content":"x"}`, http.StatusNotFound},
		{"no content field", "w1", `{}`, http.StatusBadRequest},
		{"not JSON", "w1", `content=x`, http.StatusBadRequest},
		{"over a megabyte", "w1", `{"content":"` + strings.Repeat("a", maxScratchpad+1) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := scratchReq(t, s, http.MethodPut, c.ws, c.body); rec.Code != c.code {
				t.Fatalf("code = %d (%s), want %d", rec.Code, rec.Body, c.code)
			}
			if got := scratchBody(t, scratchReq(t, s, http.MethodGet, "w1", "")); got.Content != "keep me" {
				t.Fatalf("a refused put changed the pad to %q", got.Content)
			}
		})
	}
	if rec := scratchReq(t, s, http.MethodGet, "nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("get unknown = %d, want 404", rec.Code)
	}
	// Clearing is a save, not a mistake.
	if rec := scratchReq(t, s, http.MethodPut, "w1", `{"content":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d (%s)", rec.Code, rec.Body)
	}
	if got := scratchBody(t, scratchReq(t, s, http.MethodGet, "w1", "")); got.Content != "" {
		t.Fatalf("cleared pad reads %q", got.Content)
	}
}

// A save tells every lens which workspace's notes changed; a refused one is
// silent.
func TestScratchpad_SaveIsAnnounced(t *testing.T) {
	s := windowsFixture(t, fakeResolver{login: "patric@x.com", ok: true}, &model.Workspace{ID: "w1"})
	_, events := s.mgr.SubscribeEvents()
	_ = scratchReq(t, s, http.MethodPut, "w1", `{}`)
	_ = scratchReq(t, s, http.MethodPut, "w1", `{"content":"x"}`)
	ev := <-events
	if ev.Kind != "workspace-scratchpad" || ev.WorkspaceID != "w1" {
		t.Fatalf("event = %+v, want workspace-scratchpad for w1 (and none for the refused put)", ev)
	}
	select {
	case extra := <-events:
		t.Fatalf("unexpected second event %+v", extra)
	default:
	}
}

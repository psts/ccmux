package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/model"
)

// sidebarDo sends one request through the whole mux as user, so the route
// patterns and the identity tier are both under test.
func sidebarDo(t *testing.T, s *Server, method, path, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path+"?user="+user, strings.NewReader(body)))
	return rec
}

// sidebarOf renders a user's served prefs as "id:collapsed:position ",
// with the window ids replaced by their names.
func sidebarOf(t *testing.T, s *Server, user string, names map[string]string) string {
	t.Helper()
	rec := sidebarDo(t, s, "GET", "/v1/me/sidebar", user, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET as %s = %d (%s)", user, rec.Code, rec.Body)
	}
	var got struct {
		Groups []struct {
			WindowID  string `json:"windowId"`
			Collapsed bool   `json:"collapsed"`
			Position  *int   `json:"position"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	out := ""
	for _, g := range got.Groups {
		pos := "-"
		if g.Position != nil {
			pos = fmt.Sprint(*g.Position)
		}
		out += fmt.Sprintf("%s:%v:%s ", names[g.WindowID], g.Collapsed, pos)
	}
	return out
}

func sidebarFixture(t *testing.T) (*Server, map[string]string, map[string]string) {
	t.Helper()
	s := windowsFixture(t, fakeResolver{ok: false}, &model.Workspace{ID: "w1"}, &model.Workspace{ID: "w2"}, &model.Workspace{ID: "w3"})
	ids, names := map[string]string{}, map[string]string{}
	for ws, name := range map[string]string{"w1": "ALPHA", "w2": "BETA", "w3": "DASHA"} {
		_ = putGroupReq(t, s, ws, name)
		id, _ := s.mgr.WindowByName(name)
		ids[name], names[id] = id, name
	}
	return s, ids, names
}

// Patric folds DASHA and puts it last; Dasha's sidebar is untouched by
// both writes, and her own write leaves his alone.
func TestSidebarPrefs_PerPerson(t *testing.T) {
	s, ids, names := sidebarFixture(t)
	if rec := sidebarDo(t, s, "PUT", "/v1/me/sidebar/"+ids["DASHA"], "patric", `{"collapsed":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("collapse = %d (%s)", rec.Code, rec.Body)
	}
	order := fmt.Sprintf(`{"windowIds":[%q,%q,%q]}`, ids["BETA"], ids["ALPHA"], ids["DASHA"])
	if rec := sidebarDo(t, s, "PUT", "/v1/me/sidebar/order", "patric", order); rec.Code != http.StatusNoContent {
		t.Fatalf("order = %d (%s)", rec.Code, rec.Body)
	}
	if got := sidebarOf(t, s, "dasha", names); got != "" {
		t.Fatalf("dasha sees %q after patric's writes, want nothing", got)
	}
	if rec := sidebarDo(t, s, "PUT", "/v1/me/sidebar/order", "dasha", fmt.Sprintf(`{"windowIds":[%q]}`, ids["DASHA"])); rec.Code != http.StatusNoContent {
		t.Fatalf("dasha's order = %d (%s)", rec.Code, rec.Body)
	}
	if got, want := sidebarOf(t, s, "patric", names), "BETA:false:0 ALPHA:false:1 DASHA:true:2 "; got != want {
		t.Fatalf("patric = %q, want %q", got, want)
	}
	if got, want := sidebarOf(t, s, "dasha", names), "DASHA:false:0 "; got != want {
		t.Fatalf("dasha = %q, want %q", got, want)
	}
	if rec := sidebarDo(t, s, "PUT", "/v1/me/sidebar/"+ids["DASHA"], "patric", `{"collapsed":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("expand = %d (%s)", rec.Code, rec.Body)
	}
	if got, want := sidebarOf(t, s, "patric", names), "BETA:false:0 ALPHA:false:1 DASHA:false:2 "; got != want {
		t.Fatalf("patric after expand = %q, want %q", got, want)
	}
}

// Bad bodies are a 400 and an unknown window a 404, and none of them
// stores anything; an id in the order that is no window is skipped.
func TestSidebarPrefs_Rejects(t *testing.T) {
	s, ids, names := sidebarFixture(t)
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/v1/me/sidebar/" + ids["ALPHA"], `{}`, http.StatusBadRequest},
		{"/v1/me/sidebar/" + ids["ALPHA"], `{"collapsed":"yes"}`, http.StatusBadRequest},
		{"/v1/me/sidebar/" + ids["ALPHA"], `not json`, http.StatusBadRequest},
		{"/v1/me/sidebar/no-such-window", `{"collapsed":true}`, http.StatusNotFound},
		{"/v1/me/sidebar/order", `{}`, http.StatusBadRequest},
		{"/v1/me/sidebar/order", `{"windowIds":"x"}`, http.StatusBadRequest},
		{"/v1/me/sidebar/order", `{"windowIds":[""]}`, http.StatusBadRequest},
		{"/v1/me/sidebar/order", fmt.Sprintf(`{"windowIds":[%q,%q]}`, ids["ALPHA"], ids["ALPHA"]), http.StatusBadRequest},
	} {
		if rec := sidebarDo(t, s, "PUT", c.path, "patric", c.body); rec.Code != c.code {
			t.Errorf("PUT %s %s = %d, want %d (%s)", c.path, c.body, rec.Code, c.code, rec.Body)
		}
	}
	if got := sidebarOf(t, s, "patric", names); got != "" {
		t.Fatalf("rejected writes stored %q", got)
	}
	if rec := sidebarDo(t, s, "PUT", "/v1/me/sidebar/order", "patric", fmt.Sprintf(`{"windowIds":["gone",%q]}`, ids["BETA"])); rec.Code != http.StatusNoContent {
		t.Fatalf("order naming a deleted window = %d (%s)", rec.Code, rec.Body)
	}
	if got, want := sidebarOf(t, s, "patric", names), "BETA:false:1 "; got != want {
		t.Fatalf("patric = %q, want %q", got, want)
	}
}

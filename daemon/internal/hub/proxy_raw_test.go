package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The member fetch must carry raw=1: without it the member stamps per-caller
// views onto the list, and the hub's one-shot legacy-group import would read a
// stamped value instead of the persisted column, corrupting the migration.
func TestWorkspaces_FetchesRaw(t *testing.T) {
	var gotQuery string
	member := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"w1"}]`))
	}))
	defer member.Close()

	c := NewClient(member.Client().Transport)
	wss, err := c.Workspaces(context.Background(), Host{ID: "m", Addr: strings.TrimPrefix(member.URL, "https://")})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(wss) != 1 || wss[0].ID != "w1" {
		t.Fatalf("decoded %+v", wss)
	}
	if gotQuery != "raw=1" {
		t.Fatalf("member fetch query = %q, want raw=1 (unstamped list)", gotQuery)
	}
}

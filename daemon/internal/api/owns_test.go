package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/peers"
	"ccmux.dev/ccmuxd/internal/repoowns"
)

// A repo's line reaches every session's context: the summary once it is
// written, a typed line over it, and the summary again when that is
// cleared. The routes answer the lens's editor; an agent's own session
// owns no repo.
func TestOwns_RoutesAndTheSessionsList(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	// The fixture's repo is /tmp: seed its summary as stored, with the hash
	// of what is there, so nothing is summarized (repoowns tests that).
	_, hash := repoowns.Source("/tmp")
	if err := f.st.SetRepoOwnsAuto("/tmp", "flood alerts, gauges", hash, 1); err != nil {
		t.Fatal(err)
	}
	svc := repoowns.New(f.st, func(context.Context, string, int) (string, error) {
		t.Error("nothing changed; the model must not be asked")
		return "", nil
	})
	f.srv.SetOwns(svc, f.st)
	f.srv.EnablePeers(peers.NewService(f.st, f.srv.mgr, testSecret))

	sessionsOwns := func() string {
		pane := f.ws.Panes[0].ID
		tok := peers.TokenForPane(testSecret, pane)
		nextFakePID++
		resp := postJSON(t, f.base+"/v1/peers/register", tok, map[string]any{"pane_id": pane, "pid": nextFakePID, "cwd": "/tmp", "git_root": "/tmp"})
		var reg struct {
			PeerID string `json:"peer_id"`
		}
		json.NewDecoder(resp.Body).Decode(&reg)
		resp = postJSON(t, f.base+"/v1/peers/sessions", tok, map[string]any{"peer_id": reg.PeerID})
		var got struct {
			Sessions []map[string]any `json:"sessions"`
		}
		json.NewDecoder(resp.Body).Decode(&got)
		if len(got.Sessions) != 1 {
			t.Fatalf("sessions = %v", got.Sessions)
		}
		owns, _ := got.Sessions[0]["owns"].(string)
		return owns
	}
	if got := sessionsOwns(); got != "flood alerts, gauges" {
		t.Fatalf("the list carries the stored summary: %q", got)
	}
	route := "/v1/workspaces/" + f.ws.ID + "/owns"
	if rec := do(t, f.srv, "PUT", route, `{"manual":"  flood alerts,\n gauges and maps "}`); rec.Code != 200 ||
		!strings.Contains(rec.Body.String(), `"line":"flood alerts, gauges and maps"`) || !strings.Contains(rec.Body.String(), `"auto":"flood alerts, gauges"`) {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if got := sessionsOwns(); got != "flood alerts, gauges and maps" {
		t.Fatalf("a typed line wins: %q", got)
	}
	do(t, f.srv, "PUT", route, `{"manual":""}`)
	if rec := do(t, f.srv, "GET", route, ""); !strings.Contains(rec.Body.String(), `"line":"flood alerts, gauges"`) {
		t.Fatalf("clearing it shows the summary again: %s", rec.Body)
	}
	if rec := do(t, f.srv, "PUT", route, `{"manual":"`+strings.Repeat("x", 201)+`"}`); rec.Code != 400 {
		t.Fatalf("a long line = %d, want 400", rec.Code)
	}
	if rec := do(t, f.srv, "GET", "/v1/workspaces/nope/owns", ""); rec.Code != 404 {
		t.Fatalf("unknown workspace = %d", rec.Code)
	}
	_, agentWS := f.start(t, "x-poster", "")
	if rec := do(t, f.srv, "GET", "/v1/workspaces/"+agentWorkspaceID(t, f, agentWS.ID)+"/owns", ""); rec.Code != 400 {
		t.Fatalf("an agent's session = %d, want 400", rec.Code)
	}
}

func agentWorkspaceID(t *testing.T, f *windowAgentFixture, paneID string) string {
	t.Helper()
	id := f.srv.mgr.WorkspaceForPane(paneID)
	if id == "" {
		t.Fatal("agent pane has no workspace")
	}
	return id
}

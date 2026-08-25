package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A create that OMITS startupCommand gets a bare shell, exactly like an
// explicit "". No daemon-side default is typed into the pane — the lens
// preselects a harness via resolvedHarness instead (empty-with-preselect).
// This pins the retirement of the old omit-means-configured-default contract.
func TestCreateWorkspace_OmittedStartupCommandIsBareShell(t *testing.T) {
	_, base := floodStack(t, "ccmux-omitcreate-itest")

	body := `{"name":"omit","repoPath":"/tmp","createdBy":"tester"}`
	resp, err := http.Post(base+"/v1/workspaces", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d, want 201", resp.StatusCode)
	}
	var ws struct {
		Panes []struct {
			StartupCommand string `json:"startupCommand"`
			Harness        string `json:"harness"`
		} `json:"panes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ws); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ws.Panes) != 1 || ws.Panes[0].StartupCommand != "" || ws.Panes[0].Harness != "" {
		t.Fatalf("panes = %+v, want one bare-shell pane with no harness", ws.Panes)
	}
}

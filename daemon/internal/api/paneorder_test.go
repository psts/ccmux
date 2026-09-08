package api

import "testing"

func TestPutPaneOrder_Validation(t *testing.T) {
	s := settingsServer(t)
	if rec := do(t, s, "PUT", "/v1/workspaces/ws-x/pane-order", `{"order":[]}`); rec.Code != 400 {
		t.Errorf("empty order = %d, want 400", rec.Code)
	}
	if rec := do(t, s, "PUT", "/v1/workspaces/ws-x/pane-order", `nope`); rec.Code != 400 {
		t.Errorf("bad json = %d, want 400", rec.Code)
	}
	if rec := do(t, s, "PUT", "/v1/workspaces/ws-x/pane-order", `{"order":["p1"]}`); rec.Code != 404 {
		t.Errorf("unknown workspace = %d, want 404", rec.Code)
	}
}

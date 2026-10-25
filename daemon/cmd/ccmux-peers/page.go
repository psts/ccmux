package main

import (
	"encoding/json"
	"fmt"
)

// toolPage sends the page tool's one push through the pane's daemon route
// (POST /v1/panes/{id}/page), which keeps the one-per-incident rule.
func (a *app) toolPage(args json.RawMessage) any {
	if a.paneID == "" {
		return toolText("Paging needs a ccmux pane; this session has none.", true)
	}
	var in struct {
		Incident string `json:"incident"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	var resp struct {
		Sent int `json:"sent"`
	}
	if err := a.daemon.post("/v1/panes/"+a.paneID+"/page", in, &resp); err != nil {
		return toolText("Page not sent: "+err.Error(), true)
	}
	return toolText(fmt.Sprintf("Paged: %d device(s) got it. Do not page again for incident %q.", resp.Sent, in.Incident), false)
}

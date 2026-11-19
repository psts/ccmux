package main

import "encoding/json"

// threadToolText is the thread tool's whole description. It is short on
// purpose: every session carries it from its first turn, and only a
// thread's lead ever calls it. What a thread is, and when to merge, is in
// the lead's own brief (the daemon's internal/threads).
const threadToolText = "For the lead of a ccmux thread (a session whose brief says it leads one), about its own thread and its helpers, " +
	"the sessions it stood up in other repos by messaging those repos by name. Actions: helpers (who they are, where each works, what its branch holds), " +
	"stand_down (end the helper for repo), merge (land every member's branch: each repo's checks run, all land or none; it takes minutes and the result comes as a message), " +
	"stop_merge (stop a merge that is running), end (stand every helper down and close the thread). Any other session is refused."

// toolThread hands the call to the daemon's pane route, which knows whose
// thread the pane's session leads; the shim adds nothing but the identity.
func (a *app) toolThread(args json.RawMessage) any {
	if a.paneID == "" {
		return toolText("The thread tool needs a ccmux session; this one has no pane.", true)
	}
	var in struct {
		Action string `json:"action"`
		Repo   string `json:"repo"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return toolText("Bad arguments: "+err.Error(), true)
	}
	var resp struct {
		Message string `json:"message"`
	}
	if err := a.daemon.post("/v1/panes/"+a.paneID+"/thread", in, &resp); err != nil {
		return toolText("Thread: "+err.Error(), true)
	}
	return toolText(resp.Message, false)
}

package api

import (
	"encoding/json"
	"fmt"
	"log"

	"ccmux.dev/ccmuxd/internal/agent"
)

// historyBudget is the most bytes of turns a chat hello carries. The hello
// goes out as one WebSocket frame, and the Mac lens fails any frame over
// WebSocketPump.maxMessageBytes (64 MiB): the receive errors, the pump
// reconnects, gets the same hello, and loops on "reconnecting" for good.
// That happened for real at URLSession's old 1 MiB default (radar's hello,
// 1.28 MB, 2026-10-02). The history is bounded by conversation count
// (historySessions), not by size, so without this a busy agent could grow
// past any client cap. 8 MiB is about five times the largest hello seen and
// well under the Mac cap; the browser has no cap, and both lenses get the
// same trimmed hello, so they still show the same chat.
const historyBudget = 8 << 20

// historyTrimmedID is the id of the divider capHistory puts where it cut.
const historyTrimmedID = "session:trimmed"

// capHistory keeps the newest turns that fit in budget bytes of JSON and,
// when it drops any, leads with a session-role divider saying so. Both
// lenses already draw a role "session" turn as a divider, so the cut needs
// no lens code. The newest turn always stays, even alone over the budget:
// a chat that opens empty is worse than one large frame.
func capHistory(turns []agent.Turn, budget int) []agent.Turn {
	start := len(turns)
	total := 1 // the array's opening bracket; each turn adds its comma or closing bracket
	for i := len(turns) - 1; i >= 0; i-- {
		n := turnBytes(turns[i]) + 1
		if total+n > budget && i < len(turns)-1 {
			break
		}
		total += n
		start = i
	}
	if start == 0 {
		return turns
	}
	marker := agent.Turn{
		ID:    historyTrimmedID,
		Role:  "session",
		Time:  turns[start].Time, // a 0 would leave a dangling " · " in both lenses
		Parts: []agent.TurnPart{},
		Title: fmt.Sprintf("Older messages not shown: this chat's history is over %d MB", budget>>20),
	}
	return append([]agent.Turn{marker}, turns[start:]...)
}

// turnBytes is the turn's size as the hello will encode it. A turn that
// cannot encode counts as 0 and is logged here, because nothing downstream
// says so: the hello's own encode then fails too, and the socket writer
// and writeJSON both drop that error without a line.
func turnBytes(t agent.Turn) int {
	b, err := json.Marshal(t)
	if err != nil {
		log.Printf("agent chat: turn %s will not encode, so the hello carrying it will fail: %v", t.ID, err)
	}
	return len(b)
}

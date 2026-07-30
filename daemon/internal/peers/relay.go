// Permission relay: when a worker's Claude Code opens a tool-approval dialog,
// the request fans out (as normal peer messages, wording preserved verbatim
// from the old claude-peers server — sessions are trained on it) to everyone
// who messaged the worker in the last ten minutes. A delegator's "yes <id>"
// reply comes back through Send, where tryVerdictLocked converts it into a
// structured permission_verdict event for the worker. The local terminal
// dialog stays open as fallback; the first valid verdict wins.
package peers

import (
	"fmt"
	"strings"
)

// PermissionRequest records an outstanding tool-approval request for a worker
// and relays it to the worker's recent senders. Returns how many peers it
// reached. The broadcast set is computed from the event log (not stored), so
// it survives daemon and thin-client restarts alike.
func (s *Service) PermissionRequest(workerID, requestID, toolName, description, inputPreview string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker := s.peers[workerID]
	if worker == nil {
		return 0, fmt.Errorf("peer %s is not registered", workerID)
	}
	s.prunePermsLocked()
	rid := strings.ToLower(requestID)
	pr := &permRequest{workerID: workerID, at: s.Now().UnixMilli()}
	s.perms[rid] = pr
	// Written through, because the dialog this tracks can stay open for hours and
	// a daemon restart in between used to orphan it: the verdict stopped matching
	// and arrived at the worker as ordinary chat while it waited.
	_ = s.st.SavePermRequest(rid, pr.workerID, pr.resolved, pr.at)

	senders, err := s.st.RecentPeerSenders(workerID, s.Now().Add(-recentSenderWindow).UnixMilli())
	if err != nil {
		return 0, err
	}
	group := s.groupOfLocked(worker)
	text := relayText(worker.Name, requestID, toolName, description, inputPreview)
	relayed := 0
	for _, sid := range senders {
		target := s.peers[sid]
		if target == nil || sid == workerID {
			continue
		}
		if s.deliverLocked(s.eventFromLocked(worker, target, group, text)) == nil {
			relayed++
		}
	}
	return relayed, nil
}

// relayText is the exact wording the old server.ts broadcast — recipients'
// instructions reference it ("starting with [claude-peers permission relay]").
func relayText(workerName, requestID, toolName, description, inputPreview string) string {
	return "[claude-peers permission relay]\n" +
		fmt.Sprintf("Peer %q needs approval to run %s: %s\n", workerName, toolName, description) +
		fmt.Sprintf("Args: %s\n\n", inputPreview) +
		fmt.Sprintf("Reply with send_message: message exactly \"yes %s\" to approve, \"no %s\" to deny.\n", requestID, requestID) +
		fmt.Sprintf("Only respond if you actually delegated this work to %q. If you didn't, ignore this — don't reply yes or no.", workerName)
}

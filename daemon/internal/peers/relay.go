// Card relay: when a worker's Claude Code opens a tool-approval dialog, or a
// chat-served agent raises a permission or question card, the request fans
// out (as normal peer messages; the permission wording preserved verbatim
// from the old claude-peers server — sessions are trained on it) to everyone
// who messaged the worker in the last ten minutes. A delegator's "yes <id>"
// or "answer <id> <text>" reply comes back through Send, where the verdict
// and answer matchers convert it into a structured event for the worker and,
// for a pane with a chat server, into a reply on its card. The local dialog
// or card stays open as fallback; the first valid reply wins.
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
	return s.relayAsk(workerID, requestID, askPermission, func(workerName string) string {
		return relayText(workerName, requestID, toolName, description, inputPreview)
	})
}

// QuestionRequest is PermissionRequest for a question card: text is the
// card as the agent's chat shows it (the questions and their options), and
// a delegator answers with "answer <id> <text>".
func (s *Service) QuestionRequest(workerID, requestID, text string) (int, error) {
	return s.relayAsk(workerID, requestID, askQuestion, func(workerName string) string {
		return questionRelayText(workerName, requestID, text)
	})
}

// relayAsk records the ask of that kind under requestID and fans text
// (rendered with the worker's name) to the worker's recent senders.
func (s *Service) relayAsk(workerID, requestID, kind string, text func(workerName string) string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker := s.peers[workerID]
	if worker == nil {
		return 0, fmt.Errorf("peer %s is not registered", workerID)
	}
	s.prunePermsLocked()
	rid := strings.ToLower(requestID)
	pr := &permRequest{workerID: workerID, kind: kind, at: s.Now().UnixMilli()}
	s.perms[rid] = pr
	// Written through, because the dialog this tracks can stay open for hours and
	// a daemon restart in between used to orphan it: the verdict stopped matching
	// and arrived at the worker as ordinary chat while it waited.
	_ = s.st.SavePermRequest(rid, pr.workerID, pr.kind, pr.resolved, pr.at)

	senders, err := s.st.RecentPeerSenders(workerID, s.Now().Add(-recentSenderWindow).UnixMilli())
	if err != nil {
		return 0, err
	}
	group := s.groupOfLocked(worker)
	body := text(worker.Name)
	relayed := 0
	for _, sid := range senders {
		target := s.peers[sid]
		if target == nil || sid == workerID {
			continue
		}
		if s.deliverLocked(s.eventFromLocked(worker, target, group, body)) == nil {
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

// questionRelayText is the question card's wording; the shim's instructions
// name its opening line and the reply form.
func questionRelayText(workerName, requestID, text string) string {
	return "[claude-peers question relay]\n" +
		fmt.Sprintf("Peer %q asks:\n%s\n\n", workerName, strings.TrimSpace(text)) +
		fmt.Sprintf("Reply with send_message: message \"answer %s <your answer>\" — the id, then your answer in plain words (an option's label, or your own text).\n", requestID) +
		fmt.Sprintf("Only answer if you actually delegated this work to %q. If you didn't, ignore this — don't reply.", workerName)
}

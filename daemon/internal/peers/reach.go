// Who may message whom. The default is unchanged and deliberately narrow — your
// own group — because that is what keeps teammate names unambiguous and stops a
// stale peer id from wandering into another project. Crossing the boundary is
// possible but never accidental: the sender has to name the group it is
// reaching into, which is exactly how a human phrases the instruction ("check in
// with backend in ChartLabs"), and the crossing opens a return path so the
// recipient can answer normally.
package peers

import "time"

// replyGrantTTL bounds how long a cross-group message licenses a reply. Long
// enough for a teammate to finish a task and report back, short enough that the
// boundary is not permanently dissolved by one message.
const replyGrantTTL = 2 * time.Hour

// checkReachableLocked returns "" when the sender may message the target, or
// the tool-facing error explaining why not.
func (s *Service) checkReachableLocked(req SendReq, sender *Peer, senderGroup string, target *Peer) string {
	targetGroup := s.groupOfLocked(target)
	if sameGroup(targetGroup, senderGroup) {
		return ""
	}
	if req.ToGroup != "" && sameGroup(req.ToGroup, targetGroup) {
		return "" // explicitly addressed into that group
	}
	if s.hasReplyGrantLocked(sender.ID, target.ID) {
		return "" // answering someone who reached into our group first
	}
	// The historic sentence stays the prefix — running sessions pattern-match on
	// it — but it can no longer stand alone: crossing is now possible, and an
	// error that only says "cannot" would teach the opposite of the truth.
	return "Cannot send messages across projects — peer " + target.ID +
		" is in \"" + targetGroup + "\", you are in \"" + senderGroup +
		"\". Pass to_group=\"" + targetGroup + "\" to reach it."
}

// grantReplyLocked lets `to` answer `from` across a group boundary for a while.
// Written through, because a daemon restart inside the two-hour window used to
// revoke the return path mid-conversation: the teammate that was reached into
// then got "cannot send messages across projects" when it tried to answer.
func (s *Service) grantReplyLocked(to, from string) {
	exp := s.Now().Add(replyGrantTTL).UnixMilli()
	s.replyGrants[to+"\x00"+from] = exp
	_ = s.st.SaveReplyGrant(to, from, exp)
}

// hasReplyGrantLocked reports whether `from` may answer `to` because `to`
// messaged it across the boundary recently. Expired grants are dropped as they
// are found, which keeps the map bounded without a sweeper.
func (s *Service) hasReplyGrantLocked(from, to string) bool {
	key := from + "\x00" + to
	exp, ok := s.replyGrants[key]
	if !ok {
		return false
	}
	if s.Now().UnixMilli() >= exp {
		delete(s.replyGrants, key)
		_ = s.st.DeleteReplyGrant(from, to)
		return false
	}
	return true
}

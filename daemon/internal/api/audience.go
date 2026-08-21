package api

import "time"

// Notification routing: an attention event belongs to the people working in
// that repo, not to everyone at a screen. Before this, alertsFor asked only
// "is this reader present anywhere", which was invisible with one person and
// wrong with two — every repo's notification landed on every Mac.

// driverRecency is how long one keystroke makes someone THE person a repo's
// notifications go to. Long enough to cover an agent run started and walked
// away from (the same order as the 30-minute subagent hold); after it lapses,
// routing widens to everyone holding the window open.
const driverRecency = 30 * time.Minute

// alertAudience returns the logins a workspace's attention should reach, and
// whether that set is BOUNDED. The ladder: someone typed there within
// driverRecency → theirs alone ("the person currently working on that repo");
// else everyone with the workspace's window open (their working set, and
// closing the window mutes it). A workspace in no window, a window nobody has
// open, or unknowable window state is UNBOUNDED — everyone, the
// pre-multi-user behavior — because a mis-route that silences a needs-input
// is worse than a spare notification.
func (s *Server) alertAudience(wsID string) (map[string]bool, bool) {
	if login, at, ok := s.focus.DriverLogin(wsID); ok &&
		time.Since(time.UnixMilli(at)) < driverRecency {
		return map[string]bool{login: true}, true
	}
	logins, grouped := s.mgr.WindowOpenLogins(wsID)
	if !grouped || len(logins) == 0 {
		return nil, false
	}
	return logins, true
}

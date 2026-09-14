package api

// Notification routing: an attention event belongs to the people working in
// that repo, not to everyone at a screen. Before this, alertsFor asked only
// "is this reader present anywhere", which was invisible with one person and
// wrong with two — every repo's notification landed on every Mac.

// alertAudience returns the logins a workspace's attention should reach, and
// whether that set is BOUNDED. The ladder: the last person who typed there →
// theirs alone, for as long as nobody else types (remembered after their lens
// is gone and across restarts: manager.LastDriver); else everyone with the
// workspace's window open (their working set, and closing the window mutes
// it). A workspace in no window, a window nobody has open, or unknowable
// window state is UNBOUNDED — everyone, the pre-multi-user behavior — because
// a mis-route that silences a needs-input is worse than a spare notification.
//
// There used to be a 30-minute recency on the driver rung, after which routing
// widened to the window holders. Combined with drivers being forgotten the
// moment their lens disconnected, that sent a repo Dasha was working in to
// Patric's phone because he had opened her window from the web lens once.
// The person who last typed in a repo is its person until someone else types;
// wanting its alerts is a matter of typing there, not of having peeked.
func (s *Server) alertAudience(wsID string) (map[string]bool, bool) {
	if login, _, ok := s.focus.DriverLogin(wsID); ok {
		return map[string]bool{login: true}, true
	}
	logins, grouped := s.mgr.WindowOpenLogins(wsID)
	if !grouped || len(logins) == 0 {
		return nil, false
	}
	return logins, true
}

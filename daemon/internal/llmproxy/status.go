package llmproxy

import "time"

// AccountStatus is one row of the settings Accounts tab: the account joined
// with what the proxy has learned from its traffic. Usage percentages are
// -1 until an upstream that reports them (Anthropic subscriptions) has
// answered through the proxy at least once.
type AccountStatus struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// State: "ok" (usable), "limited" (out of quota until LimitedUntil),
	// "unreachable" (the upstream did not answer; LastError says how),
	// "failing" (it answered, with an error; LastError says which),
	// "unauthorized" (credential rejected), "untried" (no traffic seen yet).
	State        string `json:"state"`
	LimitedUntil string `json:"limitedUntil,omitempty"`
	// LastError is carried only while the account is CURRENTLY unreachable or
	// failing. An account that has recovered reads as active, and pairing
	// "active" with the text of an old failure invites reading a healthy
	// account as a broken one.
	LastError     string  `json:"lastError,omitempty"`
	SessionPct    float64 `json:"sessionPct"`
	WeeklyPct     float64 `json:"weeklyPct"`
	SessionReset  string  `json:"sessionReset,omitempty"`
	WeeklyReset   string  `json:"weeklyReset,omitempty"`
	LastSeen      string  `json:"lastSeen,omitempty"`
	LastStatus    int     `json:"lastStatus,omitempty"`
	CredentialSet bool    `json:"credentialSet"`
}

// Statuses reports every configured account's live health, in settings
// order. Unreadable is not empty, same as Accounts.
func (s *Service) Statuses() ([]AccountStatus, error) {
	accs, err := s.Accounts()
	if err != nil {
		return nil, err
	}
	s.health.mu.Lock()
	defer s.health.mu.Unlock()
	now := s.health.now()
	out := make([]AccountStatus, 0, len(accs))
	for _, a := range accs {
		out = append(out, statusRow(a, s.health.get(a.Name), now))
	}
	return out, nil
}

func statusRow(a Account, h *acctHealth, now time.Time) AccountStatus {
	st := AccountStatus{
		Name: a.Name, Kind: a.Kind,
		SessionPct: h.sessionPct, WeeklyPct: h.weeklyPct,
		SessionReset: h.sessionReset, WeeklyReset: h.weeklyReset,
		LastStatus: h.lastStatus, CredentialSet: a.APIKey != "",
	}
	if !h.lastSeen.IsZero() {
		st.LastSeen = h.lastSeen.Format(time.RFC3339)
	}
	switch {
	case h.unauthorized:
		st.State = "unauthorized"
	case now.Before(h.limitedUntil):
		st.State = "limited"
		st.LimitedUntil = h.limitedUntil.Format(time.RFC3339)
	// After the quota case on purpose: a limit carries a reset the upstream
	// named and can stand for days, while unreachable is a short guess
	// (unreachableCooldown). When both hold, the limit is worth showing.
	case now.Before(h.downUntil):
		st.State = "unreachable"
		st.LastError = h.lastError
	// Keyed on the LAST status, so it needs no separate flag and clears
	// itself: the next response that serves sets lastStatus under 400 and
	// wipes lastError. A transport failure sets lastStatus to 0, so it reads
	// unreachable above rather than falling in here.
	case h.lastStatus >= 400:
		st.State = "failing"
		st.LastError = h.lastError
	case h.lastSeen.IsZero():
		st.State = "untried"
	default:
		st.State = "ok"
	}
	return st
}

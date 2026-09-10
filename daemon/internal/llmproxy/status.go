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
	// LastError is carried only in the states that HAVE a current reason,
	// unreachable and failing. An account that has recovered reads as active,
	// and pairing "active" with the text of an old failure invites reading a
	// healthy account as a broken one.
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

// statusRow joins one account with what the proxy learned about it. The STATE
// is not decided here — acctHealth.state owns that, so routing and this row
// can never disagree about what an account is. All this adds is the per-state
// detail the tab shows alongside it.
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
	state := h.state(now)
	st.State = string(state)
	switch state {
	case stateLimited:
		st.LimitedUntil = h.limitedUntil.Format(time.RFC3339)
	case stateUnreachable, stateFailing:
		st.LastError = h.lastError
	}
	return st
}

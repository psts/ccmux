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
	// "unauthorized" (credential rejected), "untried" (no traffic seen yet).
	State        string  `json:"state"`
	LimitedUntil string  `json:"limitedUntil,omitempty"`
	SessionPct   float64 `json:"sessionPct"`
	WeeklyPct    float64 `json:"weeklyPct"`
	SessionReset string  `json:"sessionReset,omitempty"`
	WeeklyReset  string  `json:"weeklyReset,omitempty"`
	LastSeen     string  `json:"lastSeen,omitempty"`
	LastStatus   int     `json:"lastStatus,omitempty"`
	CredentialSet bool   `json:"credentialSet"`
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
	case h.lastSeen.IsZero():
		st.State = "untried"
	default:
		st.State = "ok"
	}
	return st
}

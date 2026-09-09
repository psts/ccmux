package llmproxy

import "slices"

// Candidate resolution: which accounts may answer one pane's request, and in
// what order. Three tiers, most specific first.
//
//  1. The pane's own override, when it has one, is always tried first.
//  2. The harness recorded on the pane contributes the rest: every account
//     whose KIND it declared it can talk to, in the harness's own account
//     order when it set one, else in the order the accounts are configured.
//  3. A pane with no recorded harness (a plain shell, a tool started by
//     hand) has no declaration to filter on, so it follows the default
//     account and fails over only within that account's own kind.
//
// Health orders the result last: a limited or rejected account sinks to the
// back without being dropped, so a lifted limit still gets a chance.

// Request dialects. Every account's upstream understands exactly one:
// "codex" accounts answer OpenAI's Responses API, every other kind answers
// Anthropic's Messages API. A failover replay may only be re-aimed at an
// account speaking the SAME dialect as the request was built for. Crossing
// that line would hand the second upstream a body it cannot parse along with
// a credential it should never see, which is why this is enforced here (and
// again per attempt in poolTransport) rather than left to the kind checkboxes.
const (
	dialectMessages  = "messages"
	dialectResponses = "responses"
)

func dialectOf(kind string) string {
	if kind == "codex" {
		return dialectResponses
	}
	return dialectMessages
}

// KindAllowed is THE harness/account compatibility rule, read from a
// harness's declared account kinds: empty means any kind EXCEPT the two
// per-pane ones. A codex account's upstream answers another dialect
// entirely, and a meridian account is a Claude Code loop that a hand-started
// claude must not ride, so neither is something a harness gets by default —
// it has to say so.
func KindAllowed(kinds []string, kind string) bool {
	if len(kinds) == 0 {
		return kind != "codex" && kind != KindMeridian
	}
	return slices.Contains(kinds, kind)
}

// PaneHarness reports what the harness recorded on a pane may use: the
// account kinds it declared and its custom account order (empty = follow the
// configured order). known is false for a pane ccmux did not start under a
// named harness, which is a different routing class, not merely an
// undeclared one — see tier 3 above.
type PaneHarness func(paneID string) (kinds, order []string, known bool)

// SetPaneHarness wires the pane-to-harness lookup. The proxy resolves routing
// per request and the manager owns pane state, so this stays a hook rather
// than an import: llmproxy must not depend on the manager it is called from.
func (s *Service) SetPaneHarness(fn PaneHarness) { s.harnessFor = fn }

func (s *Service) harnessOf(paneID string) (kinds, order []string, known bool) {
	if s.harnessFor == nil {
		return nil, nil, false
	}
	return s.harnessFor(paneID)
}

// candidatesFor builds the ordered pool for one pane. Never empty: the last
// resort is the default account, which with nothing configured is the direct
// Anthropic pass-through.
func (s *Service) candidatesFor(paneID string) ([]Account, error) {
	accs, err := s.Accounts()
	if err != nil {
		return nil, err
	}
	pool := s.harnessPool(paneID, accs)
	if len(pool) == 0 {
		// Either no harness declaration to work from, or nothing configured
		// that it can use. Both mean the same thing to a pane: fall back to
		// the default account rather than fail.
		if pool, err = s.defaultPool(paneID, accs); err != nil {
			return nil, err
		}
	}
	if head := overrideFor(accs, s.paneOverride(paneID)); head != nil {
		pool = headFirst(pool, *head)
	}
	return s.health.order(sameDialect(pool)), nil
}

// harnessPool is tier 2: the accounts the pane's harness declared it can
// talk to, in its own order when it set one. nil when the pane has no
// recorded harness.
func (s *Service) harnessPool(paneID string, accs []Account) []Account {
	kinds, order, known := s.harnessOf(paneID)
	if !known {
		return nil
	}
	allowed := make([]Account, 0, len(accs))
	for _, a := range accs {
		if KindAllowed(kinds, a.Kind) {
			allowed = append(allowed, a)
		}
	}
	return inOrder(allowed, order)
}

// defaultPool is tier 3: the default account, then the other accounts of its
// own kind as failover. Same-kind is the only dialect-safe generalization
// available without a harness declaration to check, and it is exactly the
// shape the claude subscription pool has had since it shipped.
//
// The synthetic pass-through (no route configured) pools with nothing: it
// forwards the pane's OWN login, so failing it over onto a keyed account
// would silently change whose credential answers.
func (s *Service) defaultPool(paneID string, accs []Account) ([]Account, error) {
	preferred, err := s.resolve(paneID)
	if err != nil {
		return nil, err
	}
	route, err := s.routeFor(paneID)
	if err != nil {
		return nil, err
	}
	// An empty route is the synthetic pass-through. Tested on the ROUTE and
	// not on the resolved name, which is "anthropic" and is a name a real
	// account may legally take — matching on it would pool a pane's own login
	// with keyed accounts, the one thing this branch exists to prevent.
	if route == "" {
		return []Account{preferred}, nil
	}
	pool := []Account{preferred}
	for _, a := range accs {
		if a.Kind == preferred.Kind && a.Name != preferred.Name {
			pool = append(pool, a)
		}
	}
	return pool, nil
}

// paneOverride is the pane's explicit route name, "" when it has none. A
// read failure reports none: candidatesFor already has a pool by this point,
// and losing an override is better than failing the request outright.
func (s *Service) paneOverride(paneID string) string {
	routes, err := s.PaneRoutes()
	if err != nil {
		return ""
	}
	return routes[paneID]
}

func overrideFor(accs []Account, name string) *Account {
	if name == "" {
		return nil
	}
	return findAccount(accs, name)
}

// inOrder sorts allowed accounts by a harness's declared order: the named
// ones first in that order, then everything else as configured. A name that
// no longer matches an account is skipped rather than failing the pane —
// account names rot as accounts are renamed and deleted, and a stale entry
// in a preference list is not a reason to stop answering.
func inOrder(allowed []Account, order []string) []Account {
	if len(order) == 0 {
		return allowed
	}
	out := make([]Account, 0, len(allowed))
	for _, name := range order {
		if a := findAccount(allowed, name); a != nil && findAccount(out, name) == nil {
			out = append(out, *a)
		}
	}
	for _, a := range allowed {
		if findAccount(out, a.Name) == nil {
			out = append(out, a)
		}
	}
	return out
}

// headFirst promotes one account to the front, keeping the rest in order.
func headFirst(pool []Account, head Account) []Account {
	out := []Account{head}
	for _, a := range pool {
		if a.Name != head.Name {
			out = append(out, a)
		}
	}
	return out
}

// sameDialect drops members that cannot serve the request the head's dialect
// will build. The kind checkboxes are advisory (an unknown harness may need
// an override the registry has never heard of); this is not.
func sameDialect(pool []Account) []Account {
	if len(pool) < 2 {
		return pool
	}
	want := dialectOf(pool[0].Kind)
	out := make([]Account, 0, len(pool))
	out = append(out, pool[0])
	for _, a := range pool[1:] {
		if dialectOf(a.Kind) == want {
			out = append(out, a)
		}
	}
	return out
}

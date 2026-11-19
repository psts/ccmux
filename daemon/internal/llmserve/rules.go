package llmserve

import (
	"log"
	"strings"

	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/llmproxy"
)

// Registry is the harness list, as far as routing needs it.
type Registry interface {
	Resolve(name string) (harness.Harness, error)
}

// HarnessRules answers the proxy's "what does this pane run, and what may it
// use" from two lookups: the pane's harness name, and the harness registry.
// ONE function for both homes of the proxy: the daemon passes its manager's
// memory, the service passes the database (Reader), and the rule between
// them cannot drift.
//
// known is false for a pane ccmux did not start under a named harness (a
// plain shell, a tool the user typed) AND for one whose recorded harness no
// longer resolves. Both route as a pane with no harness: the default account.
func HarnessRules(paneHarness func(paneID string) string, reg Registry) llmproxy.PaneHarness {
	return func(paneID string) (kinds, order []string, known bool) {
		name := paneHarness(paneID)
		if name == "" {
			return nil, nil, false
		}
		h, err := reg.Resolve(name)
		if err != nil {
			// An unknown name is ordinary (renamed or removed under a running
			// pane). An unreadable or corrupt registry is not: it moves every
			// harness pane onto the default account, a routing change nothing
			// else would report.
			if !strings.Contains(err.Error(), "unknown harness") {
				log.Printf("llm: pane %s: harness %q unreadable, routing as if it had none: %v", paneID, name, err)
			}
			return nil, nil, false
		}
		return h.AccountKinds, h.AccountOrder, true
	}
}

// paneHarness is the Reader's lookup in the shape HarnessRules takes. A read
// that fails is logged and answered as "no harness", the same way an
// unreadable registry is above. A database that cannot be read at all does
// not get this far as a routing decision: the proxy reads the accounts from
// the same file first, and that read failing is a 502 for the request.
func (r *Reader) paneHarness(paneID string) string {
	name, err := r.HarnessForPane(paneID)
	if err != nil {
		log.Printf("llm: pane %s: its harness is unreadable, routing as if it had none: %v", paneID, err)
		return ""
	}
	return name
}

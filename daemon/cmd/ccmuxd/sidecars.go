package main

import (
	"context"
	"log"
	"strings"

	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/meridian"
)

// wireSidecars starts Meridian sidecar supervision: one process per
// meridian-kind account, so opencode/pi panes can spend a Claude subscription
// (internal/meridian). Reconciled here from the stored accounts and again on
// every settings apply; the opencode plugin path rides into pane env when
// Meridian is installed.
func wireSidecars(ctx context.Context, llmSvc *llmproxy.Service, apiSrv *api.Server, mgr *manager.Manager) *meridian.Supervisor {
	sidecars := meridian.New(ctx, harness.LookPath)
	if accs, err := llmSvc.Accounts(); err != nil {
		log.Printf("meridian: llm accounts unreadable at boot, no sidecars started: %v", err)
	} else {
		sidecars.Reconcile(meridian.SpecsFrom(accs))
	}
	apiSrv.SetSidecars(sidecars)
	mgr.OpencodePlugin = func() string { return meridian.PluginPath(harness.LookPath) }
	return sidecars
}

// paneHarnessLookup answers the proxy's "what does this pane run, and what
// may it use" question. The proxy resolves routing per request and the
// manager owns pane state, so this closure is the seam between them rather
// than an import either way.
//
// known is false for a pane ccmux did not start under a named harness (a
// plain shell, a tool the user typed themselves) AND for one whose recorded
// harness no longer resolves. Both mean the same thing to routing: there is
// no declaration to filter accounts by, so the pane follows the default
// account instead of a harness order it never had.
func paneHarnessLookup(mgr *manager.Manager) llmproxy.PaneHarness {
	return func(paneID string) (kinds, order []string, known bool) {
		name := mgr.HarnessForPane(paneID)
		if name == "" || mgr.Harnesses == nil {
			return nil, nil, false
		}
		h, err := mgr.Harnesses.Resolve(name)
		if err != nil {
			// An unknown name is ordinary (a harness was renamed or removed
			// under a running pane) and tier 3 is the right answer for it.
			// An unreadable or corrupt registry is not ordinary: it silently
			// moves every harness pane onto the default account, which is a
			// routing change nothing else would report. Same rule the proxy
			// states for its own reads — a proxy that guesses where to send
			// tokens is worse than one that says it cannot tell.
			if !strings.Contains(err.Error(), "unknown harness") {
				log.Printf("llm: pane %s: harness %q unreadable, routing as if it had none: %v", paneID, name, err)
			}
			return nil, nil, false
		}
		return h.AccountKinds, h.AccountOrder, true
	}
}

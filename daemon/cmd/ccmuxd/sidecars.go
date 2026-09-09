package main

import (
	"context"
	"log"

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

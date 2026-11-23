package main

import (
	"context"
	"log"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/api"
	"ccmux.dev/ccmuxd/internal/harness"
	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/meridian"
)

// wireSidecars sets up the Meridian sidecars: one process per meridian-kind
// account, so opencode/pi panes can spend a Claude subscription
// (internal/meridian). They run where the LLM proxy runs: in its own service
// when it has one (internal/llmserve, sidecars.go there), so a restart of
// this daemon, every deploy, leaves them up; in this process otherwise
// (macOS, no systemd, -llm-proxy-unit none, a scratch daemon with no unit),
// reconciled here from the stored accounts and again on every settings
// apply. The opencode plugin path rides into pane env either way: it is read
// from the installed Meridian, not from a running sidecar.
//
// The returned stop ends the sidecars this process runs (none when the
// service runs them).
func wireSidecars(ctx context.Context, proxy *llmProxy, llmSvc *llmproxy.Service, apiSrv *api.Server, mgr *manager.Manager) (stop func()) {
	mgr.OpencodePlugin = func() string { return meridian.PluginPath(harness.LookPath) }
	if !sidecarsInDaemon(proxy) {
		apiSrv.SetSidecars(newRemoteSidecars(proxy.client, llmSvc.Accounts))
		log.Printf("meridian: sidecars run in the llm proxy service %s (journalctl --user -u %s)", proxy.unit, proxy.unit)
		return func() {}
	}
	sidecars := meridian.New(ctx, harness.LookPath)
	if accs, err := llmSvc.Accounts(); err != nil {
		log.Printf("meridian: llm accounts unreadable at boot, no sidecars started: %v", err)
	} else {
		sidecars.Reconcile(meridian.SpecsFrom(accs))
	}
	apiSrv.SetSidecars(sidecars)
	return sidecars.Stop
}

// sidecarsInDaemon is whether this process runs the sidecars: exactly when
// it runs the proxy too.
func sidecarsInDaemon(proxy *llmProxy) bool { return proxy.unit == "" }

// sidecarClient is the slice of *llmserve.Client the remote view uses.
type sidecarClient interface {
	Sidecars(ctx context.Context) (map[string]meridian.Status, error)
	ReconcileSidecars(ctx context.Context) error
}

// remoteSidecars is the API's view of sidecars the proxy service runs. It
// starts nothing: the service reads the accounts itself. Reconcile only asks
// it to look now (it would within seconds anyway), and Status is its answer,
// kept a moment, because GET /v1/health carries it and every lens polls that.
type remoteSidecars struct {
	c    sidecarClient
	accs func() ([]llmproxy.Account, error)
	ttl  time.Duration

	mu     sync.Mutex
	at     time.Time
	st     map[string]meridian.Status
	logged string // the last read error logged, so a down service logs once
}

func newRemoteSidecars(c sidecarClient, accs func() ([]llmproxy.Account, error)) *remoteSidecars {
	return &remoteSidecars{c: c, accs: accs, ttl: 2 * time.Second}
}

// Reconcile ignores the specs it is handed: they are the accounts the
// service reads from the same database.
func (r *remoteSidecars) Reconcile([]meridian.Spec) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.c.ReconcileSidecars(ctx); err != nil {
		log.Printf("meridian: the llm proxy service was not told of the new accounts (it reads them again within seconds): %v", err)
	}
}

// Status is the service's answer, kept a moment.
func (r *remoteSidecars) Status() map[string]meridian.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st != nil && time.Since(r.at) < r.ttl {
		return r.st
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := r.c.Sidecars(ctx)
	switch {
	case err != nil:
		st = r.unknown(err)
	case st == nil:
		st = map[string]meridian.Status{}
	}
	if err == nil {
		r.logged = ""
	}
	r.at, r.st = time.Now(), st
	return st
}

// unknown is Status when the service did not answer (down, restarting, or
// an older build with no such route): every meridian account with the
// reason, so Settings says why instead of "not supervised". Logged once per
// distinct error, not at every poll.
func (r *remoteSidecars) unknown(err error) map[string]meridian.Status {
	why := "state unknown: the llm proxy service did not answer: " + err.Error()
	accs, aerr := r.accs()
	said := why
	if aerr != nil {
		said += "; the accounts were not read either, so none is named: " + aerr.Error()
	}
	if said != r.logged {
		log.Printf("meridian: sidecar %s", said)
		r.logged = said
	}
	out := map[string]meridian.Status{}
	if aerr != nil {
		return out
	}
	for _, s := range meridian.SpecsFrom(accs) {
		out[s.Name] = meridian.Status{LastError: why}
	}
	return out
}

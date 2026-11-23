package llmserve

import (
	"context"
	"log"
	"net/http"
	"time"

	"ccmux.dev/ccmuxd/internal/llmproxy"
	"ccmux.dev/ccmuxd/internal/meridian"
)

// The Meridian sidecars (internal/meridian) run in this service, not in the
// daemon: a request routed to a meridian account reaches its sidecar from
// here, and a sidecar the daemon's restart took down was marked unreachable
// by the pool and failed over, maybe to a metered key, on every deploy.
//
// The set of sidecars follows the meridian-kind accounts in the daemon's
// database. Nothing tells this service when they change (it reads the
// settings row per request for everything else too), so it reads them again
// every sidecarEvery; the daemon also asks it to look at once after a
// settings write (POST /ctl/sidecars/reconcile), which only saves the wait.

// sidecarEvery is how often the accounts are read for the sidecar set.
const sidecarEvery = 5 * time.Second

// sidecarSet is the slice of *meridian.Supervisor this service drives.
type sidecarSet interface {
	Reconcile([]meridian.Spec)
	Status() map[string]meridian.Status
}

// accountSource is where the sidecar set is read from: the proxy's own
// service over the read-only database.
type accountSource interface {
	Accounts() ([]llmproxy.Account, error)
}

// sidecars keeps the running set matching the accounts.
type sidecars struct {
	set  sidecarSet
	accs accountSource
	poke chan struct{}
}

func newSidecars(set sidecarSet, accs accountSource) *sidecars {
	return &sidecars{set: set, accs: accs, poke: make(chan struct{}, 1)}
}

// reconcile reads the accounts once and matches the set to them. A read
// that fails leaves the set as it is: the database can be missing or busy
// for a moment (reader.go), and that is not "no accounts", which would stop
// every sidecar.
func (s *sidecars) reconcile() {
	accs, err := s.accs.Accounts()
	if err != nil {
		log.Printf("meridian: accounts unreadable, sidecars left as they were: %v", err)
		return
	}
	s.set.Reconcile(meridian.SpecsFrom(accs))
}

// keep reconciles now, then every tick and on every poke, until ctx ends.
func (s *sidecars) keep(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.reconcile()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.poke:
		}
	}
}

// nudge asks keep to look now; a nudge already waiting covers this one.
func (s *sidecars) nudge() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

func (s *sidecars) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /ctl/sidecars", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.set.Status())
	})
	mux.HandleFunc("POST /ctl/sidecars/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		s.nudge()
		writeJSON(w, http.StatusOK, struct{}{})
	})
}

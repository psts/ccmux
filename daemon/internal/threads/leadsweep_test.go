package threads

import (
	"errors"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
)

// The sweep leaves alone a thread whose lead runs, a merging thread whose
// lead ended (its merge's end takes it apart), and a thread already ended.
func TestTheSweepTakesApartOnlyAnOpenThreadWhoseLeadEnded(t *testing.T) {
	r, th, _, backend, _ := team(t)
	r.svc.SweepLeadless()
	if r.state(th.ID).State != store.ThreadOpen || r.row(backend.ID).Wanted != store.SessionRunning || r.removed() != "" {
		t.Fatalf("a thread whose lead runs is not touched: %+v %s", r.state(th.ID), r.removed())
	}

	r2, th2, lead2, backend2, admin2 := team(t)
	r2.asksMerge(th2, ByLead)
	r2.jobs.stopErr = errors.New("unit not reached") // the job runs on
	r2.endLead(lead2)
	r2.svc.SweepLeadless()
	r2.untouched(th2, backend2, admin2)

	r3, th3, lead3, _, _ := team(t)
	r3.endLead(lead3)
	before := r3.state(th3.ID)
	r3.svc.SweepLeadless()
	if after := r3.state(th3.ID); after != before || r3.removed() != "backend|app" {
		t.Fatalf("an ended thread is not taken apart again: %+v %+v %s", before, after, r3.removed())
	}
}

// A lead whose row cannot be read is left for the next pass.
func TestTheSweepLeavesAThreadWhoseLeadIsNotRead(t *testing.T) {
	r, th, lead, backend, _ := team(t)
	r.svc.St = &threadFailsAfter{Store: r.st, left: 0, err: errors.New("database is locked")}
	r.jobs.openErr = errors.New("database is locked")
	r.endLead(lead)
	r.jobs.openErr = nil
	r.svc.St = sessionFails{Store: r.st, id: lead.ID, left: new(int)}
	*r.svc.St.(sessionFails).left = 1
	r.svc.SweepLeadless()
	if r.state(th.ID).State != store.ThreadOpen || r.row(backend.ID).Wanted != store.SessionRunning {
		t.Fatalf("an unread lead is left alone: %+v", r.state(th.ID))
	}
	r.svc.SweepLeadless()
	if r.state(th.ID).State != store.ThreadEnded {
		t.Fatalf("the next pass takes it apart: %+v", r.state(th.ID))
	}
}

// sessionFails is the registry with one session's row refusing to be read
// a number of times.
type sessionFails struct {
	Store
	id   string
	left *int
}

func (f sessionFails) RunnerSession(id string) (*store.RunnerSession, error) {
	if id == f.id && *f.left > 0 {
		*f.left--
		return nil, errors.New("database is locked")
	}
	return f.Store.RunnerSession(id)
}

// A merge ends and its lead's row cannot be read: the lead is neither gone
// nor alive, so the end is an error and is settled again; the thread still
// reads as merging until then.
func TestAMergesEndWithItsLeadUnreadIsSettledAgain(t *testing.T) {
	r, th, lead, _, _ := team(t)
	job, res := r.asksMerge(th, ByLead)
	fails := 1
	r.svc.St = sessionFails{Store: r.st, id: lead.ID, left: &fails}
	if err := r.svc.MergeHandler().Apply(job, res); err == nil {
		t.Fatal("an unread lead leaves the job to be settled again")
	}
	if got := r.state(th.ID); got.State != store.ThreadMerging {
		t.Fatalf("the thread still reads as merging: %+v", got)
	}
	if said := r.sess.prompts(lead.ID); len(said) != 0 {
		t.Fatalf("nobody is told yet: %v", said)
	}
	if err := r.svc.MergeHandler().Apply(job, res); err != nil {
		t.Fatal(err)
	}
	if got := r.state(th.ID); got.State == store.ThreadMerging {
		t.Fatalf("the next try settles it: %+v", got)
	}
}

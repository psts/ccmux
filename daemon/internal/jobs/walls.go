package jobs

import (
	"fmt"
	"log"
	"os"

	"ccmux.dev/ccmuxd/internal/walls"
)

// A walled job. The walls are a drop-in file for the job's own unit
// (walls.Plan.DropIn), written before its first start, and the job's spec
// says what the job must find when it checks them (Spec.Walls). Both are
// written once: a job that is started again runs behind the same walls,
// whatever the setting says by then.

// wall writes a new job's walls, and returns what the job is to check.
// Nil for a job that runs open. The job's own folder and the locks' folder
// are made here: the walls open both, and systemd wants every folder they
// open to be there.
func (m *Manager) wall(id string, n New) (*walls.Expect, error) {
	if m.Walls == nil {
		return nil, nil
	}
	plan, err := m.Walls(n)
	if err != nil || plan == nil {
		return nil, err
	}
	for _, dir := range []string{m.Paths.Dir(id), m.Paths.Locks} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		plan.Write = append(plan.Write, dir)
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("the job cannot be walled: %w", err)
	}
	ns, err := walls.MountNS()
	if err != nil {
		return nil, fmt.Errorf("the job cannot be walled: the daemon's mount namespace was not read: %w", err)
	}
	if _, err := walls.SetDropIn(m.UnitDir, m.Paths.Unit(id), plan.DropIn()); err != nil {
		return nil, fmt.Errorf("the job's walls were not written: %w", err)
	}
	// Always: the unit is new, and systemd reads a drop-in only then.
	if err := m.Units.Reload(); err != nil {
		m.unwall(id)
		return nil, fmt.Errorf("the job's walls were written and systemd did not read them: %w", err)
	}
	return plan.Expect(ns), nil
}

// unwall removes a job's drop-in file, once the job has ended or was never
// started. A file that stays is only litter: the unit it names never runs
// again.
func (m *Manager) unwall(id string) {
	if m.UnitDir == "" {
		return
	}
	if _, err := walls.SetDropIn(m.UnitDir, m.Paths.Unit(id), ""); err != nil {
		log.Printf("jobs: %s: its walls file was not removed: %v", id, err)
	}
}

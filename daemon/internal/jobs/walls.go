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
	if err := m.ownFolders(id, plan); err != nil {
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

// ownFolders adds a job's own folders to its plan and makes them: its own
// folder in the jobs' (of which it sees nothing else: another job's spec
// carries the daemon's environment), the locks' folder, and the folders
// that stand in for what Claude Code shares between sessions.
func (m *Manager) ownFolders(id string, plan *walls.Plan) error {
	dir := m.Paths.Dir(id)
	for _, made := range []string{dir, m.Paths.Locks} {
		if err := os.MkdirAll(made, 0o700); err != nil {
			return err
		}
	}
	plan.Own = append(plan.Own, walls.Own{Dir: m.Paths.State, Write: []string{dir}})
	plan.Write = append(plan.Write, m.Paths.Locks)
	plan.PrivateRoot = dir
	if err := plan.MakePrivate(); err != nil {
		return err
	}
	return plan.Validate()
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

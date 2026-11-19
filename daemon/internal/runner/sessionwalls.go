package runner

import (
	"errors"
	"fmt"
	"os"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/walls"
)

// A walled session. Its walls are a drop-in file for its own unit, written
// at every start from what the Sandbox setting says then (Config.Walls),
// and its spec says what its runner must find when it checks them
// (Spec.Walls, Exec). A session that runs open has neither, and a drop-in
// from an earlier start is removed.

// plan is the walls a session starts behind, with the runner's own
// folders added: its own folder in State (its state file), its images'
// folder, and the folder its socket is made in. Nil for a session that
// runs open.
//
// The sockets' folder is every session's: a walled session could remove
// another's socket. That costs the other session its connection to the
// daemon until its next start, and opens nothing.
func (c Config) plan(sess store.RunnerSession, paths Paths) (*walls.Plan, error) {
	if c.Walls == nil {
		return nil, nil
	}
	plan, err := c.Walls(sess)
	if err != nil || plan == nil {
		return nil, err
	}
	plan.Write = append(plan.Write, paths.OwnDir(sess.ID), paths.MediaDir(sess.ID), paths.Runtime)
	return plan, nil
}

// walledArgs are a walled session's flags: its runner keeps its state file
// in the session's own folder, and, when the plan asks for it, runs its
// shell commands in Claude Code's own sandbox, opened onto the same
// folders as the walls (claudeserve/shellbox.mjs).
func walledArgs(args []string, sess store.RunnerSession, paths Paths, plan walls.Plan) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i += 2 {
		if out[i] == "--state" {
			out[i+1] = paths.WalledStateFile(sess.ID)
		}
	}
	if !plan.ShellBox {
		return out
	}
	for _, dir := range plan.Open() {
		out = append(out, "--shell-box-write", dir)
	}
	return out
}

// stateArg is the state file a spec's runner keeps, "" for none.
func stateArg(sp Spec) string {
	for i := 0; i+1 < len(sp.Args); i += 2 {
		if sp.Args[i] == "--state" {
			return sp.Args[i+1]
		}
	}
	return ""
}

// wall makes a session's unit what its new spec says: behind sp.Plan's
// walls, or open. It runs before the spec is written, so the spec on disk
// is still the last start's.
//
// The state file follows the session across its walls: the last start's
// spec says where the runner kept it, and when the session is walled now
// and was not (or the other way round) it is moved to where the new runner
// keeps it (moveState). What it holds is which prompts reached Claude;
// left behind, the newest ones would be sent again.
func (s *Supervisor) wall(sess store.RunnerSession, sp *Spec) error {
	text, err := s.wallText(sess, sp)
	if err != nil {
		return err
	}
	if err := s.moveState(sess, *sp); err != nil {
		return err
	}
	if s.paths.UnitDir == "" {
		if sp.Plan != nil {
			return errors.New("session runner: it is to be walled, and this daemon was given no folder for its walls")
		}
		return nil
	}
	changed, err := walls.SetDropIn(s.paths.UnitDir, s.paths.Unit(sess.ID), text)
	if err != nil {
		return fmt.Errorf("session runner: its walls were not written: %w", err)
	}
	if !changed {
		return nil
	}
	// systemd reads a unit's files when it loads it, and a unit that ran
	// before is loaded: without this the start is on the old walls.
	if err := s.units.Reload(); err != nil {
		return fmt.Errorf("session runner: its walls were written and systemd did not read them: %w", err)
	}
	return nil
}

// wallText is the drop-in text of a session's plan, "" for one that runs
// open. For a walled one it makes the runner's own folders (systemd wants
// every folder the walls open to be there) and writes into sp what the
// runner must find when it checks.
func (s *Supervisor) wallText(sess store.RunnerSession, sp *Spec) (string, error) {
	if sp.Plan == nil {
		return "", nil
	}
	for _, dir := range []string{s.paths.OwnDir(sess.ID), s.paths.MediaDir(sess.ID), s.paths.Runtime} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("session runner: a folder of its walls: %w", err)
		}
	}
	if err := sp.Plan.Validate(); err != nil {
		return "", fmt.Errorf("session runner: it cannot be walled: %w", err)
	}
	ns, err := walls.MountNS()
	if err != nil {
		return "", fmt.Errorf("session runner: the daemon's mount namespace was not read: %w", err)
	}
	sp.Walls = sp.Plan.Expect(ns)
	return sp.Plan.DropIn(), nil
}

// moveState moves the state file from where the last start's runner kept
// it to where the new spec's runner will, when the session crossed its
// walls between the two starts, and only then. A role agent that wakes in
// a fresh conversation changes its state file too (it is named after the
// conversation), and that one must start empty: carried across, the old
// file would tell the new conversation to carry on with a turn the old one
// was stopped in.
//
// A last spec or a state file that cannot be read is logged and the start
// goes on: the spec is rewritten right after this, and a start blocked on
// an old file nobody reads would be blocked for good.
func (s *Supervisor) moveState(sess store.RunnerSession, sp Spec) error {
	last, err := ReadSpec(s.paths.SpecFile(sess.ID))
	if errors.Is(err, os.ErrNotExist) {
		return nil // a first start
	}
	if err != nil {
		s.note(sess.ID, "its last spec was not read, so its state file stays where it was: "+err.Error())
		return nil
	}
	from, to := stateArg(last), stateArg(sp)
	walled := s.paths.WalledStateFile(sess.ID)
	if from == "" || to == "" || (from == walled) == (to == walled) {
		return nil // the same side of the walls
	}
	if _, err := os.Stat(from); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.note(sess.ID, "its state file was not read, so it stays where it was: "+err.Error())
		}
		return nil
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("session runner: its state file did not follow it to %s: %w", to, err)
	}
	return nil
}

// unwall removes an ended session's drop-in file.
func (s *Supervisor) unwall(sessionID string) {
	if s.paths.UnitDir == "" {
		return
	}
	if _, err := walls.SetDropIn(s.paths.UnitDir, s.paths.Unit(sessionID), ""); err != nil {
		s.note(sessionID, "its walls file was not removed: "+err.Error())
	}
}

// refusedNote is the note of a session whose runner refused to run open.
func refusedNote(unit string) string {
	return "put to sleep: the sandbox is on, and its walls were not up when it started, so it did not run (journalctl --user -u " + unit + " says what it found). " + walls.Fix
}

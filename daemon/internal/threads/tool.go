package threads

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
)

// List is every thread whose lead has not ended, oldest first.
func (s *Service) List() ([]store.Thread, error) { return s.St.Threads() }

// toolActions are the thread tool's actions (the shim's `thread`): what the
// lead does with its own thread.
var toolActions = map[string]func(s *Service, threadID int64, repo string) (string, error){
	"helpers": func(s *Service, id int64, _ string) (string, error) { return s.Describe(id) },
	"stand_down": func(s *Service, id int64, repo string) (string, error) {
		return s.StandDown(id, strings.TrimSpace(repo))
	},
	"merge": func(s *Service, id int64, _ string) (string, error) { return s.Merge(id) },
	"end":   func(s *Service, id int64, _ string) (string, error) { return s.End(id) },
}

// Tool runs one action of the thread tool for the session that called it.
// The tool is the lead's alone: the lead ends the thread and stands its
// helpers down, a helper only reports to it.
func (s *Service) Tool(sessionID, action, repo string) (string, error) {
	sess, th, err := s.member(sessionID)
	if err != nil {
		return "", err
	}
	if sess.Role != store.RoleLead {
		return "", fmt.Errorf("the thread tool is the lead's: you are a helper in thread #%d, so tell %s what you need", th.ID, s.leadName(th))
	}
	run, ok := toolActions[action]
	if !ok {
		return "", fmt.Errorf(`action is "helpers", "stand_down" (with repo), "merge" or "end"`)
	}
	return run(s, th.ID, repo)
}

// Describe is a thread as its lead reads it: the thread's state, then each
// member with where it works and what its branch holds.
func (s *Service) Describe(threadID int64) (string, error) {
	d, err := s.Detail(threadID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Thread #%d %q is %s", d.Thread.ID, titleOf(d.Thread), d.Thread.State)
	if d.Thread.Note != "" {
		b.WriteString(" (" + d.Thread.Note + ")")
	}
	b.WriteString(".\n")
	if len(d.Members) == 1 {
		b.WriteString("No helpers. Message a repo of this window by its name to stand one up.\n")
	}
	for _, m := range d.Members {
		b.WriteString(memberLine(m) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// memberLine is one member in Describe.
func memberLine(m Member) string {
	place := "reading only in " + m.Session.Repo
	if m.Session.Worktree != "" {
		place = fmt.Sprintf("in its worktree %s on %s: %s", m.Session.Worktree, m.Session.Branch, strings.ToLower(m.Status[:1])+m.Status[1:])
	}
	doing := m.Session.Attention
	if doing == "" || m.Session.Wanted != store.SessionRunning {
		doing = m.Session.Wanted
	}
	return fmt.Sprintf("- %s (%s, %s): %s; %s", m.Session.Name, m.Session.Role, filepath.Base(m.Session.Repo), place, doing)
}

// LeadChanged hears that a session's title, goal or model changed. When the
// session leads a thread, the thread follows: its row carries the lead's
// title and goal, and its model is the lead's, which every helper is set to
// as well. A model applies at a session's next start, a helper's too.
func (s *Service) LeadChanged(sessionID string) {
	lead, th, err := s.member(sessionID)
	if err != nil || lead.Role != store.RoleLead {
		return
	}
	model := th.Model
	if th.Title == lead.Title && th.Goal == lead.Goal && th.Model == lead.Model {
		return
	}
	th.Title, th.Goal, th.Model, th.UpdatedAt = lead.Title, lead.Goal, lead.Model, s.millis()
	if err := s.St.SaveThread(th); err != nil {
		log.Printf("threads: #%d does not follow its lead's title, goal and model: %v", th.ID, err)
		return
	}
	if model != th.Model {
		s.helpersFollow(th)
	}
}

// helpersFollow sets a thread's model on its helpers.
func (s *Service) helpersFollow(th store.Thread) {
	helpers, err := s.helpers(th.ID)
	if err != nil {
		log.Printf("threads: #%d: its helpers were not set to %q: %v", th.ID, th.Model, err)
		return
	}
	for _, h := range helpers {
		if err := s.Sessions.SetModel(h.ID, th.Model); err != nil {
			log.Printf("threads: #%d: %s was not set to %q: %v", th.ID, h.Name, th.Model, err)
		}
	}
}

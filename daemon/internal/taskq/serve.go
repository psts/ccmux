package taskq

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// A task worker that tests its work in a browser needs its own dev servers:
// the repo's normal ports belong to the person's servers, and three workers
// cannot share one port anyway. Each part gets a block of ports of its own,
// one per hostname the repo's session maps, and a name under the dev domain
// for each: "chartlabs-app" (port 4000) becomes "chartlabs-app-t12" on a
// port of the part's block. The person's names never point at a worker.

// Ports for task servers sit between the old hostname allocations
// (21000-21999, see model.IsLegacyAllocatedPort) and the kernel's range for
// outgoing connections (32768 up), so neither can be holding one. Other
// software still may (MongoDB's 27017 falls in block 501): the worker's
// server then fails with "address in use", which it reports.
const (
	portBase   = 22000
	portStride = 10   // ports per part: a repo maps at most this many names
	portSlots  = 1076 // 22000 + 1076*10 = 32760
)

// Route is one dev server a task part may run: its own name and port, and
// the person's name and port it stands in for.
type Route struct {
	Name     string
	Port     int
	Main     string
	MainPort int
}

// PartRoutes gives part partID of task taskID one route per hostname the
// repo's session maps, in the session's order. Ports depend only on the part
// and the row, so the brief written at the start and the routing table built
// after a daemon restart agree. Parts this far apart in number never run at
// once: a window runs three at a time.
func PartRoutes(taskID, partID int64, hostnames []model.Hostname) []Route {
	block := portBase + int(partID%portSlots)*portStride
	out := []Route{}
	for i, h := range hostnames {
		if i == portStride {
			break
		}
		out = append(out, Route{Name: TaskHostname(h.Name, taskID), Port: block + i, Main: h.Name, MainPort: h.Port})
	}
	return out
}

// TaskHostname is the name task taskID's copy of a server goes by: the
// person's name with "-t<id>" on the end, cut so the whole stays one DNS
// label (63 characters) and never ends in a dash before the suffix.
func TaskHostname(name string, taskID int64) string {
	suffix := fmt.Sprintf("-t%d", taskID)
	if keep := 63 - len(suffix); len(name) > keep {
		name = strings.TrimRight(name[:keep], "-")
	}
	return name + suffix
}

// publishRoutes hands the devhost server the routes of every part whose
// worker pane is still open, so a worker's names come and go with it. Sent
// on every pass: after a daemon restart the first pass puts them back. A
// pass that cannot read a session's names sends nothing, so the routes the
// last good pass sent stay up.
func (r *Runner) publishRoutes(tasks []store.Task, parts []store.Part) {
	window := map[int64]string{}
	for _, t := range tasks {
		window[t.ID] = t.WindowID
	}
	routes := map[string]int{}
	for _, p := range parts {
		if p.PaneID == "" {
			continue
		}
		if _, open := r.Host.TaskPane(p.PaneID); !open {
			continue
		}
		hs, err := r.Host.TaskHostnames(p.RepoPath, window[p.TaskID])
		if err != nil {
			log.Printf("tasks: #%d: hostnames of %s unreadable, routes not updated this pass: %v", p.TaskID, p.RepoPath, err)
			return
		}
		for _, rt := range PartRoutes(p.TaskID, p.ID, hs) {
			routes[rt.Name] = rt.Port
		}
	}
	r.Host.SetTaskRoutes(routes)
}

// serversBrief is the brief's section on dev servers: the worker's own
// ports and names, its teammates' names, and the rules that keep the
// person's servers and the shared database safe. Its own names unreadable
// fails the start rather than tell the worker it has no ports; a teammate's
// are left out, and the bus is there to ask.
func (r *Runner) serversBrief(j job) (string, error) {
	domain := r.Host.DevDomain()
	own, err := r.Host.TaskHostnames(j.part.RepoPath, j.task.WindowID)
	if err != nil {
		return "", fmt.Errorf("reading the repo's dev hostnames: %w", err)
	}
	var b strings.Builder
	b.WriteString("## Dev servers\n\n")
	b.WriteString(ownServers(PartRoutes(j.task.ID, j.part.ID, own), domain))
	for _, m := range j.mates {
		hs, err := r.Host.TaskHostnames(m.RepoPath, j.task.WindowID)
		if err != nil {
			log.Printf("tasks: #%d: %s's hostnames left out of a teammate's brief: %v", j.task.ID, m.RepoPath, err)
			continue
		}
		rs := PartRoutes(j.task.ID, m.ID, hs)
		if len(rs) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\nThe %s part's servers, while its Claude runs them:\n", filepath.Base(m.RepoPath))
		for _, rt := range rs {
			fmt.Fprintf(&b, "- %s (instead of %s): %s\n", rt.Name, rt.Main, routeAddress(rt, domain))
		}
	}
	b.WriteString(fmt.Sprintf(serverRules, j.part.RepoPath))
	return b.String(), nil
}

// ownServers lists the worker's own routes, or says it has none.
func ownServers(rs []Route, domain string) string {
	if len(rs) == 0 {
		return "This repo's ccmux session maps no dev servers, so you have no ports of\n" +
			"your own. If you must run a server, use a free port, never the repo's usual one.\n"
	}
	var b strings.Builder
	b.WriteString("The repo's usual dev servers belong to the person: do not stop, restart or\n" +
		"reuse them, and do not use their ports. To try your work, run your own\n" +
		"copies from your worktree, on these ports only:\n\n")
	for _, rt := range rs {
		fmt.Fprintf(&b, "- %s (usually port %d): yours is port %d, at %s\n", rt.Main, rt.MainPort, rt.Port, routeAddress(rt, domain))
	}
	return b.String()
}

// routeAddress is where a route answers: its name under the dev domain, or
// the bare port where this host serves no dev domain.
func routeAddress(rt Route, domain string) string {
	if domain == "" {
		return fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
	}
	return "https://" + rt.Name + "." + domain
}

// serverRules is the rest of the section; %s is the repo's main checkout.
const serverRules = `
- Start servers in the background and give the port on the command line
  (next dev -p PORT, uvicorn ... --port PORT and so on). The repo's own dev
  script usually fixes the usual port, so do not run it as it is.
- Your worktree has none of the repo's untracked files. Copy the env files you
  need (.env.local and the like) from %s, and point every address of a server
  in this feature at its task name above. Leave other addresses as they are.
- Where the repo's CLAUDE.md or skills name a usual address (to log in, to
  check a server is up), use your own instead. Their rules against starting
  servers protect the person's servers; yours are fine to start. If one will
  not start under you, say so in your report.
- The database is shared with the person's servers. Do not run migrations or
  anything else that changes its tables. If your part needs a migration, write
  it and test it with the repo's test suite, but do not apply it, and say so in
  your report.
- Stop every server you started before you report.
`

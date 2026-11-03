package manager

import (
	"fmt"
	"maps"

	"ccmux.dev/ccmuxd/internal/model"
)

// Task workers' own dev servers (see taskq/serve.go). The task runner hands
// over the routes of every live worker (name → port); the devhost server
// serves them under the dev domain beside the sessions' own names. The ports
// a worker pane listens on are kept apart from its session's (see
// applyListeners), so a session's name never routes to a worker's server.

// SetTaskRoutes replaces the task workers' routes. The runner sends them on
// every pass, so the devhost server is poked only when they changed.
func (m *Manager) SetTaskRoutes(routes map[string]int) {
	m.mu.Lock()
	same := maps.Equal(m.taskRoutes, routes)
	if !same {
		m.taskRoutes = maps.Clone(routes)
	}
	m.mu.Unlock()
	if !same {
		m.notifyDevhost()
	}
}

// TaskRoutes is a copy of the task workers' routes, never nil.
func (m *Manager) TaskRoutes() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int, len(m.taskRoutes))
	maps.Copy(out, m.taskRoutes)
	return out
}

// TaskHostnames is the hostname rows (name and port only) of the session a
// task in repo runs in: TaskWorkspace's choice, without reopening it. They
// are what a worker's own servers stand in for. An error (no session, or the
// window list unreadable) is never "no hostnames": a caller that took it so
// would tell a worker it has no ports, or drop live routes.
func (m *Manager) TaskHostnames(repo, windowID string) ([]model.Hostname, error) {
	id, err := m.taskSession(repo, windowID)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	e := m.byID[id]
	if e == nil {
		return nil, fmt.Errorf("%w %s", ErrUnknownWorkspace, id)
	}
	out := make([]model.Hostname, 0, len(e.ws.Hostnames))
	for _, h := range e.ws.Hostnames {
		out = append(out, model.Hostname{Name: h.Name, Port: h.Port})
	}
	return out, nil
}

// taskWorkerOwner stands for every task worker in the port-owner map: a
// session's name whose port a worker holds routes nowhere and reads "held by
// a task worker", never to the worker's half-built app.
var taskWorkerOwner = portOwner{id: "task-worker", name: "a task worker"}

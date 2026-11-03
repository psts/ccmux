package manager

// TasksChanged tells every lens the task queue changed, so a page showing it
// reads the list again. The queue itself lives in the store (internal/taskq).
func (m *Manager) TasksChanged() { m.events.publish(Event{Kind: "tasks-changed"}) }

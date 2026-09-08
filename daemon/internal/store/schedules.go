// Timed prompts for agent instances (docs/agent-spec.md §8, "Schedules").
//
// One row per schedule, keyed by the instance it belongs to (window +
// agent). The daemon's scheduler tick reads the due ones and hands the
// prompt to the agent; next_run_at is stored, not recomputed at start, so a
// run missed while the daemon was down is visible as "due in the past" and
// the catch-up rule can decide about it.
package store

// AgentSchedule is one timed prompt. Times are unix milliseconds.
type AgentSchedule struct {
	ID        int64  `json:"id"`
	WindowID  string `json:"windowId"`
	Agent     string `json:"agent"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	Paused    bool   `json:"paused"`
	CreatedBy string `json:"createdBy,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	LastRunAt int64  `json:"lastRunAt,omitempty"`
	NextRunAt int64  `json:"nextRunAt"`
}

const scheduleCols = "id,window_id,agent,cron,prompt,paused,created_by,created_at,last_run_at,next_run_at"

// AddAgentSchedule inserts one schedule and returns its id.
func (s *SQLite) AddAgentSchedule(sc AgentSchedule) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO agent_schedules (window_id,agent,cron,prompt,paused,created_by,created_at,last_run_at,next_run_at)
VALUES (?,?,?,?,?,?,?,?,?)`,
		sc.WindowID, sc.Agent, sc.Cron, sc.Prompt, sc.Paused, sc.CreatedBy, sc.CreatedAt, sc.LastRunAt, sc.NextRunAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AgentSchedule returns one schedule by id, or nil when it does not exist.
func (s *SQLite) AgentSchedule(id int64) (*AgentSchedule, error) {
	list, err := s.querySchedules(`SELECT `+scheduleCols+` FROM agent_schedules WHERE id=?`, id)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// AgentSchedulesFor lists an instance's schedules, oldest first.
func (s *SQLite) AgentSchedulesFor(windowID, agent string) ([]AgentSchedule, error) {
	return s.querySchedules(`SELECT `+scheduleCols+` FROM agent_schedules WHERE window_id=? AND agent=? ORDER BY id`, windowID, agent)
}

// DueAgentSchedules lists every unpaused schedule whose next run is at or
// before now, oldest due first.
func (s *SQLite) DueAgentSchedules(nowMillis int64) ([]AgentSchedule, error) {
	return s.querySchedules(`SELECT `+scheduleCols+` FROM agent_schedules WHERE paused=0 AND next_run_at<=? ORDER BY next_run_at, id`, nowMillis)
}

// SetAgentSchedulePaused flips the pause flag; a resume passes the
// recomputed next run so a paused-through run does not fire on resume.
func (s *SQLite) SetAgentSchedulePaused(id int64, paused bool, nextRun int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE agent_schedules SET paused=?, next_run_at=? WHERE id=?`, paused, nextRun, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkAgentScheduleRun records a run (or a deliberate skip when ranAt is 0)
// and the run after it.
func (s *SQLite) MarkAgentScheduleRun(id int64, ranAt, nextRun int64) error {
	if ranAt == 0 {
		_, err := s.db.Exec(`UPDATE agent_schedules SET next_run_at=? WHERE id=?`, nextRun, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE agent_schedules SET last_run_at=?, next_run_at=? WHERE id=?`, ranAt, nextRun, id)
	return err
}

// DeleteAgentSchedule removes one schedule; false when there was none.
func (s *SQLite) DeleteAgentSchedule(id int64) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM agent_schedules WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *SQLite) querySchedules(query string, args ...any) ([]AgentSchedule, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentSchedule{}
	for rows.Next() {
		var sc AgentSchedule
		if err := rows.Scan(&sc.ID, &sc.WindowID, &sc.Agent, &sc.Cron, &sc.Prompt, &sc.Paused,
			&sc.CreatedBy, &sc.CreatedAt, &sc.LastRunAt, &sc.NextRunAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

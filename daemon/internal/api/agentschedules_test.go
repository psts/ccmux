package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/agent/agenttest"
	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
)

// failingRuns is the schedule store with the one write that must not stay
// quiet made to fail.
type failingRuns struct{ *store.SQLite }

func (failingRuns) MarkAgentScheduleRun(int64, int64, int64) error { return errors.New("disk full") }

// A run whose next slot cannot be saved is paused, so the row does not
// read as due again next tick and repeat the prompt.
func TestSchedules_FailedRunWriteParksTheSchedule(t *testing.T) {
	s, st, winID := scheduleServer(t)
	s.SetSchedules(failingRuns{st}, 6*time.Hour)
	now := s.scheduleNow()
	due := now.Add(-7 * time.Hour).UnixMilli() // the missed-slot path reaches markRun without a pane
	id, _ := st.AddAgentSchedule(store.AgentSchedule{WindowID: winID, Agent: "scout", Cron: "@hourly", Prompt: "x", NextRunAt: due})
	s.fireDueSchedules()
	sc, _ := st.AgentSchedule(id)
	if sc == nil || !sc.Paused || sc.NextRunAt != due {
		t.Fatalf("after a failed write = %+v, want paused with the slot untouched", sc)
	}
}

// failingLists is the schedule store with the count query broken.
type failingLists struct{ *store.SQLite }

func (failingLists) AgentSchedulesFor(string, string) ([]store.AgentSchedule, error) {
	return nil, errors.New("database is locked")
}

// A broken count query is a display problem, not a reason to hide the
// instance list or refuse a restart: both routes still answer.
func TestSchedules_CountErrorDoesNotBlockInstancesOrRestart(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	f.srv.SetSchedules(failingLists{f.st}, 6*time.Hour)
	if code, _ := f.start(t, "x-poster", ""); code != 201 {
		t.Fatalf("start = %d", code)
	}
	rec := do(t, f.srv, "GET", "/v1/agents/x-poster/instances", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"schedules":0`) || !strings.Contains(rec.Body.String(), `"window":"Chart Labs"`) {
		t.Fatalf("instances with a broken count = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "POST", "/v1/agents/x-poster/instances/restart", ""); rec.Code != 202 {
		t.Fatalf("restart with a broken count = %d %s", rec.Code, rec.Body)
	}
}

// The instances payload carries each instance's schedule count, which is
// all the editor rows in both lenses read.
func TestSchedules_InstancesCarryTheCount(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	f.srv.SetSchedules(f.st, 6*time.Hour)
	if code, _ := f.start(t, "x-poster", ""); code != 201 {
		t.Fatalf("start = %d", code)
	}
	for _, agentName := range []string{"x-poster", "x-poster", "someone-else"} {
		f.st.AddAgentSchedule(store.AgentSchedule{WindowID: f.winID, Agent: agentName, Cron: "@hourly", Prompt: "x", NextRunAt: 1})
	}
	rec := do(t, f.srv, "GET", "/v1/agents/x-poster/instances", "")
	var out struct {
		Instances []agentDeployment `json:"instances"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || len(out.Instances) != 1 || out.Instances[0].Schedules != 2 {
		t.Fatalf("instances = %d %s", rec.Code, rec.Body)
	}
}

// scheduleServer is an agents server with a schedule store, a fixed clock
// (Wednesday 9 Sep 2026, noon local) and one window holding no session.
func scheduleServer(t *testing.T) (*Server, *store.SQLite, string) {
	t.Helper()
	s := agentsServer(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s.SetSchedules(st, 6*time.Hour)
	s.scheduleNow = func() time.Time { return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.Local) }
	winID, err := s.mgr.EnsureWindow("Chart Labs")
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, s, "PUT", "/v1/agents/scout", `{"icon":"🔍","description":"Reads.","instructions":"# Role\n"}`); rec.Code != 200 {
		t.Fatalf("put agent = %d %s", rec.Code, rec.Body)
	}
	return s, st, winID
}

func schedulesOf(t *testing.T, s *Server, winID string) []scheduleView {
	t.Helper()
	rec := do(t, s, "GET", "/v1/windows/"+winID+"/agents/scout/schedules", "")
	if rec.Code != 200 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Schedules []scheduleView `json:"schedules"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Schedules
}

func TestSchedules_AddPauseResumeDeleteThroughWindowRoutes(t *testing.T) {
	s, _, winID := scheduleServer(t)
	if list := schedulesOf(t, s, winID); len(list) != 0 {
		t.Fatalf("fresh list = %+v", list)
	}
	msg, status, err := s.applyScheduleAction(winID, "scout", "pane-1", scheduleReq{Action: "add", Cron: "0 7 * * 1", Prompt: "Run the weekly sweep."})
	if err != nil || status != 0 || !strings.Contains(msg, "Next run Mon 14 Sep 07:00") {
		t.Fatalf("add = %q, %d, %v", msg, status, err)
	}
	for _, bad := range []scheduleReq{
		{Action: "add", Cron: "7 * *", Prompt: "x"},
		{Action: "add", Cron: "0 7 * * 1", Prompt: ""},
		{Action: "add", Cron: "0 0 31 feb *", Prompt: "never"},
		{Action: "dance"},
	} {
		if _, status, err := s.applyScheduleAction(winID, "scout", "pane-1", bad); status != http.StatusBadRequest || err == nil {
			t.Fatalf("%+v accepted: %d, %v", bad, status, err)
		}
	}
	list := schedulesOf(t, s, winID)
	if len(list) != 1 || list[0].Cron != "0 7 * * 1" || list[0].CreatedBy != "pane-1" || list[0].NextRun == "" {
		t.Fatalf("after add = %+v", list)
	}
	monday := time.Date(2026, time.September, 14, 7, 0, 0, 0, time.Local).UnixMilli()
	if list[0].NextRunAt != monday {
		t.Fatalf("next = %d, want %d", list[0].NextRunAt, monday)
	}
	id := list[0].ID
	if msg, _, err := s.applyScheduleAction(winID, "scout", "p", scheduleReq{Action: "pause", ID: id}); err != nil || msg != "Paused #"+itoa(id)+"." {
		t.Fatalf("pause = %q, %v", msg, err)
	}
	if list := schedulesOf(t, s, winID); !list[0].Paused || list[0].NextRun != "" {
		t.Fatalf("paused view = %+v", list[0])
	}
	if msg, _, err := s.applyScheduleAction(winID, "scout", "p", scheduleReq{Action: "resume", ID: id}); err != nil || !strings.Contains(msg, "Next run Mon 14 Sep 07:00") {
		t.Fatalf("resume = %q, %v", msg, err)
	}
	// Another agent's id is nobody's business: 404.
	if _, status, err := s.applyScheduleAction(winID, "writer", "p", scheduleReq{Action: "remove", ID: id}); status != 404 || err == nil {
		t.Fatalf("remove as writer = %d, %v", status, err)
	}
	if msg, _, err := s.applyScheduleAction(winID, "scout", "p", scheduleReq{Action: "remove", ID: id}); err != nil || msg != "Removed #"+itoa(id)+"." {
		t.Fatalf("remove = %q, %v", msg, err)
	}
	if list := schedulesOf(t, s, winID); len(list) != 0 {
		t.Fatalf("after delete = %+v", list)
	}
	if rec := do(t, s, "GET", "/v1/windows/nope/agents/scout/schedules", ""); rec.Code != 404 {
		t.Fatalf("unknown window = %d", rec.Code)
	}
	if rec := do(t, agentsServer(t), "GET", "/v1/windows/x/agents/scout/schedules", ""); rec.Code != 503 {
		t.Fatalf("no store = %d, want 503", rec.Code)
	}
}

func TestSchedules_CapPerInstance(t *testing.T) {
	s, _, winID := scheduleServer(t)
	for i := 0; i < maxSchedulesPerInstance; i++ {
		if _, _, err := s.applyScheduleAction(winID, "scout", "p", scheduleReq{Action: "add", Cron: "@hourly", Prompt: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, status, err := s.applyScheduleAction(winID, "scout", "p", scheduleReq{Action: "add", Cron: "@hourly", Prompt: "one too many"}); status != http.StatusConflict || err == nil {
		t.Fatalf("over the cap = %d, %v", status, err)
	}
}

// The tick without tmux: everything that does not reach a pane.
func TestSchedules_TickSkipsMissedSlotsAndDropsOrphans(t *testing.T) {
	s, st, winID := scheduleServer(t)
	now := s.scheduleNow()
	missed, _ := st.AddAgentSchedule(store.AgentSchedule{WindowID: winID, Agent: "scout", Cron: "0 7 * * 1", Prompt: "late", NextRunAt: now.Add(-7 * time.Hour).UnixMilli()})
	noBase, _ := st.AddAgentSchedule(store.AgentSchedule{WindowID: winID, Agent: "ghost", Cron: "@hourly", Prompt: "x", NextRunAt: now.UnixMilli()})
	noWindow, _ := st.AddAgentSchedule(store.AgentSchedule{WindowID: "gone", Agent: "scout", Cron: "@hourly", Prompt: "x", NextRunAt: now.UnixMilli()})
	broken, _ := st.AddAgentSchedule(store.AgentSchedule{WindowID: winID, Agent: "scout", Cron: "not cron", Prompt: "x", NextRunAt: now.UnixMilli()})
	future, _ := st.AddAgentSchedule(store.AgentSchedule{WindowID: winID, Agent: "scout", Cron: "@hourly", Prompt: "x", NextRunAt: now.Add(time.Hour).UnixMilli()})

	s.fireDueSchedules()

	if sc, _ := st.AgentSchedule(missed); sc.LastRunAt != 0 || sc.NextRunAt <= now.UnixMilli() {
		t.Fatalf("missed slot should skip ahead without a run: %+v", sc)
	}
	if sc, _ := st.AgentSchedule(noBase); sc != nil {
		t.Fatalf("deleted base's schedule kept: %+v", sc)
	}
	if sc, _ := st.AgentSchedule(noWindow); sc != nil {
		t.Fatalf("gone window's schedule kept: %+v", sc)
	}
	if sc, _ := st.AgentSchedule(broken); !sc.Paused {
		t.Fatalf("unparseable cron not paused: %+v", sc)
	}
	if sc, _ := st.AgentSchedule(future); sc.NextRunAt != now.Add(time.Hour).UnixMilli() {
		t.Fatalf("future row touched: %+v", sc)
	}
}

// The tick with tmux: a due schedule for an instance not yet in the window
// starts it with the prompt, stamps the run and moves to the next slot.
func TestSchedules_TickStartsTheInstanceAndPaneRouteWorks(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 1;:")
	f.srv.SetSchedules(f.st, 6*time.Hour)
	before := time.Now()
	id, _ := f.st.AddAgentSchedule(store.AgentSchedule{WindowID: f.winID, Agent: "x-poster", Cron: "@hourly", Prompt: "post the digest", NextRunAt: before.Add(-time.Second).UnixMilli()})

	f.srv.fireDueSchedules()

	sc, _ := f.st.AgentSchedule(id)
	if sc.LastRunAt < before.UnixMilli() || sc.NextRunAt <= before.UnixMilli() {
		t.Fatalf("not stamped as run: %+v", sc)
	}
	if a := f.list(t); a.State == "absent" {
		t.Fatalf("instance not started: %+v", a)
	}
	f.waitState(t, "asleep") // the noop harness exits at once
	ws := f.workspace(t, f.list(t).Workspace)
	if ws["createdBy"] != scheduleSource {
		t.Fatalf("session createdBy = %v, want %q", ws["createdBy"], scheduleSource)
	}

	// The pane route, as the shim's tool calls it: from the agent's own pane.
	pane := f.list(t).Pane
	rec := do(t, f.srv, "POST", "/v1/panes/"+pane+"/schedules", `{"action":"add","cron":"*/5 * * * *","prompt":"poll"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Scheduled #") || !strings.Contains(rec.Body.String(), `"cron":"*/5 * * * *"`) {
		t.Fatalf("pane add = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "POST", "/v1/panes/"+pane+"/schedules", `{"action":"list"}`); rec.Code != 200 || strings.Count(rec.Body.String(), `"id":`) != 2 {
		t.Fatalf("pane list = %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "POST", "/v1/panes/"+f.ws.Panes[0].ID+"/schedules", `{"action":"list"}`); rec.Code != 400 {
		t.Fatalf("a plain pane may not schedule: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, f.srv, "POST", "/v1/panes/nope/schedules", `{"action":"list"}`); rec.Code != 404 {
		t.Fatalf("unknown pane = %d", rec.Code)
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// The tick against a RUNNING opencode instance: an idle one gets the prompt
// pushed and the row stamped; a busy one is left alone, row untouched, so
// the next tick retries.
func TestSchedules_TickPushesIntoIdleAndWaitsForBusy(t *testing.T) {
	oc := agenttest.NewFakeOpencode()
	defer oc.Server.Close()
	f := newWindowAgentFixture(t, "sleep 4;: --port "+strconv.Itoa(oc.Port()))
	f.srv.SetSchedules(f.st, 6*time.Hour)
	code, pane := f.start(t, "x-poster", "")
	if code != 201 {
		t.Fatalf("start = %d", code)
	}
	f.waitState(t, "running")
	now := time.Now()
	pushed, _ := f.st.AddAgentSchedule(store.AgentSchedule{WindowID: f.winID, Agent: "x-poster", Cron: "@hourly", Prompt: "post the digest", NextRunAt: now.Add(-time.Second).UnixMilli()})

	f.srv.fireDueSchedules()

	prompts, _, _ := oc.Recorded()
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "[ccmux schedule #") || !strings.HasSuffix(prompts[0], "post the digest") {
		t.Fatalf("pushed prompts = %q", prompts)
	}
	if sc, _ := f.st.AgentSchedule(pushed); sc == nil || sc.LastRunAt == 0 || sc.NextRunAt <= now.UnixMilli() {
		t.Fatalf("pushed row not stamped: %+v", sc)
	}

	// Busy: the row waits, nothing is pushed, nothing is stamped.
	f.srv.mgr.ApplyAgentSignal(pane.ID, model.AttentionRunning, true)
	waiting, _ := f.st.AddAgentSchedule(store.AgentSchedule{WindowID: f.winID, Agent: "x-poster", Cron: "@hourly", Prompt: "later", NextRunAt: now.Add(-time.Second).UnixMilli()})
	f.srv.fireDueSchedules()
	if prompts, _, _ := oc.Recorded(); len(prompts) != 1 {
		t.Fatalf("a busy agent was pushed into: %q", prompts)
	}
	if sc, _ := f.st.AgentSchedule(waiting); sc == nil || sc.LastRunAt != 0 || sc.NextRunAt != now.Add(-time.Second).UnixMilli() {
		t.Fatalf("waiting row was touched: %+v", sc)
	}
}

// The tick against a running instance with no opencode port (a Claude
// harness): no chat path, so the slot is skipped, not run and not retried.
func TestSchedules_TickSkipsRunningClaudeInstance(t *testing.T) {
	f := newWindowAgentFixture(t, "sleep 4;:")
	f.srv.SetSchedules(f.st, 6*time.Hour)
	if code, _ := f.start(t, "x-poster", ""); code != 201 {
		t.Fatalf("start = %d", code)
	}
	f.waitState(t, "running")
	now := time.Now()
	id, _ := f.st.AddAgentSchedule(store.AgentSchedule{WindowID: f.winID, Agent: "x-poster", Cron: "@hourly", Prompt: "x", NextRunAt: now.Add(-time.Second).UnixMilli()})
	f.srv.fireDueSchedules()
	if sc, _ := f.st.AgentSchedule(id); sc == nil || sc.LastRunAt != 0 || sc.NextRunAt <= now.UnixMilli() {
		t.Fatalf("skip should advance without a run: %+v", sc)
	}
}

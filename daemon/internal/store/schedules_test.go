package store

import "testing"

func TestAgentSchedules_AddListDueAndDelete(t *testing.T) {
	st := taskStore(t)
	id1, err := st.AddAgentSchedule(AgentSchedule{WindowID: "w1", Agent: "scout", Cron: "0 7 * * 1", Prompt: "sweep", CreatedBy: "pane-a", CreatedAt: 100, NextRunAt: 1000})
	if err != nil || id1 == 0 {
		t.Fatalf("add: %d, %v", id1, err)
	}
	id2, _ := st.AddAgentSchedule(AgentSchedule{WindowID: "w1", Agent: "scout", Cron: "@hourly", Prompt: "poll", CreatedAt: 101, NextRunAt: 5000})
	st.AddAgentSchedule(AgentSchedule{WindowID: "w2", Agent: "scout", Cron: "@daily", Prompt: "other window", CreatedAt: 102, NextRunAt: 10})

	list, err := st.AgentSchedulesFor("w1", "scout")
	if err != nil || len(list) != 2 || list[0].ID != id1 || list[1].ID != id2 || list[0].Prompt != "sweep" {
		t.Fatalf("for w1/scout = %+v, %v", list, err)
	}
	if got, _ := st.AgentSchedulesFor("w1", "writer"); len(got) != 0 {
		t.Fatalf("another agent's list = %+v", got)
	}

	due, err := st.DueAgentSchedules(1000)
	if err != nil || len(due) != 2 || due[0].WindowID != "w2" || due[1].ID != id1 {
		t.Fatalf("due at 1000 = %+v, %v", due, err)
	}
	if err := st.MarkAgentScheduleRun(id1, 1000, 8000); err != nil {
		t.Fatal(err)
	}
	one, _ := st.AgentSchedule(id1)
	if one == nil || one.LastRunAt != 1000 || one.NextRunAt != 8000 {
		t.Fatalf("after run = %+v", one)
	}
	// A skip advances next without claiming a run.
	st.MarkAgentScheduleRun(id1, 0, 9000)
	one, _ = st.AgentSchedule(id1)
	if one.LastRunAt != 1000 || one.NextRunAt != 9000 {
		t.Fatalf("after skip = %+v", one)
	}

	ok, err := st.DeleteAgentSchedule(id2)
	if err != nil || !ok {
		t.Fatalf("delete = %v, %v", ok, err)
	}
	if ok, _ := st.DeleteAgentSchedule(id2); ok {
		t.Fatal("second delete claimed a row")
	}
	if missing, _ := st.AgentSchedule(id2); missing != nil {
		t.Fatalf("deleted still there: %+v", missing)
	}
}

func TestAgentSchedules_PausedOnesAreNotDue(t *testing.T) {
	st := taskStore(t)
	id, _ := st.AddAgentSchedule(AgentSchedule{WindowID: "w", Agent: "a", Cron: "@hourly", Prompt: "p", NextRunAt: 10})
	if ok, err := st.SetAgentSchedulePaused(id, true, 10); err != nil || !ok {
		t.Fatalf("pause = %v, %v", ok, err)
	}
	if due, _ := st.DueAgentSchedules(100); len(due) != 0 {
		t.Fatalf("paused is due: %+v", due)
	}
	// Resume carries the recomputed next run, so the paused-through one is gone.
	st.SetAgentSchedulePaused(id, false, 500)
	if due, _ := st.DueAgentSchedules(100); len(due) != 0 {
		t.Fatalf("resumed fired the old slot: %+v", due)
	}
	if due, _ := st.DueAgentSchedules(500); len(due) != 1 || due[0].Paused {
		t.Fatalf("resumed not due at its new slot: %+v", due)
	}
	if ok, _ := st.SetAgentSchedulePaused(999, true, 0); ok {
		t.Fatal("pausing a missing row claimed success")
	}
}

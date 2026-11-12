package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// report is a worker's report the size they really are: a heading, then
// a few thousand characters over many lines.
func report(lines int) string {
	return "DECISIONS I MADE\n" + strings.Repeat("- Plan said one thing / I did another / why: it was needed.\n", lines)
}

// builtRepo is a repo's queue after a batch: done tasks #1..done, each with
// a long report, then open tasks up to #done+open. Later ones finished later.
func builtRepo(done, open int) ([]store.Task, []store.Part) {
	var tasks []store.Task
	var parts []store.Part
	for i := 1; i <= done+open; i++ {
		id := int64(i)
		t := store.Task{ID: id, Title: fmt.Sprintf("task %d", i), Status: taskq.Waiting, UpdatedAt: id}
		p := store.Part{ID: id, TaskID: id, RepoPath: "/p/app", Status: taskq.Pending, Plan: strings.Repeat("plan ", 600)}
		if i <= done {
			t.Status, t.Note = taskq.Done, "merged app into dev"
			p.Status, p.Note, p.Model, p.Effort = taskq.Ready, report(60), "opus", "medium"
		}
		tasks, parts = append(tasks, t), append(parts, p)
	}
	return tasks, parts
}

func idsOf(tasks []toolTask) []int64 {
	out := make([]int64, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return out
}

// The list a session gets shows every open task and only the latest
// finished ones, and says how many it left out. On kullio, 2026-11-12, it
// listed all 33 with every worker's whole report: 124,000 characters, which
// Claude Code refused, so each worker's report came back as an error.
func TestToolAnswer_ListsOpenTasksAndTheLatestFinished(t *testing.T) {
	tasks, parts := builtRepo(33, 2)
	got := toolAnswerOf(tasks, parts, toolShows{})
	want := []int64{24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34, 35}
	if fmt.Sprint(idsOf(got.Tasks)) != fmt.Sprint(want) || got.Older != 23 {
		t.Fatalf("listed %v with %d older, want %v with 23 older", idsOf(got.Tasks), got.Older, want)
	}
	body, _ := json.Marshal(got.Tasks)
	if len(body) > 8000 {
		t.Fatalf("the answer is %d characters; a batch's worth has to stay small", len(body))
	}
	if strings.Contains(string(body), "plan plan") {
		t.Fatal("the answer carries the parts' plans, which the tool never shows")
	}
}

// A report is cut to one short line that says where the rest is.
func TestToolAnswer_CutsNotesToOneLine(t *testing.T) {
	tasks, parts := builtRepo(1, 0)
	tasks[0].Note = "merge failed\n" + strings.Repeat("a line of check output\n", 80)
	got := toolAnswerOf(tasks, parts, toolShows{}).Tasks[0]
	for name, note := range map[string]string{"task": got.Note, "part": got.Parts[0].Note} {
		if strings.Contains(note, "\n") || len(note) > 300 || !strings.Contains(note, "list with id 1") {
			t.Errorf("%s note = %q, want one short line that names list with id 1", name, note)
		}
	}
	if !strings.HasPrefix(got.Parts[0].Note, "DECISIONS I MADE - Plan said one thing") {
		t.Errorf("part note = %q, want it to start with the report's own first words", got.Parts[0].Note)
	}
	short := toolAnswerOf(tasks[:1], []store.Part{{TaskID: 1, RepoPath: "/p/app", Note: "route added"}}, toolShows{})
	if note := short.Tasks[0].Parts[0].Note; note != "route added" {
		t.Errorf("a short note = %q, want it untouched", note)
	}
}

// Naming one task shows it alone, and whole when asked: that is how a
// session still reads a worker's full report.
func TestToolAnswer_OneTask(t *testing.T) {
	tasks, parts := builtRepo(33, 2)
	whole := toolAnswerOf(tasks, parts, toolShows{only: 7, whole: true})
	if fmt.Sprint(idsOf(whole.Tasks)) != "[7]" || whole.Older != 0 || whole.Tasks[0].Parts[0].Note != report(60) {
		t.Fatalf("list with id 7 = %v, %d older, note of %d characters; want #7 alone with its whole report",
			idsOf(whole.Tasks), whole.Older, len(whole.Tasks[0].Parts[0].Note))
	}
	cut := toolAnswerOf(tasks, parts, toolShows{only: 7})
	if fmt.Sprint(idsOf(cut.Tasks)) != "[7]" || len(cut.Tasks[0].Parts[0].Note) > 300 {
		t.Fatalf("a builder's own task = %v with a note of %d characters; want #7 alone, cut", idsOf(cut.Tasks), len(cut.Tasks[0].Parts[0].Note))
	}
}

// What an action shows: a builder its own task, a list that names a task
// that one whole, anything else the short list. A task with no part in the
// caller's repo is not there to name.
func TestTasks_WhatTheToolShows(t *testing.T) {
	s := taskServer(t)
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "one", Plan: "p"})
	call(t, s, appRepo, paneTaskReq{Action: "file", Title: "two", Plan: "p"})
	call(t, s, backendRepo, paneTaskReq{Action: "file", Title: "theirs", Plan: "p"})
	parts, _ := s.tasks.Parts()
	parts[1].Status, parts[1].PaneID = taskq.Running, "worker-pane"
	s.tasks.SavePart(parts[1])
	worker := appRepo
	worker.pane = "worker-pane"

	cases := []struct {
		who  taskFiler
		req  paneTaskReq
		want toolShows
	}{
		{worker, paneTaskReq{Action: "report", Status: "done"}, toolShows{only: 2}},
		{worker, paneTaskReq{Action: "add_part", Repo: "admin"}, toolShows{only: 2}},
		{appRepo, paneTaskReq{Action: "list", ID: 1}, toolShows{only: 1, whole: true}},
		{appRepo, paneTaskReq{Action: "list"}, toolShows{}},
		{appRepo, paneTaskReq{Action: "edit", ID: 1}, toolShows{}},
	}
	for _, c := range cases {
		if got, _, err := s.toolShowsFor(c.who, c.req); err != nil || got != c.want {
			t.Errorf("%s %d shows %+v (%v), want %+v", c.req.Action, c.req.ID, got, err, c.want)
		}
	}
	if _, status, err := s.toolShowsFor(appRepo, paneTaskReq{Action: "list", ID: 3}); err == nil || status != 404 {
		t.Errorf("list of another repo's task = %d %v, want a 404", status, err)
	}
}

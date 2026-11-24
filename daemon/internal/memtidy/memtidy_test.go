package memtidy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

func TestDigestKeepsWordsAndToolCallsNotResults(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.jsonl")
	lines := []string{
		`{"type":"user","message":{"content":"make the export faster"}}`,
		`{"type":"user","isMeta":true,"message":{"content":"<meta>"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Reading it."},{"type":"tool_use","name":"Read","input":{"file_path":"/r/x.go"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"SECRET TOOL OUTPUT"}]}}`,
		`not json`,
	}
	os.WriteFile(f, []byte(strings.Join(lines, "\n")), 0o644)
	got, err := digest(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Person: make the export faster", "Claude: Reading it.", `[tool Read {"file_path":"/r/x.go"}]`} {
		if !strings.Contains(got, want) {
			t.Errorf("digest lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET TOOL OUTPUT") || strings.Contains(got, "<meta>") {
		t.Errorf("tool results and meta lines are left out:\n%s", got)
	}
}

func TestHeadTailKeepsBothEnds(t *testing.T) {
	s := strings.Repeat("a", 50) + strings.Repeat("b", 50)
	got := headTail(s, 10, 10)
	if !strings.HasPrefix(got, "aaaaaaaaaa\n") || !strings.HasSuffix(got, "bbbbbbbbbb") {
		t.Fatalf("got %q", got)
	}
}

func TestReposGroupWorktreesUnderTheirRepoAndLeaveAgentsOut(t *testing.T) {
	rows := []store.RunnerSession{
		{Kind: store.SessionLead, Cwd: "/r/app"},
		{Role: "builder", Repo: "/r/app", Cwd: "/w/app-task-1", Worktree: "/w/app-task-1"},
		{Kind: store.SessionAgent, Cwd: "/agents/radar"},
	}
	got := reposOf(rows)
	if len(got) != 1 || got[0].Path != "/r/app" {
		t.Fatalf("repos: %+v", got)
	}
	if strings.Join(got[0].Cwds, ",") != "/r/app,/w/app-task-1" {
		t.Fatalf("cwds: %v", got[0].Cwds)
	}
}

func TestCheckRefusesWhatWouldHarm(t *testing.T) {
	old := files{"MEMORY.md": []byte("- [a](a.md) — x\n"), "a.md": []byte("a")}
	cases := map[string]files{
		"no MEMORY.md":     {"a.md": []byte("a")},
		"past what Claude": {"MEMORY.md": []byte(strings.Repeat("- line\n", indexLines+1)), "a.md": []byte("a")},
		"secret":           {"MEMORY.md": old["MEMORY.md"], "a.md": []byte("key sk-ant-api03-abcdefghijklmnopqrstuvwxyz")},
		"ccmux token":      {"MEMORY.md": old["MEMORY.md"], "a.md": []byte("token tok3n-value-123")},
	}
	for want, new := range cases {
		err := check(old, new, diff(old, new), []string{"tok3n-value-123"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	keep := files{"MEMORY.md": []byte("- [a](a.md) — y\n"), "a.md": []byte("a, and the key sk-ant-api03-abcdefghijklmnopqrstuvwxyz")}
	old["a.md"] = keep["a.md"]
	if err := check(old, keep, diff(old, keep), nil); err != nil {
		t.Errorf("a secret that was already there is not new: %v", err)
	}
}

func TestCheckRefusesRemovingHalfTheNotes(t *testing.T) {
	old, new := files{"MEMORY.md": []byte("i\n")}, files{"MEMORY.md": []byte("i\n")}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		old[n+".md"] = []byte(n)
	}
	new["a.md"] = []byte("a")
	if err := check(old, new, diff(old, new), nil); err == nil || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("got %v", err)
	}
}

// nightFixture is a repo with memory and one conversation from today.
type nightFixture struct {
	t        *Tidier
	live     string
	repoPath string
}

func newNight(t *testing.T, claude func(ctx context.Context, dir, prompt string) (string, error)) nightFixture {
	root := t.TempDir()
	repoPath := filepath.Join(root, "repo")
	os.MkdirAll(filepath.Join(repoPath, ".git"), 0o755)
	projects := filepath.Join(root, "projects")
	live := filepath.Join(projectDir(projects, repoPath), "memory")
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "MEMORY.md"), []byte("- [old](old.md) — old\n"), 0o644)
	os.WriteFile(filepath.Join(live, "old.md"), []byte("old note"), 0o644)
	os.WriteFile(filepath.Join(live, ".consolidate-lock"), []byte("1"), 0o644)
	os.WriteFile(filepath.Join(projectDir(projects, repoPath), "c1.jsonl"), []byte(`{"type":"user","message":{"content":"the dev login is admin"}}`), 0o644)
	now := time.Now().Add(time.Minute)
	tid := &Tidier{
		Sessions: func() ([]store.RunnerSession, error) {
			return []store.RunnerSession{{Kind: store.SessionLead, Cwd: repoPath}}, nil
		},
		Projects: projects, StateDir: filepath.Join(root, "state"), Hour: 0,
		Claude: claude, now: func() time.Time { return now },
	}
	return nightFixture{t: tid, live: live, repoPath: repoPath}
}

func readLog(t *testing.T, dir string) []outcome {
	b, _ := os.ReadFile(filepath.Join(dir, "log.jsonl"))
	var out []outcome
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var o outcome
		if json.Unmarshal([]byte(l), &o) == nil {
			out = append(out, o)
		}
	}
	return out
}

func TestANightTidiesBacksUpAndMovesTheBookmark(t *testing.T) {
	var seen string
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		b, _ := os.ReadFile(filepath.Join(dir, "conversations", mustOne(t, filepath.Join(dir, "conversations"))))
		seen = string(b)
		os.WriteFile(filepath.Join(dir, "memory", "MEMORY.md"), []byte("- [login](login.md) — dev login\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "memory", "login.md"), []byte("the dev login is admin"), 0o644)
		os.Remove(filepath.Join(dir, "memory", "old.md"))
		return "added login, removed old", nil
	})
	f.t.Once(context.Background())
	if !strings.Contains(seen, "the dev login is admin") {
		t.Fatalf("the conversation did not reach Claude: %q", seen)
	}
	got, _ := readNotes(f.live)
	if string(got["login.md"]) != "the dev login is admin" || got["old.md"] != nil {
		t.Fatalf("live memory: %v", got)
	}
	if _, err := os.Stat(filepath.Join(f.live, ".consolidate-lock")); err != nil {
		t.Errorf("a file that is not a note is left alone: %v", err)
	}
	logs := readLog(t, f.t.StateDir)
	if len(logs) != 1 || logs[0].Result != "applied" || logs[0].Answer != "added login, removed old" {
		t.Fatalf("log: %+v", logs)
	}
	backups, _ := filepath.Glob(filepath.Join(f.t.StateDir, "backups", "*", "*", "old.md"))
	if len(backups) != 1 {
		t.Fatalf("backup of the old memory: %v", backups)
	}
	if f.t.due() {
		t.Fatal("tonight's pass is done")
	}
}

func TestAConversationAlreadyReadIsNotReadAgain(t *testing.T) {
	calls := 0
	f := newNight(t, func(context.Context, string, string) (string, error) { calls++; return "", nil })
	f.t.Once(context.Background())
	f.t.LastDayReset(t)
	f.t.Once(context.Background())
	if calls != 1 {
		t.Fatalf("Claude ran %d times; the second night had nothing new", calls)
	}
}

// A night whose Claude failed (a crash, the time limit, no login) or was
// cut off by a deploy leaves the bookmark and the memory, and a cut-off
// one is not marked done.
func TestAFailedOrCutOffNightKeepsTheBookmark(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	for name, claude := range map[string]func(context.Context, string, string) (string, error){
		"failed":  func(context.Context, string, string) (string, error) { return "", errors.New("boom") },
		"cut off": func(context.Context, string, string) (string, error) { cancel(); return "", context.Canceled },
	} {
		f := newNight(t, claude)
		f.t.Once(ctx)
		st, _ := f.t.loadState()
		if _, ok := st.Bookmarks[f.repoPath]; ok {
			t.Errorf("%s: the bookmark moved", name)
		}
		if got, _ := readNotes(f.live); string(got["old.md"]) != "old note" || len(got) != 2 {
			t.Errorf("%s: live memory changed: %v", name, got)
		}
		if name == "cut off" && !f.t.due() {
			t.Error("a night cut off by a deploy is not done")
		}
	}
}

// A night over the budget reads the oldest conversations, says how many
// it left, and stops the bookmark before them; an unreadable one is
// counted, not read.
func TestABusyNightLeavesTheRestForTheNext(t *testing.T) {
	defer func(b int) { repoBudget = b }(repoBudget)
	repoBudget = 300
	var seen []string
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		ents, _ := os.ReadDir(filepath.Join(dir, "conversations"))
		for _, e := range ents {
			seen = append(seen, e.Name())
		}
		return "", nil
	})
	pd := filepath.Dir(f.live)
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"d1", "d2", "d3"} {
		p := filepath.Join(pd, name+".jsonl")
		os.WriteFile(p, []byte(`{"type":"user","message":{"content":"`+strings.Repeat("w", 200)+`"}}`), 0o644)
		os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	bad := filepath.Join(pd, "c0.jsonl")
	os.WriteFile(bad, []byte("x"), 0o000)
	os.Chtimes(bad, base.Add(-time.Minute), base.Add(-time.Minute))
	f.t.Once(context.Background())
	checkBusyNight(t, readLog(t, f.t.StateDir), seen)
	st, _ := f.t.loadState()
	if got := time.UnixMilli(st.Bookmarks[f.repoPath]); !got.Before(base.Add(time.Minute)) {
		t.Fatalf("the bookmark %v must stop before d2", got)
	}
}

func TestASessionWritingMemoryMeanwhileWins(t *testing.T) {
	var f nightFixture
	f = newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		os.WriteFile(filepath.Join(f.live, "fresh.md"), []byte("a session saved this"), 0o644)
		os.WriteFile(filepath.Join(dir, "memory", "old.md"), []byte("tidied"), 0o644)
		return "", nil
	})
	f.t.Once(context.Background())
	got, _ := readNotes(f.live)
	if string(got["old.md"]) != "old note" || string(got["fresh.md"]) != "a session saved this" {
		t.Fatalf("live memory: %v", got)
	}
	st, _ := f.t.loadState()
	if _, ok := st.Bookmarks[f.repoPath]; ok {
		t.Fatal("a skipped night leaves the bookmark, so the next one reads it all again")
	}
}

func TestDreamOnSkipsTheNight(t *testing.T) {
	f := newNight(t, func(context.Context, string, string) (string, error) {
		t.Fatal("no Claude while Claude Code's own auto-dream is on")
		return "", nil
	})
	f.t.DreamOn = func() bool { return true }
	f.t.Once(context.Background())
	if f.t.due() {
		t.Fatal("the night counts as done")
	}
}

func mustOne(t *testing.T, dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 1 {
		t.Fatalf("want one file in %s: %v %v", dir, ents, err)
	}
	return ents[0].Name()
}

// LastDayReset makes tonight's pass due again, as tomorrow would.
func (t *Tidier) LastDayReset(tb testing.TB) {
	st, err := t.loadState()
	if err != nil {
		tb.Fatal(err)
	}
	st.LastDay = ""
	if err := t.saveState(st); err != nil {
		tb.Fatal(err)
	}
}

func TestDreamLiveFollowsClaudeCodesRule(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	off := write("off.json", `{"cachedGrowthBookFeatures":{"tengu_onyx_plover":{"enabled":false,"minHours":24}}}`)
	avail := write("avail.json", `{"cachedGrowthBookFeatures":{"tengu_onyx_plover":{"available":true}}}`)
	on := write("on.json", `{"cachedGrowthBookFeatures":{"tengu_onyx_plover":{"enabled":true}}}`)
	yes, no, none := write("yes.json", `{"autoDreamEnabled":true}`), write("no.json", `{"autoDreamEnabled":false}`), filepath.Join(dir, "missing")
	cases := []struct {
		claude, settings string
		want             bool
	}{
		{off, yes, false}, // this host today: the user's yes, Anthropic's no
		{avail, yes, true},
		{avail, none, false},
		{on, none, true},
		{on, no, false},
		{none, yes, false},
	}
	for i, c := range cases {
		if got := DreamLive(c.claude, c.settings); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func TestTheFirstPassWaitsForTheNextNight(t *testing.T) {
	f := newNight(t, nil)
	if !f.t.due() {
		t.Fatal("the fixture's night is due")
	}
	f.t.startTomorrow()
	if f.t.due() {
		t.Fatal("a host that never ran one starts tomorrow")
	}
	f.t.LastDayReset(t)
	f.t.startTomorrow()
	if !f.t.due() {
		t.Fatal("a host with a state file keeps it")
	}
}

// A session can plant links in a memory folder. The tidy-up must never
// write through one: not a fixed temp name, not the note itself.
func TestApplyNeverWritesThroughAPlantedLink(t *testing.T) {
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		os.WriteFile(filepath.Join(dir, "memory", "MEMORY.md"), []byte("- [old](old.md) — new index\n"), 0o644)
		return "", nil
	})
	outside := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(outside, []byte("keep"), 0o644)
	os.Symlink(outside, filepath.Join(f.live, "MEMORY.md.tmp"))
	f.t.Once(context.Background())
	if b, _ := os.ReadFile(outside); string(b) != "keep" {
		t.Fatalf("the file behind the link was written: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(f.live, "MEMORY.md")); !strings.Contains(string(b), "new index") {
		t.Fatalf("the index was not tidied: %q", b)
	}
	if logs := readLog(t, f.t.StateDir); len(logs) != 1 || logs[0].Result != "applied" {
		t.Fatalf("log: %+v", logs)
	}
}

// A planted folder link, inside the memory or in place of it, never
// carries a write out: that night is refused and logged, and a real
// subfolder (archive/) still takes its notes.
func TestApplyRefusesLinkedFolders(t *testing.T) {
	cases := map[string]func(f nightFixture, outside string){
		"subfolder": func(f nightFixture, outside string) { os.Symlink(outside, filepath.Join(f.live, "agents")) },
		"memory folder": func(f nightFixture, outside string) {
			for _, n := range []string{"MEMORY.md", "old.md"} {
				b, _ := os.ReadFile(filepath.Join(f.live, n))
				os.WriteFile(filepath.Join(outside, n), b, 0o644)
			}
			os.RemoveAll(f.live)
			os.Symlink(outside, f.live)
		},
	}
	for name, plant := range cases {
		f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
			m := filepath.Join(dir, "memory")
			os.WriteFile(filepath.Join(m, "MEMORY.md"), []byte("- [old](old.md) — x\n- [x](agents/x.md) — y\n"), 0o644)
			os.MkdirAll(filepath.Join(m, "agents"), 0o755)
			os.WriteFile(filepath.Join(m, "agents", "x.md"), []byte("planted"), 0o644)
			os.WriteFile(filepath.Join(m, "CLAUDE.md"), []byte("planted"), 0o644)
			return "", nil
		})
		outside := t.TempDir()
		plant(f, outside)
		f.t.Once(context.Background())
		for _, n := range []string{"x.md", "CLAUDE.md"} {
			if _, err := os.Stat(filepath.Join(outside, n)); err == nil {
				t.Errorf("%s: %s was written outside the memory", name, n)
			}
		}
		if logs := readLog(t, f.t.StateDir); len(logs) != 1 || logs[0].Result != "failed" {
			t.Errorf("%s: log: %+v", name, logs)
		}
	}
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		os.MkdirAll(filepath.Join(dir, "memory", "archive"), 0o755)
		os.WriteFile(filepath.Join(dir, "memory", "archive", "a.md"), []byte("kept"), 0o644)
		return "", nil
	})
	os.MkdirAll(filepath.Join(f.live, "archive"), 0o755)
	f.t.Once(context.Background())
	if b, _ := os.ReadFile(filepath.Join(f.live, "archive", "a.md")); string(b) != "kept" {
		t.Fatalf("a real subfolder takes its notes: %q", b)
	}
}

// checkBusyNight is what a night over the budget logs and reads.
func checkBusyNight(t *testing.T, logs []outcome, seen []string) {
	t.Helper()
	if len(logs) != 1 || logs[0].Conversations != 1 || logs[0].Deferred != 3 || len(logs[0].Unreadable) != 1 {
		t.Fatalf("log: %+v (seen %v)", logs, seen)
	}
	if u := logs[0].Unreadable[0]; !strings.Contains(u, "c0.jsonl") || !strings.Contains(u, "permission denied") {
		t.Fatalf("an unreadable conversation is named with why: %q", u)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], "d1") {
		t.Fatalf("the oldest goes first: %v", seen)
	}
}

func TestAnEmptiedNoteIsRemovedAndAnUntouchedOneKept(t *testing.T) {
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		m := filepath.Join(dir, "memory")
		os.WriteFile(filepath.Join(m, "MEMORY.md"), []byte("- [new](new.md) — merged\n"), 0o644)
		os.WriteFile(filepath.Join(m, "new.md"), []byte("merged"), 0o644)
		os.WriteFile(filepath.Join(m, "old.md"), []byte(" \n"), 0o644)
		return "", nil
	})
	os.WriteFile(filepath.Join(f.live, "keep.md"), []byte("keep"), 0o644)
	f.t.Once(context.Background())
	got, _ := readNotes(f.live)
	if _, ok := got["old.md"]; ok || string(got["keep.md"]) != "keep" || string(got["new.md"]) != "merged" {
		t.Fatalf("live memory: %v", got)
	}
}

// A note under a linked folder is refused before anything lands, so the
// live memory is all old, never half new.
func TestALinkedFolderRefusesTheWholeNight(t *testing.T) {
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		m := filepath.Join(dir, "memory")
		os.WriteFile(filepath.Join(m, "MEMORY.md"), []byte("- [x](agents/x.md) — y\n"), 0o644)
		os.WriteFile(filepath.Join(m, "a.md"), []byte("new"), 0o644)
		os.MkdirAll(filepath.Join(m, "agents"), 0o755)
		os.WriteFile(filepath.Join(m, "agents", "x.md"), []byte("planted"), 0o644)
		return "", nil
	})
	os.Symlink(t.TempDir(), filepath.Join(f.live, "agents"))
	f.t.Once(context.Background())
	got, _ := readNotes(f.live)
	if _, ok := got["a.md"]; ok || !strings.Contains(string(got["MEMORY.md"]), "old") {
		t.Fatalf("nothing lands when one note is under a link: %v", got)
	}
}

// A write that fails after others landed says which landed and where the
// old memory is, and the bookmark stays for the next night.
func TestAWriteThatFailsPartwaySaysWhatLanded(t *testing.T) {
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		m := filepath.Join(dir, "memory")
		os.WriteFile(filepath.Join(m, "a.md"), []byte("new"), 0o644)
		os.MkdirAll(filepath.Join(m, "ro"), 0o755)
		os.WriteFile(filepath.Join(m, "ro", "x.md"), []byte("new"), 0o644)
		return "", nil
	})
	ro := filepath.Join(f.live, "ro")
	os.MkdirAll(ro, 0o755)
	os.Chmod(ro, 0o500)
	defer os.Chmod(ro, 0o755)
	f.t.Once(context.Background())
	logs := readLog(t, f.t.StateDir)
	if len(logs) != 1 || logs[0].Result != "partial" || !strings.Contains(logs[0].Why, "a.md") || !strings.Contains(logs[0].Why, "backups") {
		t.Fatalf("log: %+v", logs)
	}
	st, _ := f.t.loadState()
	if _, ok := st.Bookmarks[f.repoPath]; ok {
		t.Fatal("a partial night keeps the bookmark")
	}
}

// A refused night (here: a secret) leaves the live memory byte for byte,
// says why, and moves the bookmark: those conversations are not tried again.
func TestARefusedNightLeavesMemoryAndMovesOn(t *testing.T) {
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		os.WriteFile(filepath.Join(dir, "memory", "old.md"), []byte("token s3cret-api-token-value"), 0o644)
		return "", nil
	})
	f.t.Secrets = func() []string { return []string{"s3cret-api-token-value"} }
	before, _ := readNotes(f.live)
	f.t.Once(context.Background())
	after, _ := readNotes(f.live)
	if !same(before, after) {
		t.Fatalf("live memory changed: %v", after)
	}
	logs := readLog(t, f.t.StateDir)
	if len(logs) != 1 || logs[0].Result != "refused" || !strings.Contains(logs[0].Why, "ccmux token") {
		t.Fatalf("log: %+v", logs)
	}
	st, _ := f.t.loadState()
	if _, ok := st.Bookmarks[f.repoPath]; !ok {
		t.Fatal("a refused night moves the bookmark")
	}
}

func TestPruneBackupsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"2026-11-01T0300", "2026-11-02T0300", "2026-11-03T0300", "2026-11-04T0300"} {
		os.MkdirAll(filepath.Join(dir, n), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o644)
	pruneBackups(dir, 2)
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "2026-11-03T0300,2026-11-04T0300,note.txt" {
		t.Fatalf("kept: %v", names)
	}
}

func TestATranscriptOfAnUnknownFormatIsUnreadable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.jsonl")
	os.WriteFile(p, []byte("not json\nnor this\n"), 0o644)
	if _, err := digest(p); err == nil || !strings.Contains(err.Error(), "2 lines") {
		t.Fatalf("got %v", err)
	}
	q := filepath.Join(t.TempDir(), "d.jsonl")
	os.WriteFile(q, []byte(`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`+"\n"), 0o644)
	if text, err := digest(q); err != nil || strings.TrimSpace(text) != "" {
		t.Fatalf("a known format with nothing to say is empty, not unreadable: %q %v", text, err)
	}
}

// The tidy-up's Claude is walled by its flags: no tool listed, the shell
// and web refused, edits only by acceptEdits, run in its work folder.
func TestHeadlessRunsClaudeWithItsWallsInItsFolder(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\npwd\nfor a in \"$@\"; do echo \"$a\"; done\n"), 0o755)
	work := t.TempDir()
	out, err := Headless(fake, "sonnet", nil, time.Minute)(context.Background(), work, "the prompt")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if real, _ := filepath.EvalSymlinks(work); lines[0] != work && lines[0] != real {
		t.Fatalf("ran in %q, not its work folder", lines[0])
	}
	args := strings.Join(lines[1:], "\n")
	for _, want := range []string{"-p\nthe prompt", "--permission-mode\nacceptEdits", "--setting-sources\nproject", "--strict-mcp-config", "--no-session-persistence"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if !strings.Contains(args, "--disallowedTools\nBash,WebFetch,WebSearch") {
		t.Errorf("the shell and web must be refused:\n%s", args)
	}
	if strings.Contains(args, "--allowedTools") || strings.Contains(args, "--allowed-tools") {
		t.Errorf("no tool may be on the allow list:\n%s", args)
	}
}

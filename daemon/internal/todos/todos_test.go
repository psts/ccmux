package todos

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// book is a Book over a real registry, with a clock the test moves and a
// count of the changes it told the pages about.
func book(t *testing.T) (*Book, *int64, *int) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "todos.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock, told := int64(1000), 0
	b := &Book{Store: st, Now: func() time.Time { return time.UnixMilli(clock) }, Changed: func() { told++ }}
	return b, &clock, &told
}

func add(t *testing.T, b *Book, repo, text, urgency string) store.Todo {
	t.Helper()
	got, _, err := b.Add(repo, text, urgency, "app")
	if err != nil {
		t.Fatalf("add %q: %v", text, err)
	}
	return got
}

func kindOf(err error) int {
	var p *Problem
	if errors.As(err, &p) {
		return p.Kind
	}
	return -1
}

func sp(s string) *string { return &s }

func TestNormalize_TakesAwayWhatAgentsVaryAndKeepsWhatTellsPlacesApart(t *testing.T) {
	same := []string{"Fix the flaky test in store_test.go", "fix the  flaky test in `store_test.go`.", "FIX: the flaky test in store_test.go!"}
	for _, s := range same[1:] {
		if Normalize(s) != Normalize(same[0]) {
			t.Errorf("Normalize(%q) = %q, want it equal to %q", s, Normalize(s), Normalize(same[0]))
		}
	}
	if Normalize("nil check at x.go:12") == Normalize("nil check at x.go:40") {
		t.Error("two line numbers compare as one place")
	}
	if got := Normalize("  ???  "); got != "???" {
		t.Errorf("text with no letters or digits = %q, want it compared as written", got)
	}
}

func TestAdd_TheSameThingSeenAgainIsTheSameItem(t *testing.T) {
	b, _, told := book(t)
	first := add(t, b, "/r/app", "Fix the flaky test", Low)
	if first.ID == 0 || first.Status != Open || first.AddedBy != "app" || first.CreatedAt != 1000 {
		t.Fatalf("first = %+v", first)
	}

	again, added, err := b.Add("/r/app", "fix the flaky test.", Low, "app-task-7")
	if err != nil || added || again.ID != first.ID || again.AddedBy != "app" {
		t.Fatalf("second sighting = %+v, added %v, %v; want the first item back", again, added, err)
	}
	if *told != 1 {
		t.Fatalf("pages told %d times, want 1: a repeat changes nothing", *told)
	}

	// A more urgent sighting raises it; a less urgent one does not lower it.
	raised, added, err := b.Add("/r/app", "Fix the flaky test", High, "app")
	if err != nil || added || raised.ID != first.ID || raised.Urgency != High {
		t.Fatalf("urgent sighting = %+v, added %v, %v; want #%d raised to high", raised, added, err, first.ID)
	}
	lower, _, _ := b.Add("/r/app", "Fix the flaky test", Medium, "app")
	if lower.Urgency != High {
		t.Fatalf("a calmer sighting lowered it to %s", lower.Urgency)
	}

	// Tasked is still unfinished: no second row beside it either.
	if _, err := b.Change(first.ID, Edit{Status: sp(Tasked)}); err != nil {
		t.Fatal(err)
	}
	if again, added, _ := b.Add("/r/app", "Fix the flaky test", Low, "app"); added || again.ID != first.ID {
		t.Fatalf("a tasked item got a second row: %+v", again)
	}

	// Another repo, or the same repo once the item is done, is a new item.
	if other := add(t, b, "/r/blog", "Fix the flaky test", Low); other.ID == first.ID {
		t.Fatal("two repos share one item")
	}
	if _, err := b.Finish(first.ID, "app", ""); err != nil {
		t.Fatal(err)
	}
	if fresh, added, _ := b.Add("/r/app", "Fix the flaky test", Low, "app"); !added || fresh.ID == first.ID {
		t.Fatalf("seen again after it was done = %+v, added %v; want a new item", fresh, added)
	}
	list, _ := b.List("/r/app")
	if len(list) != 2 {
		t.Fatalf("/r/app holds %d rows, want 2 (one done, one new)", len(list))
	}
}

func TestAdd_RefusesWhatIsNotAnItem(t *testing.T) {
	b, _, _ := book(t)
	for name, c := range map[string]struct{ repo, text, urgency string }{
		"no text":     {"/r/app", "   ", Low},
		"too long":    {"/r/app", strings.Repeat("x", MaxText+1), Low},
		"bad urgency": {"/r/app", "Fix it", "urgent"},
		"no repo":     {"", "Fix it", Low},
	} {
		if _, _, err := b.Add(c.repo, c.text, c.urgency, "app"); kindOf(err) != Invalid {
			t.Errorf("%s: err = %v, want an Invalid problem", name, err)
		}
	}
	if got := add(t, b, "/r/app", "  Fix\n  it  ", ""); got.Text != "Fix it" || got.Urgency != Medium {
		t.Fatalf("kept as %+v; want one line and medium when no urgency is given", got)
	}
}

func TestChange_StatusUrgencyAndText(t *testing.T) {
	b, clock, _ := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", Low)

	*clock = 2000
	got, err := b.Change(it.ID, Edit{Urgency: sp(High), Text: sp("Fix the flaky store test")})
	if err != nil || got.Urgency != High || got.Text != "Fix the flaky store test" || got.UpdatedAt != 2000 {
		t.Fatalf("edit = %+v, %v", got, err)
	}

	*clock = 3000
	done, err := b.Finish(it.ID, "app-task-9", "fixed in 1a2b3c")
	if err != nil || done.Status != Done || done.DoneBy != "app-task-9" || done.DoneAt != 3000 || done.Note != "fixed in 1a2b3c" {
		t.Fatalf("finish = %+v, %v", done, err)
	}
	if _, err := b.Change(it.ID, Edit{Status: sp(Tasked)}); kindOf(err) != Conflict {
		t.Fatalf("a done item made a task: err = %v, want a Conflict", err)
	}

	back, err := b.Change(it.ID, Edit{Status: sp(Open)})
	if err != nil || back.Status != Open || back.DoneBy != "" || back.DoneAt != 0 {
		t.Fatalf("reopen = %+v, %v", back, err)
	}
	if _, err := b.Change(it.ID, Edit{Status: sp("later")}); kindOf(err) != Invalid {
		t.Fatalf("status later: err = %v, want Invalid", err)
	}
	if _, err := b.Change(it.ID, Edit{Urgency: sp("urgent")}); kindOf(err) != Invalid {
		t.Fatalf("urgency urgent: err = %v, want Invalid", err)
	}
	if _, err := b.Change(999, Edit{Urgency: sp(Low)}); kindOf(err) != Missing {
		t.Fatalf("no such item: err = %v, want Missing", err)
	}
}

func TestChange_NeverLeavesTwoUnfinishedCopies(t *testing.T) {
	b, _, _ := book(t)
	a := add(t, b, "/r/app", "Fix the flaky test", Low)
	c := add(t, b, "/r/app", "Update the readme", Low)

	if _, err := b.Change(c.ID, Edit{Text: sp("fix the flaky test")}); kindOf(err) != Conflict {
		t.Fatalf("retyped into a copy of #%d: err = %v, want a Conflict", a.ID, err)
	}
	// Done, then seen again, then the old one reopened: the second stands.
	if _, err := b.Finish(a.ID, "app", ""); err != nil {
		t.Fatal(err)
	}
	add(t, b, "/r/app", "Fix the flaky test", Low)
	if _, err := b.Change(a.ID, Edit{Status: sp(Open)}); kindOf(err) != Conflict {
		t.Fatalf("reopened beside its newer copy: err = %v, want a Conflict", err)
	}
	if got, _ := b.Get(a.ID); got.Status != Done {
		t.Fatalf("a refused reopen left #%d as %s", a.ID, got.Status)
	}
}

func TestChange_ThatChangesNothingSavesNothing(t *testing.T) {
	b, clock, told := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", Low)
	*clock, *told = 5000, 0
	got, err := b.Change(it.ID, Edit{Urgency: sp(Low), Status: sp(Open)})
	if err != nil || got.UpdatedAt != 1000 || *told != 0 {
		t.Fatalf("no-op edit = %+v, %v, pages told %d times; want it untouched", got, err, *told)
	}
}

func TestDelete(t *testing.T) {
	b, _, _ := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", Low)
	if err := b.Delete(it.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(it.ID); kindOf(err) != Missing {
		t.Fatalf("second delete: err = %v, want Missing", err)
	}
}

func TestList_ReadsUnfinishedByUrgencyThenNewestThenDone(t *testing.T) {
	b, clock, _ := book(t)
	names := map[int64]string{}
	for i, c := range []struct{ text, urgency string }{
		{"low old", Low}, {"high old", High}, {"medium", Medium}, {"high new", High}, {"done early", High}, {"done late", Low},
	} {
		*clock = int64(1000 + i)
		names[add(t, b, "/r/app", c.text, c.urgency).ID] = c.text
	}
	finish := func(text string, at int64) {
		for id, n := range names {
			if n == text {
				*clock = at
				if _, err := b.Finish(id, "app", ""); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	finish("done early", 2000)
	finish("done late", 3000)

	list, err := b.List("/r/app")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range list {
		got = append(got, it.Text)
	}
	want := "high new, high old, medium, low old, done late, done early"
	if strings.Join(got, ", ") != want {
		t.Fatalf("order = %s\n want %s", strings.Join(got, ", "), want)
	}
	if n := len(Unfinished(list)); n != 4 {
		t.Fatalf("Unfinished = %d items, want 4", n)
	}
}

func TestFiledAndSettle_AnItemFollowsItsTask(t *testing.T) {
	b, clock, told := book(t)
	merged := add(t, b, "/r/app", "Fix the flaky test", Low)
	cancelled := add(t, b, "/r/app", "Update the readme", Low)
	running := add(t, b, "/r/app", "Drop the dead flag", Low)
	removed := add(t, b, "/r/app", "Trim the log line", Low)
	untouched := add(t, b, "/r/app", "Rename the helper", Low)

	if err := b.Fileable(merged.ID, "/r/blog"); kindOf(err) != Missing {
		t.Fatalf("another repo's item: err = %v, want Missing", err)
	}
	if err := b.Fileable(999, "/r/app"); kindOf(err) != Missing {
		t.Fatalf("no such item: err = %v, want Missing", err)
	}
	if err := b.Fileable(merged.ID, "/r/app"); err != nil {
		t.Fatal(err)
	}
	for id, task := range map[int64]int64{merged.ID: 11, cancelled.ID: 12, running.ID: 13, removed.ID: 14} {
		if err := b.Filed(id, task); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := b.Get(merged.ID); got.Status != Tasked || got.TaskID != 11 {
		t.Fatalf("filed = %+v, want tasked under task 11", got)
	}

	states := map[int64]string{11: TaskMerged, 12: TaskCancelled, 13: TaskActive, 14: TaskGone}
	state := func(id int64) string { return states[id] }
	*clock, *told = 9000, 0
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(merged.ID); got.Status != Done || got.DoneBy != "task #11" || got.DoneAt != 9000 {
		t.Fatalf("its task merged: %+v, want done by task #11", got)
	}
	if got, _ := b.Get(cancelled.ID); got.Status != Open || got.TaskID != 12 || !strings.Contains(got.Note, "task #12 was cancelled") {
		t.Fatalf("its task was cancelled: %+v, want it open again, still linked, with a note", got)
	}
	if got, _ := b.Get(running.ID); got.Status != Tasked || got.TaskID != 13 {
		t.Fatalf("its task still runs: %+v, want it left tasked", got)
	}
	if got, _ := b.Get(removed.ID); got.Status != Open || got.TaskID != 0 || !strings.Contains(got.Note, "task #14 was removed") {
		t.Fatalf("its task was removed: %+v, want it open with no task", got)
	}
	if got, _ := b.Get(untouched.ID); got.Status != Open {
		t.Fatalf("an item with no task moved: %+v", got)
	}
	if *told != 3 {
		t.Fatalf("pages told %d times, want 3", *told)
	}

	// A merge saves its task as done twice, and any later pass asks again.
	*told = 0
	if err := b.Settle(state); err != nil || *told != 0 {
		t.Fatalf("settling again: %v, pages told %d times; want nothing to change", err, *told)
	}
	// A cancelled task queued again takes its item with it, to the end.
	states[12] = TaskActive
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(cancelled.ID); got.Status != Tasked || got.Note != "" {
		t.Fatalf("its task was queued again: %+v, want it tasked with the note gone", got)
	}
	states[12] = TaskMerged
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(cancelled.ID); got.Status != Done || got.DoneBy != "task #12" {
		t.Fatalf("then it merged: %+v, want done by task #12", got)
	}
	// A task that cannot be read changes nothing.
	states[13] = ""
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(running.ID); got.Status != Tasked {
		t.Fatalf("an unread task moved its item: %+v", got)
	}
	// Done before its task was filed stays done, with who finished it.
	if _, err := b.Finish(untouched.ID, "app", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.Fileable(untouched.ID, "/r/app"); kindOf(err) != Conflict {
		t.Fatalf("filing a done item: err = %v, want a Conflict", err)
	}
	if err := b.Filed(untouched.ID, 15); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(untouched.ID); got.Status != Done || got.TaskID != 0 || got.DoneBy != "app" {
		t.Fatalf("done item after a late filing: %+v, want it left as it was", got)
	}
}

// gitRepo makes a repo with one commit and returns its folder as git
// names it (a temp folder can sit behind a symlink).
func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// keyOf is Keys.Of for a path it must be able to name.
func keyOf(t *testing.T, k *Keys, path string) string {
	t.Helper()
	key, err := k.Of(path)
	if err != nil {
		t.Fatalf("Of(%q): %v", path, err)
	}
	return key
}

func TestKeys_AWorktreeASubfolderAndASymlinkAreTheirReposList(t *testing.T) {
	repo := gitRepo(t)
	worktree := filepath.Join(filepath.Dir(repo), "wt", "task-7")
	runGit(t, repo, "worktree", "add", "-q", "-b", "task/7", worktree)
	sub := filepath.Join(repo, "daemon", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	inWorktree := filepath.Join(worktree, "deep")
	if err := os.MkdirAll(inWorktree, 0o755); err != nil {
		t.Fatal(err)
	}

	var k Keys
	for name, path := range map[string]string{
		"the repo": repo, "with a slash": repo + "/", "a worktree": worktree, "a folder in a worktree": inWorktree,
		"a subfolder": sub, "a symlink": link,
	} {
		if got := keyOf(t, &k, path); got != repo {
			t.Errorf("%s: Of(%q) = %q, want %q", name, path, got, repo)
		}
	}
}

func TestKeys_AFolderInNoRepoIsItsOwnList(t *testing.T) {
	plain := t.TempDir()
	var k Keys
	if got := keyOf(t, &k, plain+"/"); got != plain {
		t.Errorf("Of(a plain folder) = %q, want %q", got, plain)
	}
	if got := keyOf(t, &k, "/nowhere/at/all"); got != "/nowhere/at/all" {
		t.Errorf("Of(a folder that is not there) = %q, want the path", got)
	}
	if got := keyOf(t, &k, ""); got != "" {
		t.Errorf(`Of("") = %q, want ""`, got)
	}
}

// fakeKeys is Keys over a made-up disk: every folder is there, and git
// answers each one as answers says ("" for a folder it has no line for is
// git's own "not a git repository").
func fakeKeys(answers map[string]func() (string, error), asked map[string]int) *Keys {
	return &Keys{
		Exists: func(string) bool { return true },
		Git: func(dir string, args ...string) (string, error) {
			asked[dir]++
			if answer, ok := answers[dir]; ok {
				return answer()
			}
			return "", errors.New("exit status 128: fatal: not a git repository (or any of the parent directories): .git")
		},
	}
}

func TestKeys_AsksGitOncePerFolderAndAgainForOneThatWasNoRepo(t *testing.T) {
	asked := map[string]int{}
	answers := map[string]func() (string, error){"/r/app/wt": func() (string, error) { return "/r/app/.git", nil }}
	k := fakeKeys(answers, asked)
	for i := 0; i < 3; i++ {
		if got := keyOf(t, k, "/r/app/wt"); got != "/r/app" {
			t.Fatalf("Of = %q, want /r/app", got)
		}
	}
	if asked["/r/app/wt"] != 1 {
		t.Errorf("git asked %d times for one folder, want 1", asked["/r/app/wt"])
	}
	if got := keyOf(t, k, "/r/new"); got != "/r/new" {
		t.Fatalf("Of(no repo yet) = %q", got)
	}
	answers["/r/new"] = func() (string, error) { return "/r/new/.git", nil } // git init ran there since
	if got := keyOf(t, k, "/r/new/"); got != "/r/new" || asked["/r/new"] != 2 {
		t.Errorf("a folder that became a repo: Of = %q after %d asks; its first answer must not be kept", got, asked["/r/new"])
	}
}

// A git that fails for any other reason than "this is no repo" has not
// answered. Its folder gets no key: filed under the worktree's own path,
// an item would be on a list no page shows.
func TestKeys_AGitThatCouldNotAnswerNamesNoList(t *testing.T) {
	asked := map[string]int{}
	fails := map[string]string{
		"/wt/refused": "exit status 128: fatal: detected dubious ownership in repository at '/r/app'",
		"/wt/missing": `exec: "git": executable file not found in $PATH`,
		"/wt/old":     "exit status 129: error: unknown option `path-format=absolute'",
	}
	answers := map[string]func() (string, error){
		"/r/plain": func() (string, error) { return "", errors.New("fatal: not a git repository") },
	}
	for dir, msg := range fails {
		answers[dir] = func() (string, error) { return "", errors.New(msg) }
	}
	k := fakeKeys(answers, asked)
	for dir, msg := range fails {
		key, err := k.Of(dir)
		if err == nil || key != "" || !strings.Contains(err.Error(), msg) || !strings.Contains(err.Error(), dir) {
			t.Errorf("Of(%s) = %q, %v; want no key and an error naming the folder and git's words", dir, key, err)
		}
	}
	// Nothing of a failure is kept: the next ask goes to git again.
	answers["/wt/refused"] = func() (string, error) { return "/r/app/.git", nil }
	if got := keyOf(t, k, "/wt/refused"); got != "/r/app" || asked["/wt/refused"] != 2 {
		t.Errorf("once git answers: Of = %q after %d asks, want /r/app after 2", got, asked["/wt/refused"])
	}
	// "No repo here" is an answer, and stays one.
	if got := keyOf(t, k, "/r/plain"); got != "/r/plain" {
		t.Errorf("Of(a folder in no repo) = %q, want itself", got)
	}
}

func TestKeys_ARepoWithNoDotGitFolderIsNamedByItsTop(t *testing.T) {
	top := func() (string, error) { return "/super/sub", nil }
	k := &Keys{
		Exists: func(string) bool { return true },
		Git: func(dir string, args ...string) (string, error) {
			if args[len(args)-1] == "--git-common-dir" {
				return "/super/.git/modules/sub", nil
			}
			return top()
		},
	}
	if got := keyOf(t, k, "/super/sub/pkg"); got != "/super/sub" {
		t.Errorf("Of(a submodule's folder) = %q, want its top /super/sub", got)
	}
	top = func() (string, error) {
		return "", errors.New("exit status 128: fatal: this operation must be run in a work tree")
	}
	if key, err := k.Of("/bare/repo.git"); err == nil || key != "" {
		t.Errorf("Of(a repo with no work tree) = %q, %v; want no key and git's error", key, err)
	}
}

// An item whose task was cancelled or removed is handed on again: it must
// start clean, with no trace of the old task. Left linked to the cancelled
// one, the page would show that task as having it, and the next pass would
// put the item straight back to open.
func TestTaskedAgain_AnItemLetsGoOfItsOldTask(t *testing.T) {
	b, _, _ := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", Low)
	other := add(t, b, "/r/app", "Update the readme", Low)
	if err := b.Filed(it.ID, 11); err != nil {
		t.Fatal(err)
	}
	if err := b.Filed(other.ID, 12); err != nil {
		t.Fatal(err)
	}
	states := map[int64]string{11: TaskCancelled, 12: TaskActive}
	state := func(id int64) string { return states[id] }
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}

	// A person presses Make task again.
	again, err := b.Change(it.ID, Edit{Status: sp(Tasked)})
	if err != nil || again.Status != Tasked || again.TaskID != 0 || again.Note != "" {
		t.Fatalf("handed on again = %+v, %v; want tasked, with the cancelled task and its note gone", again, err)
	}
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Tasked {
		t.Fatalf("the cancelled task pulled the item back to %s", got.Status)
	}
	// The session files a new task for it, which is merged.
	if err := b.Filed(it.ID, 21); err != nil {
		t.Fatal(err)
	}
	states[21] = TaskMerged
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Done || got.DoneBy != "task #21" || got.Note != "" {
		t.Fatalf("after the new task merged: %+v, want done by task #21 with no stale note", got)
	}
	// The item beside it, whose task is on its way, kept its task throughout.
	if got, _ := b.Get(other.ID); got.Status != Tasked || got.TaskID != 12 {
		t.Fatalf("an item with a live task lost it: %+v", got)
	}

	// A removed task's note goes with the next filing too.
	gone := add(t, b, "/r/app", "Drop the dead flag", Low)
	if err := b.Filed(gone.ID, 31); err != nil {
		t.Fatal(err)
	}
	states[31] = TaskGone
	if err := b.Settle(state); err != nil {
		t.Fatal(err)
	}
	if err := b.Filed(gone.ID, 32); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(gone.ID); got.Status != Tasked || got.TaskID != 32 || got.Note != "" {
		t.Fatalf("filed again after its task was removed: %+v, want tasked under #32 with no note", got)
	}
}

// An item added once is one item while it is not done, whatever its
// numbers say on the next sighting; a higher urgency still raises it, a
// name that only starts the same is another item, and a done one is seen
// again as new.
func TestAddOnceFoldsBySomethingThatNamesTheThing(t *testing.T) {
	b, _, _ := book(t)
	first, added, err := b.AddOnce("/r", "disk / is low (", "disk / is low (91% full, 12 GB free)", "medium", "weekly sweep")
	if err != nil || !added {
		t.Fatalf("first sighting: %v %v", added, err)
	}
	again, added, err := b.AddOnce("/r", "disk / is low (", "disk / is low (96% full, 4 GB free)", "high", "weekly sweep")
	if err != nil || added || again.ID != first.ID || again.Urgency != "high" {
		t.Fatalf("seen again with other numbers is the same item, raised: %+v %v %v", again, added, err)
	}
	if _, added, _ := b.AddOnce("/r", "disk /ext is low (", "disk /ext is low (91% full)", "medium", "x"); !added {
		t.Fatal("another name is another item")
	}
	if _, err := b.Finish(first.ID, "person", "freed"); err != nil {
		t.Fatal(err)
	}
	if _, added, _ := b.AddOnce("/r", "disk / is low (", "disk / is low (92% full)", "medium", "x"); !added {
		t.Fatal("a done item is not the one seen again")
	}
	if _, _, err := b.AddOnce("/r", "elsewhere (", "disk / is low", "medium", "x"); kindOf(err) != Invalid {
		t.Fatalf("a text that does not start with its prefix is refused: %v", err)
	}
}

// A prefix with two spaces in a row (a folder name can have them) finds
// its item again, though the kept text has its spaces folded; another
// folder's item is not taken for it.
func TestAddOnceFindsAPrefixWithFoldedSpaces(t *testing.T) {
	b, _, _ := book(t)
	pre := "Leftover Claude Code worktree /p/my  app/.claude/worktrees/a ("
	if _, added, err := b.AddOnce("/r", pre, pre+"2 GB)", "low", "weekly sweep"); err != nil || !added {
		t.Fatalf("first: %v %v", added, err)
	}
	if _, added, err := b.AddOnce("/r", pre, pre+"3 GB)", "low", "weekly sweep"); err != nil || added {
		t.Fatalf("the same worktree is the same item: %v %v", added, err)
	}
	other := "Leftover Claude Code worktree /p/my app/.claude/worktrees/b ("
	if _, added, err := b.AddOnce("/r", other, other+"1 GB)", "low", "weekly sweep"); err != nil || !added {
		t.Fatalf("another worktree is another item: %v %v", added, err)
	}
}

// Make task puts an item in its repo's planning line; filing takes it out
// as tasked, and its task's merge makes it done: open ↔ planning → tasked →
// done.
func TestPlan_AnItemGoesThroughThePlanningLine(t *testing.T) {
	b, clock, told := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", "low")
	other := add(t, b, "/r/app", "Update the readme", "low")

	*clock = 2000
	now, was, err := b.Plan(it.ID)
	if err != nil || now.Status != Planning || now.PlanningAt != 2000 || was.Status != Open {
		t.Fatalf("planned = %+v (was %+v), %v", now, was, err)
	}
	*clock = 3000
	again, wasAgain, err := b.Plan(it.ID)
	if err != nil || again.PlanningAt != 2000 || wasAgain.Status != Planning {
		t.Fatalf("planned again = %+v (was %+v), %v; want its place in the line kept", again, wasAgain, err)
	}
	if _, _, err := b.Plan(other.ID); err != nil {
		t.Fatal(err)
	}
	line, err := b.Line("/r/app")
	if err != nil || len(line) != 2 || line[0].ID != it.ID || line[1].ID != other.ID {
		t.Fatalf("the line = %+v, %v; want the longest in it first", line, err)
	}
	if blog, _ := b.Line("/r/blog"); len(blog) != 0 {
		t.Fatalf("another repo's line: %+v", blog)
	}

	// Put back: open, and out of the line. Then in again, at the end.
	back, err := b.Change(it.ID, Edit{Status: sp(Open)})
	if err != nil || back.Status != Open || back.PlanningAt != 0 {
		t.Fatalf("put back = %+v, %v", back, err)
	}
	*clock = 4000
	if now, _, _ = b.Plan(it.ID); now.PlanningAt != 4000 {
		t.Fatalf("planned anew = %+v", now)
	}
	if line, _ = b.Line("/r/app"); len(line) != 2 || line[0].ID != other.ID {
		t.Fatalf("an item put back and planned again goes to the end: %+v", line)
	}

	before := *told
	if err := b.Filed(it.ID, 41); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Tasked || got.TaskID != 41 || got.PlanningAt != 0 || *told == before {
		t.Fatalf("filed = %+v (told %d times); want tasked under #41 and out of the line", got, *told-before)
	}
	if err := b.Settle(func(int64) string { return TaskMerged }); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Done {
		t.Fatalf("its task merged: %+v", got)
	}
	if line, _ = b.Line("/r/app"); len(line) != 1 || line[0].ID != other.ID {
		t.Fatalf("the line after the filing: %+v", line)
	}
	// Ticked off by the planner (already fixed, not worth a task): out of the line.
	if got, err := b.Finish(other.ID, "app-planner", "fixed in abc123"); err != nil || got.Status != Done || got.PlanningAt != 0 {
		t.Fatalf("ticked off = %+v, %v", got, err)
	}
}

// What cannot go into the line, and that a status change never puts an
// item there: nothing would have been sent to the planner.
func TestPlan_RefusesWhatIsDoneOrATasksAndIsNoStatusChange(t *testing.T) {
	b, _, _ := book(t)
	done := add(t, b, "/r/app", "Fixed already", "low")
	if _, err := b.Finish(done.ID, "Pat", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Plan(done.ID); kindOf(err) != Conflict {
		t.Fatalf("a done item: %v, want a conflict", err)
	}
	filed := add(t, b, "/r/app", "Has a task", "low")
	if err := b.Filed(filed.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Plan(filed.ID); kindOf(err) != Conflict || !strings.Contains(err.Error(), "task #7") {
		t.Fatalf("an item a task has: %v, want a conflict naming the task", err)
	}
	if _, _, err := b.Plan(999); kindOf(err) != Missing {
		t.Fatalf("no such item: %v", err)
	}
	open := add(t, b, "/r/app", "Open item", "low")
	if _, err := b.Change(open.ID, Edit{Status: sp(Planning)}); kindOf(err) != Invalid || !strings.Contains(err.Error(), "Make task") {
		t.Fatalf("planning as a status change: %v, want it refused with how it is done", err)
	}
	if got, _ := b.Get(open.ID); got.Status != Open {
		t.Fatalf("the refused change moved the item: %+v", got)
	}
}

// An item whose task was cancelled is open and still linked; planned anew
// it lets go of that task, or Settle would pull it back to open. And one
// sent to the repo's session before the line was there (tasked, no task)
// can go into the line.
func TestPlan_LetsGoOfACancelledTaskAndTakesWhatWasSentBefore(t *testing.T) {
	b, _, _ := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", "low")
	if err := b.Filed(it.ID, 11); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(func(int64) string { return TaskCancelled }); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Open || got.TaskID != 11 || got.Note == "" {
		t.Fatalf("its task was cancelled: %+v", got)
	}
	now, was, err := b.Plan(it.ID)
	if err != nil || now.Status != Planning || now.TaskID != 0 || now.Note != "" || was.TaskID != 11 {
		t.Fatalf("planned anew = %+v (was %+v), %v; want the cancelled task and its note gone", now, was, err)
	}
	if err := b.Settle(func(int64) string { return TaskCancelled }); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Planning {
		t.Fatalf("the cancelled task pulled the item out of the line: %+v", got)
	}

	sent := add(t, b, "/r/app", "Sent before the line", "low")
	if _, err := b.Change(sent.ID, Edit{Status: sp(Tasked)}); err != nil {
		t.Fatal(err)
	}
	if now, was, err := b.Plan(sent.ID); err != nil || now.Status != Planning || was.Status != Tasked {
		t.Fatalf("an item sent before = %+v (was %+v), %v", now, was, err)
	}
}

// An item that reached no planner is as it was before; one that has left
// the line since, or was in it before this Make task, is left alone.
func TestUnplan_PutsBackOnlyWhatThisPlanMoved(t *testing.T) {
	b, _, _ := book(t)
	it := add(t, b, "/r/app", "Fix the flaky test", "low")
	if err := b.Filed(it.ID, 11); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(func(int64) string { return TaskCancelled }); err != nil {
		t.Fatal(err)
	}
	_, was, _ := b.Plan(it.ID)
	if err := b.Unplan(was); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Open || got.TaskID != 11 || got.Note != was.Note || got.PlanningAt != 0 {
		t.Fatalf("unplanned = %+v, want it as it was: %+v", got, was)
	}

	// Sent again while already in the line: a failed resend leaves it there.
	first, _, _ := b.Plan(it.ID)
	_, wasPlanning, _ := b.Plan(it.ID)
	if err := b.Unplan(wasPlanning); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(it.ID); got.Status != Planning || got.PlanningAt != first.PlanningAt {
		t.Fatalf("a failed resend took the item out of the line: %+v", got)
	}

	// Filed between the plan and the failure: it is the task's now.
	other := add(t, b, "/r/app", "Update the readme", "low")
	_, wasOpen, _ := b.Plan(other.ID)
	if err := b.Filed(other.ID, 12); err != nil {
		t.Fatal(err)
	}
	if err := b.Unplan(wasOpen); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(other.ID); got.Status != Tasked || got.TaskID != 12 {
		t.Fatalf("unplan undid a filing: %+v", got)
	}
}

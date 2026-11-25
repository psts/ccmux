package memtidy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mapSettings is the settings table in memory.
type mapSettings map[string]string

func (m mapSettings) GetSetting(k string) (string, error) { return m[k], nil }
func (m mapSettings) SetSetting(k, v string) error        { m[k] = v; return nil }

type brokenSettings struct{}

func (brokenSettings) GetSetting(string) (string, error) { return "", errors.New("db gone") }
func (brokenSettings) SetSetting(string, string) error   { return errors.New("db gone") }

func TestARepoIsOnUntilSwitchedOffAndAWorktreeIsItsRepo(t *testing.T) {
	set := mapSettings{}
	now := time.UnixMilli(1_700_000_000_000)
	sw := &Switches{Settings: set, Key: func(p string) (string, error) { return strings.TrimSuffix(p, "-worktree"), nil }, now: func() time.Time { return now }}
	if st, err := sw.State("/r/app"); err != nil || !st.On || !st.OnAt.IsZero() {
		t.Fatalf("no setting is on: %+v %v", st, err)
	}
	if err := sw.Set("/r/app-worktree", false); err != nil {
		t.Fatal(err)
	}
	if set["memory-tidy:/r/app"] != "off" || len(set) != 1 {
		t.Fatalf("a worktree switches its repo: %v", set)
	}
	if on, _ := sw.Enabled("/r/app"); on {
		t.Fatal("off is off")
	}
	sw.Set("/r/app", true)
	later := now.Add(time.Hour)
	sw.now = func() time.Time { return later }
	sw.Set("/r/app", true) // on again: the time it was switched on stays
	if on, at := sw.Enabled("/r/app"); !on || !at.Equal(now) {
		t.Fatalf("switched on at %v: %v %v", now, on, at)
	}
}

func TestASwitchThatCannotBeReadSkipsTheRepo(t *testing.T) {
	for name, sw := range map[string]*Switches{
		"db":    {Settings: brokenSettings{}},
		"value": {Settings: mapSettings{"memory-tidy:/r": "maybe"}},
		"key":   {Settings: mapSettings{}, Key: func(string) (string, error) { return "", errors.New("no git") }},
	} {
		if on, _ := sw.Enabled("/r"); on {
			t.Errorf("%s: an unreadable switch is not on", name)
		}
	}
}

func TestAnOffRepoIsNotReadAndFilesNothing(t *testing.T) {
	f := newNight(t, func(context.Context, string, string) (string, error) {
		t.Fatal("no Claude for a repo that is off")
		return "", nil
	})
	var asked string
	f.t.Enabled = func(p string) (bool, time.Time) { asked = p; return false, time.Time{} }
	f.t.Todo = func(string, string, string) error { t.Fatal("an off repo files nothing"); return nil }
	f.t.Once(context.Background())
	if asked != f.repoPath {
		t.Fatalf("the switch asked was %q", asked)
	}
	st, _ := f.t.loadState()
	if _, ok := st.Bookmarks[f.repoPath]; ok || len(readLog(t, f.t.StateDir)) != 0 {
		t.Fatal("an off repo leaves its bookmark and logs no night")
	}
	if f.t.due() {
		t.Fatal("the night is still done")
	}
}

// A repo switched on again after months reads no further back than two
// days before it was switched on, whatever its old bookmark says.
func TestARepoSwitchedOnAgainDoesNotReadMonths(t *testing.T) {
	var seen []string
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		ents, _ := os.ReadDir(filepath.Join(dir, "conversations"))
		for _, e := range ents {
			seen = append(seen, e.Name())
		}
		return "", nil
	})
	pd := filepath.Dir(f.live)
	old := time.Now().Add(-90 * 24 * time.Hour)
	p := filepath.Join(pd, "months-ago.jsonl")
	os.WriteFile(p, []byte(`{"type":"user","message":{"content":"old"}}`), 0o644)
	os.Chtimes(p, old, old)
	f.t.saveState(state{Bookmarks: map[string]int64{f.repoPath: old.Add(-time.Hour).UnixMilli()}})
	f.t.Enabled = func(string) (bool, time.Time) { return true, time.Now().Add(-time.Hour) }
	f.t.Once(context.Background())
	if len(seen) != 1 || !strings.Contains(seen[0], "c1") {
		t.Fatalf("only today's conversation is read: %v", seen)
	}
}

func TestLastNightIsTheRepoNewestLine(t *testing.T) {
	f := newNight(t, func(context.Context, string, string) (string, error) { return "", nil })
	if n, err := LastNight(f.t.StateDir, f.repoPath); n != nil || err != nil {
		t.Fatalf("no log is no night: %v %v", n, err)
	}
	f.t.record(outcome{At: "a", Repo: f.repoPath, Result: "applied", CostUSD: 0.1})
	f.t.record(outcome{At: "b", Repo: "/other", Result: "failed"})
	f.t.Once(context.Background())
	n, err := LastNight(f.t.StateDir, "/not/it", f.repoPath+"/")
	if err != nil || n == nil || n.Result != "unchanged" || n.CostUSD != 0.42 || n.Conversations != 1 {
		t.Fatalf("last night: %+v %v", n, err)
	}
}

// writeProposals is a stand-in Claude that proposes what it is given.
func writeProposals(body string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, dir, _ string) (string, error) {
		return "", os.WriteFile(filepath.Join(dir, "proposals.json"), []byte(body), 0o644)
	}
}

type filed struct{ repo, text, urgency string }

func nightFiling(t *testing.T, body string) (nightFixture, *[]filed, *bytes.Buffer) {
	f := newNight(t, writeProposals(body))
	var got []filed
	f.t.Todo = func(repo, text, urgency string) error { got = append(got, filed{repo, text, urgency}); return nil }
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return f, &got, &logs
}

func TestFourProposalsFileThreeAndSaySo(t *testing.T) {
	var items []string
	for i := 1; i <= 4; i++ {
		items = append(items, fmt.Sprintf(`{"text":"build keeps failing %d: add a line to CLAUDE.md","urgency":"high"}`, i))
	}
	f, got, logs := nightFiling(t, "["+strings.Join(items, ",")+"]")
	f.t.Once(context.Background())
	if len(*got) != 3 || (*got)[0].repo != f.repoPath || (*got)[0].urgency != "high" || !strings.Contains((*got)[2].text, "failing 3") {
		t.Fatalf("filed: %+v", *got)
	}
	if !strings.Contains(logs.String(), "4 proposals, the last 1 dropped") {
		t.Fatalf("the drop is logged:\n%s", logs)
	}
	if l := readLog(t, f.t.StateDir); len(l) != 1 || l[0].Todos != 3 || l[0].Result != "unchanged" {
		t.Fatalf("log: %+v", l)
	}
}

func TestASecretOrBadProposalIsDroppedAndTheRestFiled(t *testing.T) {
	f, got, logs := nightFiling(t, `[
		{"text":"the login keeps failing: key sk-ant-api03-abcdefghijklmnopqrstuvwxyz","urgency":"high"},
		{"text":"tests flake under load: add a retry note to CLAUDE.md","urgency":""},
		{"text":"","urgency":"low"},
		{"text":"x","urgency":"urgent"}]`)
	f.t.Once(context.Background())
	if len(*got) != 1 || !strings.Contains((*got)[0].text, "tests flake") || (*got)[0].urgency != "medium" {
		t.Fatalf("filed: %+v", *got)
	}
	if strings.Contains(logs.String(), "sk-ant-") || !strings.Contains(logs.String(), "an Anthropic key") {
		t.Fatalf("the drop is logged by kind, never the secret:\n%s", logs)
	}
}

func TestMissingOrMalformedProposalsFileNothingAndTheNightApplies(t *testing.T) {
	for name, claude := range map[string]func(context.Context, string, string) (string, error){
		"missing": func(_ context.Context, dir, _ string) (string, error) {
			return "", os.WriteFile(filepath.Join(dir, "memory", "old.md"), []byte("tidied"), 0o644)
		},
		"malformed": func(_ context.Context, dir, _ string) (string, error) {
			os.WriteFile(filepath.Join(dir, "proposals.json"), []byte(`{"text":"not an array"}`), 0o644)
			return "", os.WriteFile(filepath.Join(dir, "memory", "old.md"), []byte("tidied"), 0o644)
		},
	} {
		f := newNight(t, claude)
		f.t.Todo = func(string, string, string) error { t.Fatalf("%s: nothing is filed", name); return nil }
		f.t.Once(context.Background())
		if l := readLog(t, f.t.StateDir); len(l) != 1 || l[0].Result != "applied" || l[0].Todos != 0 {
			t.Fatalf("%s: log: %+v", name, l)
		}
	}
}

func TestAFailedNightFilesNothing(t *testing.T) {
	f := newNight(t, func(_ context.Context, dir, _ string) (string, error) {
		writeProposals(`[{"text":"a thing","urgency":"low"}]`)(context.Background(), dir, "")
		return "", errors.New("boom")
	})
	f.t.Todo = func(string, string, string) error { t.Fatal("a failed night files nothing"); return nil }
	f.t.Once(context.Background())
	if l := readLog(t, f.t.StateDir); len(l) != 1 || l[0].Result != "failed" || l[0].CostUSD != 0.42 {
		t.Fatalf("a failed night still says what it cost: %+v", l)
	}
}

func TestAnswerOfReadsClaudesJSON(t *testing.T) {
	a, err := answerOf([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"ok","total_cost_usd":0.5}`), "", nil)
	if err != nil || a.Text != "ok" || a.CostUSD != 0.5 {
		t.Fatalf("%+v %v", a, err)
	}
	a, err = answerOf([]byte(`{"subtype":"error_max_turns","is_error":true,"total_cost_usd":1.5}`), "", nil)
	if err == nil || !strings.Contains(err.Error(), "error_max_turns") || a.CostUSD != 1.5 {
		t.Fatalf("is_error is a failure with its cost: %+v %v", a, err)
	}
	a, err = answerOf([]byte("plain words"), "", nil)
	if err != nil || a.Text != "plain words" || a.CostUSD != 0 {
		t.Fatalf("another format is the answer, cost unknown: %+v %v", a, err)
	}
	_, err = answerOf([]byte(`{"result":"`+strings.Repeat("x", 1000)+`","is_error":true}`), "", errors.New("exit status 1"))
	if err == nil || len(err.Error()) > 400 {
		t.Fatalf("a failure's words are clipped: %d", len(err.Error()))
	}
}

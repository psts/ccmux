package hooktrace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tempTrace(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "trace.jsonl")
	old := DefaultPath()
	SetPath(p)
	t.Cleanup(func() { SetPath(old) })
	return p
}

func TestWrite_AppendsOneJSONObjectPerLine(t *testing.T) {
	p := tempTrace(t)

	Write(Line{Stage: StageRoute, Event: "stop", Decision: "attention", Attention: "done"})
	Write(Line{Stage: StagePush, Login: "dev@example.com", Decision: "sent"})

	lines := readLines(t, p)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	var first Line
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if first.Stage != StageRoute || first.Event != "stop" || first.Attention != "done" {
		t.Errorf("line 1 lost fields: %+v", first)
	}
	if first.TS == "" {
		t.Error("line 1 has no timestamp; correlation by time is the fallback when trace_id doesn't survive a hop")
	}
}

// A trace that can't be written must never become an error the daemon has to
// handle — the caller has no return value to check, so an unwritable path has to
// be a silent no-op rather than a panic.
func TestWrite_UnwritablePathIsSilent(t *testing.T) {
	old := DefaultPath()
	SetPath(filepath.Join(t.TempDir(), "no-such-dir", "trace.jsonl"))
	t.Cleanup(func() { SetPath(old) })

	Write(Line{Stage: StageRoute, Decision: "attention"}) // must not panic
}

// Four processes append to this file. Within one process the mutex serializes
// writes; this proves concurrent callers never interleave a partial line.
func TestWrite_ConcurrentWritesStayWhole(t *testing.T) {
	p := tempTrace(t)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Write(Line{Stage: StagePush, Decision: "sent", Detail: strings.Repeat("x", 200)})
		}()
	}
	wg.Wait()

	lines := readLines(t, p)
	if len(lines) != 50 {
		t.Fatalf("want 50 lines, got %d", len(lines))
	}
	for i, l := range lines {
		var parsed Line
		if err := json.Unmarshal([]byte(l), &parsed); err != nil {
			t.Fatalf("line %d torn: %v (%q)", i+1, err, l)
		}
	}
}

// Past the cap the file restarts rather than growing without bound. Truncation
// keeps the newest lines coming; the old ones are debugging scratch, not history.
func TestWrite_TruncatesPastCap(t *testing.T) {
	p := tempTrace(t)

	big := make([]byte, maxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatal(err)
	}

	Write(Line{Stage: StageRoute, Decision: "ignored"})

	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > maxBytes {
		t.Errorf("file still %d bytes; cap is %d", st.Size(), maxBytes)
	}
	if len(readLines(t, p)) != 1 {
		t.Error("want the fresh line to survive the truncation")
	}
}

// CCMUX_HOOK_TRACE is the one knob the shell script and the daemon share, so
// pointing both at a scratch file has to work from the environment alone.
func TestDefaultPath_HonoursEnvOverride(t *testing.T) {
	t.Setenv("CCMUX_HOOK_TRACE", "/tmp/somewhere-else.jsonl")
	if got := defaultPath(); got != "/tmp/somewhere-else.jsonl" {
		t.Errorf("defaultPath() = %q, want the env override", got)
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

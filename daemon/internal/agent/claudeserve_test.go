package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/harness"
)

func TestHasChat(t *testing.T) {
	for name, want := range map[string]bool{"opencode": true, harness.Builtin: true, "pi": false, "codex": false, "": false} {
		if HasChat(name) != want {
			t.Errorf("HasChat(%q) = %v", name, !want)
		}
	}
}

func TestOfflineReadsRefuseAHarnessWithNoChat(t *testing.T) {
	ctx := context.Background()
	if _, err := OfflineSessions(ctx, "pi", t.TempDir(), "/x"); err == nil || !strings.Contains(err.Error(), "no chat history") {
		t.Errorf("sessions: %v", err)
	}
	if _, err := OfflineTranscript(ctx, "codex", t.TempDir(), "/x", "s"); err == nil || !strings.Contains(err.Error(), "no chat history") {
		t.Errorf("transcript: %v", err)
	}
	// Claude with no sidecar written yet says so rather than failing on a
	// missing file deep inside node.
	if _, err := OfflineSessions(ctx, harness.Builtin, t.TempDir(), "/x"); err == nil || !strings.Contains(err.Error(), "not installed yet") {
		t.Errorf("claude before first start: %v", err)
	}
}

// installClaudeServeDeps compares the pin with the RESOLVED version in
// node_modules, before (skip) and after (verify) npm ci. That only ever
// matches an exact version: a range like ^0.3.273 would install on every
// start and then fail the verify, and no claude agent could start. And npm
// ci refuses a lock that disagrees with package.json, so a pin bump that
// forgets to regenerate the lock would break every claude start: the two
// embedded files must name the same version.
func TestClaudeSDKPinIsAnExactVersionAndTheLockAgrees(t *testing.T) {
	v, err := pinnedSDKVersion()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(v) {
		t.Fatalf("claudeserve/package.json pins %q; it must be an exact x.y.z", v)
	}
	if got := installedSDKVersion(t.TempDir()); got != "" {
		t.Fatalf("no node_modules reads as %q, want empty", got)
	}
	body, err := claudeServeFiles.ReadFile("claudeserve/package-lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Packages map[string]struct {
			Version      string            `json:"version"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(body, &lock); err != nil {
		t.Fatal(err)
	}
	if got := lock.Packages[""].Dependencies[sdkPackage]; got != v {
		t.Errorf("the lock's root wants %q, package.json pins %q: regenerate package-lock.json", got, v)
	}
	if got := lock.Packages["node_modules/"+sdkPackage].Version; got != v {
		t.Errorf("the lock resolves %q, package.json pins %q: regenerate package-lock.json", got, v)
	}
}

// Every embedded file that is not a test ships, and everything that ships
// is embedded: the two lists are hand-kept, and a module embedded but left
// off the shipped list fails at agent start (its import is missing) with
// every Go test green.
func TestClaudeServeEmbedAndShippedListsAgree(t *testing.T) {
	entries, err := claudeServeFiles.ReadDir("claudeserve")
	if err != nil {
		t.Fatal(err)
	}
	var embedded []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".test.mjs") {
			embedded = append(embedded, e.Name())
		}
	}
	slices.Sort(embedded)
	shipped := slices.Clone(claudeServeShipped)
	slices.Sort(shipped)
	if !slices.Equal(embedded, shipped) {
		t.Errorf("embedded %v, shipped %v", embedded, shipped)
	}
	if !slices.Contains(claudeServeShipped, "package-lock.json") {
		t.Error("the lock ships with the sidecar")
	}
}

// The offline reads run the sidecar's sessions/export verbs; a stub script
// stands in for it. Sessions are filtered to the pane's folder (a session
// elsewhere is not this agent's, however new) and ordered newest first.
func TestClaudeOfflineReadsThroughTheSidecar(t *testing.T) {
	if _, err := harness.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	root, dir := t.TempDir(), t.TempDir()
	script := ClaudeServeScript(root)
	os.MkdirAll(filepath.Dir(script), 0o755)
	stub := `const a = process.argv.slice(2);
if (a[0] === "sessions") console.log(JSON.stringify([
  {id: "old", title: "older here", directory: a[2], time: {updated: 5}},
  {id: "elsewhere", title: "newest but not here", directory: "/somewhere/else", time: {updated: 99}},
  {id: "new", title: "newest here", directory: a[2], time: {updated: 9}}]));
else if (a[0] === "export") console.log(JSON.stringify({messages: [
  {info: {id: "u1", role: "user", time: {created: 1}}, parts: [{id: "u1.0", messageID: "u1", type: "text", text: "asked " + a[4]}]},
  {info: {id: "a1", role: "assistant", time: {created: 2}}, parts: [{id: "a1.0", messageID: "a1", type: "tool", tool: "Bash", state: {status: "completed", input: {command: "ls"}, output: "a.go", title: "ls"}}]}]}));
else { console.error("bad verb " + a[0]); process.exit(2); }
`
	if err := os.WriteFile(script, []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sessions, err := ClaudeOfflineSessions(ctx, root, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "new" || sessions[0].Updated != 9 || sessions[1].ID != "old" {
		t.Fatalf("sessions in %s: %+v", dir, sessions)
	}
	turns, err := ClaudeOfflineTranscript(ctx, root, dir, "new")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Parts[0].Text != "asked new" || turns[1].Parts[0].Tool != "Bash" || turns[1].Parts[0].Output != "a.go" {
		t.Fatalf("transcript: %+v", turns)
	}
	// A verb the stub refuses surfaces its stderr, not a silent empty answer.
	os.WriteFile(script, []byte(`console.error("store locked"); process.exit(3);`), 0o644)
	if _, err := ClaudeOfflineSessions(ctx, root, dir); err == nil || !strings.Contains(err.Error(), "store locked") {
		t.Fatalf("stderr must reach the caller: %v", err)
	}
}

// EnsureClaudeServe writes the embedded files and skips the install when
// the pinned SDK is already there; the marker stands in for node_modules.
func TestEnsureClaudeServeWritesFilesAndSkipsAnInstalledSDK(t *testing.T) {
	if _, err := harness.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	want, err := pinnedSDKVersion()
	if err != nil {
		t.Fatal(err)
	}
	st := NewStore(t.TempDir())
	marker := filepath.Join(st.Root, ClaudeServeDir, "node_modules", sdkPackage, "package.json")
	os.MkdirAll(filepath.Dir(marker), 0o755)
	os.WriteFile(marker, []byte(`{"version":"`+want+`"}`), 0o644)
	script, node, err := st.EnsureClaudeServe()
	if err != nil {
		t.Fatal(err)
	}
	if script != ClaudeServeScript(st.Root) || node == "" {
		t.Errorf("script %q node %q", script, node)
	}
	for _, name := range claudeServeShipped {
		if _, err := os.Stat(filepath.Join(st.Root, ClaudeServeDir, name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}
	if _, _, err := st.EnsureClaudeServe(); err != nil {
		t.Errorf("second call: %v", err)
	}
	// An upgraded daemon must land ITS sidecar over whatever an older one
	// wrote, or the chat drifts from the Go client on exactly the hosts that
	// already ran an agent. A file nobody ships (the install's own notes)
	// is left alone.
	notes := filepath.Join(st.Root, ClaudeServeDir, "node_modules", ".package-lock.json")
	os.WriteFile(notes, []byte("{}"), 0o644)
	for _, name := range claudeServeShipped {
		os.WriteFile(filepath.Join(st.Root, ClaudeServeDir, name), []byte("// stale sidecar from an older daemon\n"), 0o644)
	}
	if _, _, err := st.EnsureClaudeServe(); err != nil {
		t.Fatalf("over stale files: %v", err)
	}
	for _, name := range claudeServeShipped {
		want, _ := claudeServeFiles.ReadFile("claudeserve/" + name)
		got, _ := os.ReadFile(filepath.Join(st.Root, ClaudeServeDir, name))
		if string(got) != string(want) {
			t.Errorf("%s was not replaced by the embedded copy", name)
		}
	}
	if got, _ := os.ReadFile(notes); string(got) != "{}" {
		t.Error("the install's own files under node_modules must not be touched")
	}
	for _, name := range []string{"translate.test.mjs", "routes.test.mjs", "agent.test.mjs", "args.test.mjs"} {
		if _, err := os.Stat(filepath.Join(st.Root, ClaudeServeDir, name)); err == nil {
			t.Errorf("%s is a test, not shipped", name)
		}
	}
}

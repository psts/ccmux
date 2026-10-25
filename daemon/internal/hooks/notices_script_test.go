package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// runPromptHook runs the script for UserPromptSubmit in a hosted pane whose
// daemon answers the notices route with body, and returns its stdout.
func runPromptHook(t *testing.T, body string) (stdout string, gotAuth string) {
	t.Helper()
	requireScript(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/panes/pane-1/notices" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	cmd := exec.Command("bash", notifyScript, "user-prompt-submit")
	cmd.Stdin = strings.NewReader(`{"session_id":"s1","cwd":"/repo","hook_event_name":"UserPromptSubmit"}`)
	cmd.Env = append(scrubbedEnv(),
		"CCMUX_HOOK_TRACE="+t.TempDir()+"/trace.jsonl",
		"CCMUX_PANE_ID=pane-1", "CCMUX_PANE_TOKEN=tok", "CCMUX_DAEMON_URL="+srv.URL,
		"CCMUX_HOOKS_SOCK=/tmp/ccmux-no-such-socket.sock")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script failed: %v", err)
	}
	return string(out), gotAuth
}

// Waiting quiet notices come back as the turn's additionalContext, which is
// the only stdout Claude Code reads from a UserPromptSubmit hook.
func TestNotifyScript_PromptPrintsQuietNotices(t *testing.T) {
	out, auth := runPromptHook(t, `{"text":"[claude-peers quiet notices]\n\nfrom radar: it's new"}`)
	if auth != "Bearer tok" {
		t.Errorf("notices asked with Authorization %q, want the pane token", auth)
	}
	var got struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not the hook JSON: %v\n%q", err, out)
	}
	if got.HookSpecificOutput.HookEventName != "UserPromptSubmit" ||
		!strings.Contains(got.HookSpecificOutput.AdditionalContext, "from radar: it's new") {
		t.Errorf("hook output = %+v", got)
	}
}

// No notices, or a daemon without the route, prints nothing: the turn goes
// ahead untouched.
func TestNotifyScript_PromptWithoutNoticesPrintsNothing(t *testing.T) {
	for _, body := range []string{`{"text":""}`, `{"error":"not found"}`, `not json`} {
		if out, _ := runPromptHook(t, body); out != "" {
			t.Errorf("body %q: stdout = %q, want nothing", body, out)
		}
	}
}

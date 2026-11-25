package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/walls"
)

// liveTemplate is the session template as the live daemon wrote it before
// the owner mark (read off ~/.config/systemd/user/ccmux-session@.service on
// 2026-11-26).
const liveTemplate = `[Unit]
Description=ccmux session %i
StartLimitIntervalSec=600
StartLimitBurst=5

[Service]
Type=simple
ExecStart=/ext/sanlabs-home/.local/bin/ccmuxd runner-exec /home/sanlabs/.local/state/ccmux/sessions/%i.json
Restart=on-failure
RestartSec=2
RestartPreventExitStatus=97
KillMode=control-group
TimeoutStopSec=20
MemoryMax=75%
CPUQuota=3100%
TasksMax=8192
NoNewPrivileges=yes
StandardInput=null
`

func newUnits() *fakeUnits {
	return &fakeUnits{active: map[string]bool{}, result: map[string]string{}}
}

func ensure(t *testing.T, dir, state string, u *fakeUnits) error {
	t.Helper()
	paths := Paths{State: state, Prefix: "ccmux-session-test"}
	return EnsureTemplate(dir, "/bin/ccmuxd", paths, walls.Defaults().Caps(), u)
}

func plant(t *testing.T, dir, text string) string {
	t.Helper()
	path := filepath.Join(dir, "ccmux-session-test@.service")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheTemplateCarriesItsOwnersMark(t *testing.T) {
	dir, u := t.TempDir(), newUnits()
	if err := ensure(t, dir, "/st/sessions", u); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ccmux-session-test@.service"))
	if !strings.HasPrefix(string(b), "# ccmux-session-state=/st/sessions\n[Unit]\n") {
		t.Fatalf("the mark is the first line:\n%s", b)
	}
	if err := ensure(t, dir, "/st/sessions", u); err != nil || u.reloaded != 1 {
		t.Fatalf("the same daemon again writes nothing: %v, %d reloads", err, u.reloaded)
	}
}

func TestATemplateMarkedForAnotherFolderIsLeftAlone(t *testing.T) {
	dir, u := t.TempDir(), newUnits()
	theirs := templateText("/other/ccmuxd", "/other/sessions", walls.Defaults().Caps())
	path := plant(t, dir, theirs)
	err := ensure(t, dir, "/st/sessions", u)
	if err == nil || !strings.Contains(err.Error(), "/other/sessions") || !strings.Contains(err.Error(), "-runner-unit-prefix") {
		t.Fatalf("refused, naming whose it is and the fix: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != theirs || u.reloaded != 0 {
		t.Fatalf("file untouched and no reload (%d)", u.reloaded)
	}
}

func TestTheMarklessLiveTemplateIsTakenOverOnlyByItsOwnDaemon(t *testing.T) {
	dir, u := t.TempDir(), newUnits()
	path := plant(t, dir, liveTemplate)
	err := ensure(t, dir, "/run/user/1002/t201/state/sessions", u)
	if err == nil || !strings.Contains(err.Error(), "/home/sanlabs/.local/state/ccmux/sessions") {
		t.Fatalf("a daemon of another folder is refused: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != liveTemplate || u.reloaded != 0 {
		t.Fatal("and the live template is byte for byte what it was")
	}
	if err := ensure(t, dir, "/home/sanlabs/.local/state/ccmux/sessions", u); err != nil {
		t.Fatalf("the daemon whose folder it names takes it over: %v", err)
	}
	if b, _ := os.ReadFile(path); !bytes.HasPrefix(b, []byte(ownerMark+"/home/sanlabs/.local/state/ccmux/sessions\n")) || u.reloaded != 1 {
		t.Fatalf("now marked, and reloaded once:\n%s", b)
	}
}

func TestATemplateThatNamesNoSpecIsNotTakenOver(t *testing.T) {
	dir, u := t.TempDir(), newUnits()
	plant(t, dir, "[Service]\nExecStart=/bin/true\n")
	if err := ensure(t, dir, "/st/sessions", u); err == nil || !strings.Contains(err.Error(), "(none named)") {
		t.Fatalf("a file of nobody's that is not ours is refused: %v", err)
	}
}

func TestAMarkedTemplateOfThisDaemonIsRewrittenWhenItDiffers(t *testing.T) {
	dir, u := t.TempDir(), newUnits()
	path := plant(t, dir, templateText("/old/ccmuxd", "/st/sessions", walls.Defaults().Caps()))
	if err := ensure(t, dir, "/st/sessions", u); err != nil || u.reloaded != 1 {
		t.Fatalf("a changed binary or cap rewrites its own: %v, %d", err, u.reloaded)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "ExecStart=/bin/ccmuxd runner-exec") {
		t.Fatalf("rewritten:\n%s", b)
	}
}

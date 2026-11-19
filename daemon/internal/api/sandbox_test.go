package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ccmux.dev/ccmuxd/internal/manager"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/tmux"
	"ccmux.dev/ccmuxd/internal/walls"
)

// fakeSandbox is the daemon's side of the walls: a host it answers with,
// and what it was asked.
type fakeSandbox struct {
	host    walls.Host
	fresh   []bool
	applied []walls.Settings
	err     error
}

func (f *fakeSandbox) Host(fresh bool) walls.Host {
	f.fresh = append(f.fresh, fresh)
	return f.host
}

func (f *fakeSandbox) Apply(s walls.Settings) error {
	f.applied = append(f.applied, s)
	return f.err
}

func sandboxCall(t *testing.T, s *Server, method, path, body string) (int, sandboxView, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
	var view sandboxView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	return rec.Code, view, rec.Body.String()
}

func TestSandbox_ReadsTheDefaultsAndTheHost(t *testing.T) {
	s := settingsServer(t)
	sb := &fakeSandbox{host: walls.Host{Caps: true, Walls: false, WallsWhy: "systemd built no walls", Fix: []string{"sudo sysctl x=0"}}}
	s.SetSandbox(sb)
	code, view, body := sandboxCall(t, s, "GET", "/v1/sandbox", "")
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, body)
	}
	if view.Settings.On || view.Settings.MemoryPercent != 75 || view.Settings.JobHours != 6 || view.Settings.ExtraWrite == nil {
		t.Fatalf("a daemon that never saved any has the defaults, off: %+v", view.Settings)
	}
	if view.Host.Walls || view.Host.WallsWhy != "systemd built no walls" || len(view.Host.Fix) != 1 {
		t.Fatalf("the host as the daemon found it: %+v", view.Host)
	}
	if len(sb.fresh) != 1 || sb.fresh[0] {
		t.Fatalf("a read does not start a trial unit each time: %v", sb.fresh)
	}
}

func TestSandbox_SavesAppliesTheCapsAndAsksTheHostAfreshWhenSwitchedOn(t *testing.T) {
	s := settingsServer(t)
	sb := &fakeSandbox{host: walls.Host{Caps: true, Walls: true}}
	s.SetSandbox(sb)
	code, view, body := sandboxCall(t, s, "PUT", "/v1/sandbox", `{"on":true,"memoryPercent":60,"cpuCores":1,"tasksMax":4096,"jobHours":3,"extraWrite":["/opt/cache"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: %d %s", code, body)
	}
	if !view.Settings.On || view.Settings.MemoryPercent != 60 || len(view.Settings.ExtraWrite) != 1 {
		t.Fatalf("the answer is what was saved: %+v", view.Settings)
	}
	if len(sb.applied) != 1 || sb.applied[0].MemoryPercent != 60 {
		t.Fatalf("the units' caps follow the save: %+v", sb.applied)
	}
	if len(sb.fresh) != 1 || !sb.fresh[0] {
		t.Fatalf("switching the walls on asks the host now, so the page says at once whether it can: %v", sb.fresh)
	}
	if got, _ := s.mgr.Sandbox(); !got.On || got.CPUCores != 1 {
		t.Fatalf("stored: %+v", got)
	}
	_, view, _ = sandboxCall(t, s, "GET", "/v1/sandbox", "")
	if !view.Settings.On || view.Settings.TasksMax != 4096 {
		t.Fatalf("and read back: %+v", view.Settings)
	}
}

func TestSandbox_RefusesWhatAUnitFileCannotHold(t *testing.T) {
	s := settingsServer(t)
	sb := &fakeSandbox{}
	s.SetSandbox(sb)
	for name, body := range map[string]string{
		"no memory":       `{"memoryPercent":0,"cpuCores":1,"tasksMax":4096,"jobHours":3}`,
		"a unit line":     `{"memoryPercent":60,"cpuCores":1,"tasksMax":4096,"jobHours":3,"extraWrite":["/a\nProtectHome=no"]}`,
		"a relative path": `{"memoryPercent":60,"cpuCores":1,"tasksMax":4096,"jobHours":3,"extraWrite":["cache"]}`,
	} {
		code, _, text := sandboxCall(t, s, "PUT", "/v1/sandbox", body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, text)
		}
	}
	if len(sb.applied) != 0 {
		t.Fatalf("nothing refused reaches a unit file: %+v", sb.applied)
	}
	if got, _ := s.mgr.Sandbox(); got.MemoryPercent != 75 {
		t.Fatalf("and nothing was stored: %+v", got)
	}
}

func TestSandbox_CheckAsksTheHostAgain(t *testing.T) {
	s := settingsServer(t)
	sb := &fakeSandbox{host: walls.Host{Walls: true}}
	s.SetSandbox(sb)
	code, view, _ := sandboxCall(t, s, "POST", "/v1/sandbox/check", "")
	if code != http.StatusOK || !view.Host.Walls || len(sb.fresh) != 1 || !sb.fresh[0] {
		t.Fatalf("check: %d %+v %v", code, view.Host, sb.fresh)
	}
}

func TestSandbox_ADaemonThatRunsNoUnitsSaysSo(t *testing.T) {
	s := settingsServer(t)
	for _, call := range [][2]string{{"GET", "/v1/sandbox"}, {"PUT", "/v1/sandbox"}, {"POST", "/v1/sandbox/check"}} {
		code, _, body := sandboxCall(t, s, call[0], call[1], `{}`)
		if code != http.StatusServiceUnavailable || !strings.Contains(body, "nothing to cap or wall") {
			t.Errorf("%s %s: %d %s", call[0], call[1], code, body)
		}
	}
}

func TestSandbox_UnreadableSettingsAreAnErrorNeverTheDefaults(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// A row another build wrote, which this one cannot read.
	if err := st.SetSetting("sandbox", "{not json"); err != nil {
		t.Fatal(err)
	}
	s := &Server{mgr: manager.New(context.Background(), &tmux.Server{Socket: "unused"}, st)}
	s.SetSandbox(&fakeSandbox{})
	if _, err := s.mgr.Sandbox(); err == nil {
		t.Fatal("a garbled row must not read as the defaults")
	}
	code, _, body := sandboxCall(t, s, "GET", "/v1/sandbox", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("a page that showed \"off\" and saved would switch the walls off: %d %s", code, body)
	}
}

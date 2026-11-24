package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeInstall makes dir look like an installed sidecar of version.
func fakeInstall(t *testing.T, dir, version string) {
	t.Helper()
	pkg := filepath.Join(dir, "node_modules", sdkPackage)
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.3.330", "0.3.311", true},
		{"0.3.311", "0.3.330", false},
		{"0.10.0", "0.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"0.3.330", "", true},
		{"0.4.0-beta.1", "0.3.0", false}, // not a plain x.y.z: never newer
		{"", "0.3.0", false},
	} {
		if got := NewerVersion(c.a, c.b); got != c.want {
			t.Errorf("NewerVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// Sessions start on the promoted version only when it is newer than the
// pin and really installed; its folder gets this daemon's scripts.
func TestTheChannelSidecarWinsOnlyWhenNewerAndInstalled(t *testing.T) {
	s := testStore(t)
	pinned, _ := pinnedSDKVersion()
	if _, ok, _ := s.channelSidecar(); ok {
		t.Fatal("no version promoted: the pin serves")
	}
	if got := s.SidecarScriptNow(); got != ClaudeServeScript(s.Root) {
		t.Fatalf("SidecarScriptNow with the pin serving = %q", got)
	}
	if err := s.PromoteSDK("99.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.channelSidecar(); ok {
		t.Fatal("promoted but not installed: the pin serves")
	}
	dir := filepath.Join(s.Root, SDKChannelDir, "99.0.0")
	fakeInstall(t, dir, "99.0.0")
	script, ok, err := s.channelSidecar()
	if err != nil || !ok || script != filepath.Join(dir, "serve.mjs") {
		t.Fatalf("got %q %v %v", script, ok, err)
	}
	if got := s.SidecarScriptNow(); got != script {
		t.Fatalf("SidecarScriptNow = %q, a start now runs %q", got, script)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.mjs")); err != nil {
		t.Fatal("the scripts are written into the version's folder")
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); !os.IsNotExist(err) {
		t.Fatal("the daemon's manifest, which names the pin, is not written there")
	}
	old := filepath.Join(s.Root, SDKChannelDir, "0.0.1")
	fakeInstall(t, old, "0.0.1")
	if err := s.PromoteSDK("0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.channelSidecar(); ok {
		t.Fatalf("a promoted version older than the pin (%s) does not win", pinned)
	}
}

func TestPruneKeepsCurrentNewestAndInUse(t *testing.T) {
	s := testStore(t)
	root := filepath.Join(s.Root, SDKChannelDir)
	for _, v := range []string{"0.3.320", "0.3.321", "0.3.322", "0.3.323", "0.3.324"} {
		fakeInstall(t, filepath.Join(root, v), v)
	}
	if err := os.MkdirAll(filepath.Join(root, ".install-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteSDK("0.3.320"); err != nil {
		t.Fatal(err)
	}
	inUse := []string{filepath.Join(root, "0.3.321", "serve.mjs"), "/elsewhere/serve.mjs"}
	if err := s.PruneSDKs(2, inUse); err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"0.3.320": true, "0.3.321": true, "0.3.322": false, "0.3.323": true, "0.3.324": true, ".install-x": true} {
		_, err := os.Stat(filepath.Join(root, v))
		if (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", v, err == nil, want)
		}
	}
}

func TestAVersionMustBeOneToNameAFolder(t *testing.T) {
	s := testStore(t)
	for _, v := range []string{"../x", ".hidden", "a/b", ""} {
		if err := s.RemoveSDK(v); err == nil {
			t.Errorf("RemoveSDK(%q) taken", v)
		}
	}
	if _, err := s.InstallSDK(context.Background(), "../../x"); err == nil {
		t.Fatal("InstallSDK took a path")
	}
}

func TestTheUpdateRecordRoundTrips(t *testing.T) {
	s := testStore(t)
	if (s.LastSDKUpdate() != SDKUpdate{}) || s.SDKCurrent() != "" {
		t.Fatal("nothing recorded yet")
	}
	want := SDKUpdate{At: 5, Version: "0.3.330", Kind: "test", Error: "no answer"}
	if err := s.RecordSDKUpdate(want); err != nil {
		t.Fatal(err)
	}
	if got := s.LastSDKUpdate(); got != want {
		t.Fatalf("got %+v", got)
	}
	if v := s.SDKVersions(); v.Update != want {
		t.Fatalf("the versions carry it: %+v", v)
	}
}

// A half install a restart cut off is removed; versions are not.
func TestCleanSDKInstalls(t *testing.T) {
	s := testStore(t)
	root := filepath.Join(s.Root, SDKChannelDir)
	fakeInstall(t, filepath.Join(root, "0.3.330"), "0.3.330")
	if err := os.MkdirAll(filepath.Join(root, ".install-123"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.CleanSDKInstalls()
	if _, err := os.Stat(filepath.Join(root, ".install-123")); !os.IsNotExist(err) {
		t.Fatal("the half install stayed")
	}
	if _, err := os.Stat(filepath.Join(root, "0.3.330")); err != nil {
		t.Fatal("a version went with it")
	}
}

// Settings names the promoted version only while sessions start on it:
// a newer pin from a daemon release overtakes it.
func TestSDKVersionsNamesOnlyALivePromotion(t *testing.T) {
	s := testStore(t)
	if err := s.PromoteSDK("0.0.1"); err != nil {
		t.Fatal(err)
	}
	if v := s.SDKVersions(); v.Current != "" {
		t.Fatalf("an overtaken promotion is reported: %+v", v)
	}
	if err := s.PromoteSDK("99.0.0"); err != nil {
		t.Fatal(err)
	}
	if v := s.SDKVersions(); v.Current != "99.0.0" {
		t.Fatalf("a newer promotion is not reported: %+v", v)
	}
}

package meridian

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ccmux.dev/ccmuxd/internal/llmproxy"
)

func TestSpecsFromPicksMeridianAccountsOnly(t *testing.T) {
	specs := SpecsFrom([]llmproxy.Account{
		{Name: "anth", Kind: "anthropic", APIKey: "k"},
		{Name: "m1", Kind: llmproxy.KindMeridian, BaseURL: "http://127.0.0.1:3456", APIKey: "tok"},
	})
	if len(specs) != 1 || specs[0].Name != "m1" || specs[0].Token != "tok" || specs[0].BaseURL != "http://127.0.0.1:3456" {
		t.Fatalf("got %+v", specs)
	}
}

func TestListenParsesAndRefusesNonLoopback(t *testing.T) {
	host, port, err := Listen("")
	if err != nil || host != "127.0.0.1" || port != 3456 {
		t.Fatalf("default: %q %d %v", host, port, err)
	}
	host, port, err = Listen("http://localhost:4000")
	if err != nil || host != "localhost" || port != 4000 {
		t.Fatalf("localhost: %q %d %v", host, port, err)
	}
	for _, bad := range []string{"http://10.0.0.5:3456", "https://127.0.0.1:3456", "127.0.0.1:3456", "http://127.0.0.1:x", "http://localhost"} {
		if _, _, err := Listen(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCommandEnv(t *testing.T) {
	cmd, err := Command(context.Background(), "/bin/true", Spec{Name: "m", BaseURL: "http://127.0.0.1:3999", Token: "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-x", "MERIDIAN_PORT=3999", "MERIDIAN_HOST=127.0.0.1", "MERIDIAN_PASSTHROUGH=1"} {
		if !strings.Contains(env, want) {
			t.Errorf("env missing %s", want)
		}
	}
	if len(cmd.Args) != 1 {
		t.Errorf("token must ride in env, not argv: %v", cmd.Args)
	}
}

func TestChildPathPutsNodeBinFirst(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "node-v24", "lib", "node_modules", "@rynfar", "meridian", "dist", "cli.js")
	os.MkdirAll(filepath.Dir(real), 0o755)
	os.WriteFile(real, []byte("x"), 0o755)
	bin := filepath.Join(root, "bin", "meridian")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.Symlink(real, bin); err != nil {
		t.Skip("symlinks unavailable")
	}
	got := childPath(bin)
	want := filepath.Join(root, "node-v24", "bin")
	if !strings.HasPrefix(got, want+string(os.PathListSeparator)) {
		t.Fatalf("PATH should start with the node bin dir %s, got %s", want, got)
	}
	if !strings.HasSuffix(got, os.Getenv("PATH")) {
		t.Fatal("inherited PATH should come last")
	}
}

func TestPluginPathResolvesThroughSymlink(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "lib", "node_modules", "@rynfar", "meridian")
	for _, d := range []string{"dist", "plugin"} {
		if err := os.MkdirAll(filepath.Join(pkg, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(pkg, "dist", "cli.js")
	os.WriteFile(cli, []byte("#!/usr/bin/env node\n"), 0o755)
	os.WriteFile(filepath.Join(pkg, "plugin", "meridian.ts"), []byte("export default 1\n"), 0o644)
	bin := filepath.Join(root, "bin", "meridian")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	if err := os.Symlink(cli, bin); err != nil {
		t.Skip("symlinks unavailable")
	}
	look := func(string) (string, error) { return bin, nil }
	if got := PluginPath(look); got != filepath.Join(pkg, "plugin", "meridian.ts") {
		t.Fatalf("got %q", got)
	}
	os.Remove(filepath.Join(pkg, "plugin", "meridian.ts"))
	if got := PluginPath(look); got != "" {
		t.Fatalf("missing plugin file should yield empty, got %q", got)
	}
	if got := PluginPath(func(string) (string, error) { return "", os.ErrNotExist }); got != "" {
		t.Fatalf("uninstalled should yield empty, got %q", got)
	}
}

// fakeBin is a script that sleeps until killed, standing in for meridian.
func fakeBin(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "meridian")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, cond func() bool) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestReconcileStartsStopsAndRestartsOnChange(t *testing.T) {
	bin := fakeBin(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sv := New(ctx, func(string) (string, error) { return bin, nil })
	a := Spec{Name: "a", BaseURL: "http://127.0.0.1:3901", Token: "t1"}
	sv.Reconcile([]Spec{a})
	waitFor(t, func() bool { return sv.Status()["a"].Running })
	pid1 := sv.Status()["a"].PID

	// Same spec again: no restart.
	sv.Reconcile([]Spec{a})
	time.Sleep(50 * time.Millisecond)
	if sv.Status()["a"].PID != pid1 {
		t.Fatal("unchanged spec restarted the process")
	}

	// Rotated token: new process, and the old one is gone BEFORE Reconcile
	// returns (a live sidecar would otherwise lose the port race).
	a.Token = "t2"
	sv.Reconcile([]Spec{a})
	if err := syscall.Kill(pid1, 0); err == nil {
		t.Fatal("old process still alive after Reconcile returned")
	}
	waitFor(t, func() bool { st := sv.Status()["a"]; return st.Running && st.PID != pid1 })

	// Removed: gone from status.
	sv.Reconcile(nil)
	waitFor(t, func() bool { _, ok := sv.Status()["a"]; return !ok })
}

func TestMissingBinaryKeepsRetryingWithoutRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sv := New(ctx, func(string) (string, error) { return "", os.ErrNotExist })
	sv.Reconcile([]Spec{{Name: "a", Token: "t"}})
	waitFor(t, func() bool { st := sv.Status()["a"]; return !st.Running && st.LastError != "" })
}

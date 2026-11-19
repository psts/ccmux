package walls

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheTrialUnitIsARealWalledUnitThatChecksItself(t *testing.T) {
	var got []string
	p := &Prober{Self: "/bin/ccmuxd", Home: "/home/u", Run: func(_ context.Context, name string, args ...string) (string, error) {
		if name == "systemd-run" {
			got = args
		}
		return "", nil
	}}
	ok, why := p.trial(context.Background())
	if !ok || why != "" {
		t.Fatalf("trial: %v %q", ok, why)
	}
	line := strings.Join(got, " ")
	for _, want := range []string{"--user", "--wait", "ProtectSystem=strict", "ProtectHome=read-only", "/bin/ccmuxd " + ProbeVerb + " mnt:[", "/home/u"} {
		if !strings.Contains(line, want) {
			t.Errorf("the trial misses %q: %s", want, line)
		}
	}
}

func TestAHostThatDoesNotWallSaysWhyAndIsAskedAgainLater(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	walls, asked := false, 0
	p := &Prober{Self: "/bin/ccmuxd", Home: "/home/u", Now: func() time.Time { return now },
		Run: func(_ context.Context, name string, _ ...string) (string, error) {
			if name != "systemd-run" {
				return "", nil
			}
			asked++
			if walls {
				return "", nil
			}
			return "noise\n" + ErrNoWalls.Error() + ": it runs in the daemon's own mount namespace", errors.New("exit status 97")
		}}
	err := p.Ready()
	if err == nil || !strings.Contains(err.Error(), "daemon's own mount namespace") || !strings.Contains(err.Error(), "Settings > Sandbox") {
		t.Fatalf("not ready must say why and what to do: %v", err)
	}
	if p.Ready() == nil || asked != 1 {
		t.Fatalf("a fresh no is believed, not asked again at every start: asked %d", asked)
	}
	walls = true // the person ran the fix
	now = now.Add(retryAfter + time.Second)
	if err := p.Ready(); err != nil || asked != 2 {
		t.Fatalf("an old no is asked again: %v, asked %d", err, asked)
	}
	now = now.Add(time.Hour)
	if err := p.Ready(); err != nil || asked != 2 {
		t.Fatalf("a yes is kept: %v, asked %d", err, asked)
	}
}

func TestTheFixIsTheCommandForWhatIsMissing(t *testing.T) {
	if got := fixes(Host{Walls: true, Bwrap: true, Socat: true}, true); len(got) != 0 {
		t.Errorf("a host that has it all needs no fix: %v", got)
	}
	got := fixes(Host{Walls: false, Bwrap: false, BwrapWhy: "setting up uid map: Permission denied", Socat: false}, true)
	if len(got) != 2 || !strings.Contains(got[0], "apparmor_restrict_unprivileged_userns=0") || !strings.Contains(got[1], "apt install bubblewrap socat") {
		t.Errorf("fixes: %v", got)
	}
	if got := fixes(Host{Walls: false, Bwrap: true, Socat: true}, false); len(got) != 0 {
		t.Errorf("walls that fail for another reason get no command that would not help: %v", got)
	}
}

func TestCapsNeedTheControllersDelegated(t *testing.T) {
	own := "0::/user.slice/user-1002.slice/user@1002.service/app.slice/ccmuxd.service\n"
	read := func(have string) func(string) ([]byte, error) {
		return func(path string) ([]byte, error) {
			if path != filepath.Join("/sys/fs/cgroup/user.slice/user-1002.slice/user@1002.service", "cgroup.controllers") {
				return nil, os.ErrNotExist
			}
			return []byte(have), nil
		}
	}
	if ok, why := delegated(own, read("cpu memory pids\n")); !ok {
		t.Errorf("all three: %s", why)
	}
	if ok, why := delegated(own, read("memory pids")); ok || !strings.Contains(why, "cpu") {
		t.Errorf("a missing controller is named: %v %s", ok, why)
	}
	if ok, _ := delegated("0::/system.slice/x.service\n", read("cpu memory pids")); ok {
		t.Error("outside a user manager nothing is known")
	}
}

func TestTheProbeVerbRefusesAnUnwalledProcess(t *testing.T) {
	ns, err := MountNS()
	if err != nil {
		t.Skipf("no /proc here: %v", err)
	}
	if got := RunProbe([]string{ns, t.TempDir()}); got != ExitRefused {
		t.Fatalf("this test is not walled: exit %d", got)
	}
	if got := RunProbe(nil); got != 2 {
		t.Fatalf("usage: exit %d", got)
	}
}

package walls

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestADropInWallsTheUnitAndOpensOnlyWhatThePlanNames(t *testing.T) {
	p := Plan{
		Write:   []string{"/wt/task-1", "/repo/.git/objects", "/wt/task-1"},
		Caches:  []string{"/home/u/.cache"},
		Sealed:  []string{"/repo/.git/hooks"},
		Outside: []string{"/home/u"},
	}
	text := p.DropIn()
	for _, want := range []string{
		"[Service]\n", "ProtectSystem=strict\n", "ProtectHome=read-only\n", "PrivateTmp=yes\n",
		"RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK\n",
		"ReadWritePaths=/repo/.git/objects /wt/task-1 -/home/u/.cache\n",
		"ReadOnlyPaths=/repo/.git/hooks\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the drop-in misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "PrivateNetwork") {
		t.Errorf("a plan that keeps its network must not lose it:\n%s", text)
	}
	if strings.Contains(text, "/home/u ") || strings.HasSuffix(strings.TrimSpace(text), "/home/u") {
		t.Errorf("what must refuse a write is checked, never named in the unit:\n%s", text)
	}
}

func TestHiddenSocketsAreInaccessibleWhenTheyAreThere(t *testing.T) {
	p := Plan{Write: []string{"/a"}, Hidden: []string{"/run/user/1000/bus", "/run/user/1000/systemd/private"}}
	if text := p.DropIn(); !strings.Contains(text, "InaccessiblePaths=-/run/user/1000/bus -/run/user/1000/systemd/private\n") {
		t.Fatalf("a host with no user bus must still start the unit, so each is optional:\n%s", text)
	}
	if e := p.Expect("mnt:[1]"); len(e.Hidden) != 2 {
		t.Fatalf("and the unit checks them: %+v", e)
	}
	if strings.Contains(Plan{Write: []string{"/a"}}.DropIn(), "InaccessiblePaths") {
		t.Fatal("nothing hidden, no line")
	}
}

func TestAPlanWithNoNetworkSaysSo(t *testing.T) {
	text := Plan{Write: []string{"/a"}, NoNetwork: true}.DropIn()
	if !strings.Contains(text, "PrivateNetwork=yes\n") {
		t.Fatalf("no PrivateNetwork line:\n%s", text)
	}
}

func TestTheSamePlanIsTheSameText(t *testing.T) {
	a := Plan{Write: []string{"/b", "/a"}, Caches: []string{"/d", "/c"}}.DropIn()
	b := Plan{Write: []string{"/a", "/b", "/a/"}, Caches: []string{"/c", "/d"}}.DropIn()
	if a != b {
		t.Fatalf("order and repeats must not change the file, or it is rewritten at every start:\n%s\n%s", a, b)
	}
}

func TestAPathThatNeedsQuotingIsQuotedWhole(t *testing.T) {
	text := Plan{Write: []string{"/my repo"}, Caches: []string{"/100% mine"}}.DropIn()
	if !strings.Contains(text, `ReadWritePaths="/my repo" "-/100%% mine"`) {
		t.Fatalf("quoting:\n%s", text)
	}
}

func TestAPlanSystemdWouldMisreadIsRefused(t *testing.T) {
	for name, p := range map[string]Plan{
		"relative":    {Write: []string{"repo"}},
		"line break":  {Caches: []string{"/a\nExecStart=/bin/evil"}},
		"under /tmp":  {Write: []string{"/tmp/wt/task-1"}},
		"is /var/tmp": {Sealed: []string{"/var/tmp"}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: a plan that cannot be walled must be refused", name)
		}
	}
	if err := ValidPath("/tmpfiles/x"); err != nil {
		t.Errorf("a folder that only starts like /tmp is fine: %v", err)
	}
}

func TestAPlanThatNamesAMissingFolderSaysWhichOne(t *testing.T) {
	// Under home, not t.TempDir: a walled folder is never under /tmp.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	gone := filepath.Join(home, ".ccmux-walls-test-not-there")
	if err := (Plan{Write: []string{home}}).Validate(); err != nil {
		t.Fatalf("a folder that is there: %v", err)
	}
	if err := (Plan{Write: []string{home, gone}}).Validate(); err == nil || !strings.Contains(err.Error(), gone) {
		t.Fatalf("systemd fails such a unit with a status that names no path; the plan must: %v", err)
	}
	if err := (Plan{Write: []string{home}, Caches: []string{gone}}).Validate(); err != nil {
		t.Fatalf("a cache may be missing: %v", err)
	}
}

func TestWhatAUnitChecksIsWhatItsPlanSays(t *testing.T) {
	p := Plan{Write: []string{"/wt"}, Caches: []string{"/cache"}, Sealed: []string{"/repo/.git/hooks"}, Outside: []string{"/home/u"}, NoNetwork: true}
	e := p.Expect("mnt:[1]")
	if e.HostNS != "mnt:[1]" || !e.NoNetwork {
		t.Fatalf("expect: %+v", e)
	}
	if len(e.Writable) != 1 || e.Writable[0] != "/wt" {
		t.Errorf("only what must be there is checked as writable (a cache may be missing): %v", e.Writable)
	}
	if strings.Join(e.Refused, " ") != "/repo/.git/hooks /home/u" {
		t.Errorf("refused: %v", e.Refused)
	}
}

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

func TestASharedFolderIsEmptiedButForTheUnitsOwnPart(t *testing.T) {
	p := Plan{
		Write: []string{"/wt"},
		Own: []Own{
			{Dir: "/state/sessions", Write: []string{"/state/sessions/s1.own", "/state/sessions/s1.media"}, Read: []string{"/state/sessions/s1.json", "/state/sessions/s1.brief.md"}},
			{Dir: "/run/ccmuxd/s", Write: []string{"/run/ccmuxd/s/s1.d"}},
		},
	}
	text := p.DropIn()
	for _, want := range []string{
		"TemporaryFileSystem=/run/ccmuxd/s:ro /state/sessions:ro\n",
		"BindPaths=/run/ccmuxd/s/s1.d /state/sessions/s1.media /state/sessions/s1.own\n",
		"BindReadOnlyPaths=-/state/sessions/s1.brief.md -/state/sessions/s1.json\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the drop-in misses %q:\n%s", want, text)
		}
	}
	e := p.Expect("mnt:[1]")
	if got := strings.Join(e.Only["/state/sessions"], " "); got != "s1.media s1.own s1.brief.md s1.json" {
		t.Errorf("the unit checks that the folder shows only its own: %q", got)
	}
	if got := strings.Join(e.Writable, " "); got != "/run/ccmuxd/s/s1.d /state/sessions/s1.media /state/sessions/s1.own /wt" {
		t.Errorf("and that its own part takes a write: %q", got)
	}
	if got := strings.Join(p.Open(), " "); !strings.Contains(got, "/state/sessions/s1.own") {
		t.Errorf("a second wall inside the first opens it too: %q", got)
	}
}

func TestAPrivateFolderStandsInForTheSharedOne(t *testing.T) {
	p := Plan{Write: []string{"/wt"}, Private: []string{"/home/u/.claude/shell-snapshots", "/home/u/.claude/session-env"}, PrivateRoot: "/state/sessions/s1.own"}
	text := p.DropIn()
	want := "BindPaths=/state/sessions/s1.own/private/home_u_.claude_session-env:/home/u/.claude/session-env /state/sessions/s1.own/private/home_u_.claude_shell-snapshots:/home/u/.claude/shell-snapshots\n"
	if !strings.Contains(text, want) {
		t.Fatalf("each private folder is the unit's own, mounted in the shared one's place:\n%s", text)
	}
	if e := p.Expect("mnt:[1]"); !strings.Contains(strings.Join(e.Writable, " "), "/home/u/.claude/shell-snapshots") {
		t.Fatalf("the unit checks it can write them: %v", e.Writable)
	}
}

func TestAPlanThatDoesNotHoldTogetherIsRefused(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for name, p := range map[string]Plan{
		"private folders and nowhere to keep them": {Private: []string{home}},
		"an own part outside its shared folder":    {Own: []Own{{Dir: home, Write: []string{"/usr"}}}},
		"the shared folder as its own part":        {Own: []Own{{Dir: home, Read: []string{home}}}},
		"a shared folder that is not there":        {Own: []Own{{Dir: filepath.Join(home, ".ccmux-walls-test-not-there")}}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

func TestMakePrivateMakesTheUnitsOwnFolders(t *testing.T) {
	root := t.TempDir()
	p := Plan{Private: []string{"/home/u/.claude/sessions"}, PrivateRoot: root}
	if err := p.MakePrivate(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "private", "home_u_.claude_sessions")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the folder that stands in for it is there, and the unit's alone: %v %v", fi, err)
	}
}

func TestHiddenFilesAndFoldersAreInaccessibleToo(t *testing.T) {
	p := Plan{Write: []string{"/a"}, Hidden: []string{"/run/ccmuxd/private", "/home/u/.config/ccmux"}}
	if text := p.DropIn(); !strings.Contains(text, "InaccessiblePaths=-/home/u/.config/ccmux -/run/ccmuxd/private\n") {
		t.Fatalf("hidden:\n%s", text)
	}
}

func TestAFolderThatStandsApartIsMountedInTheSharedOnesPlace(t *testing.T) {
	p := Plan{Write: []string{"/wt"}, Apart: []Apart{{From: "/state/walled/go_pkg_mod", To: "/home/u/go/pkg/mod"}, {From: "/state/walled/.cache", To: "/home/u/.cache"}}}
	text := p.DropIn()
	if !strings.Contains(text, "BindPaths=/state/walled/.cache:/home/u/.cache /state/walled/go_pkg_mod:/home/u/go/pkg/mod\n") {
		t.Fatalf("each is mounted over the folder it stands in for:\n%s", text)
	}
	if strings.Contains(text, "ReadWritePaths=/wt -/home/u/.cache") || strings.Contains(text, " -/home/u/.cache") {
		t.Fatalf("the person's own cache is never opened:\n%s", text)
	}
	e := p.Expect("mnt:[1]")
	if got := strings.Join(e.Writable, " "); got != "/home/u/.cache /home/u/go/pkg/mod /wt" {
		t.Fatalf("the unit checks it can write there, which only the mount allows: %q", got)
	}
	if err := (Plan{Apart: []Apart{{From: "/not/there", To: "/usr"}}}).Validate(); err == nil {
		t.Fatal("a folder to mount that is not there fails the unit; the plan must say so first")
	}
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

const systemdFixture = `[Unit]
Description=ccmux daemon (ccmuxd)

[Service]
ExecStart=/home/sanlabs/.local/bin/ccmuxd -addr 127.0.0.1:7900 -tsnet -tsnet-hostname sanlabs -hub -projects-root "/srv/my projects"
Environment=PATH=/usr/local/bin:/usr/bin
Restart=on-failure

[Install]
WantedBy=default.target
`

const plistFixture = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.ccmux.ccmuxd</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/p/.local/bin/ccmuxd</string>
        <string>-addr</string>
        <string>127.0.0.1:7900</string>
        <string>-tsnet</string>
        <string>-tsnet-hostname</string>
        <string>macbook-hub</string>
        <string>-hub</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/bin</string>
    </dict>
</dict>
</plist>
`

// The migration path: hosts installed before install.json existed must have
// their answers recovered from the service file the installer wrote — this is
// exactly the tradestation-vs-sanlabs re-prompt bug.
func TestSavedFromServiceArgs_BothFormats(t *testing.T) {
	sysd := savedFromServiceArgs(parseServiceArgs(systemdFixture))
	if sysd == nil || sysd.Hostname != "sanlabs" || !sysd.Hub || !sysd.Tsnet ||
		sysd.Addr != "127.0.0.1:7900" || sysd.ProjectsRoot != "/srv/my projects" {
		t.Fatalf("systemd recovery = %+v", sysd)
	}

	mac := savedFromServiceArgs(parseServiceArgs(plistFixture))
	if mac == nil || mac.Hostname != "macbook-hub" || !mac.Hub || !mac.Tsnet {
		t.Fatalf("plist recovery = %+v", mac)
	}

	if got := savedFromServiceArgs(parseServiceArgs("not a service file at all")); got != nil {
		t.Fatalf("garbage recovered as config: %+v", got)
	}
}

func TestSplitExecLine_QuotedPaths(t *testing.T) {
	got := splitExecLine(`/bin/ccmuxd -projects-root "/srv/my projects" -hub`)
	want := []string{"/bin/ccmuxd", "-projects-root", "/srv/my projects", "-hub"}
	if len(got) != len(want) {
		t.Fatalf("tokens = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSavedInstall_JSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.json")
	if err := os.WriteFile(path, []byte(`{"addr":"127.0.0.1:7900","hostname":"sanlabs","tsnet":true,"hub":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := loadSavedInstall(path)
	if s == nil || s.Hostname != "sanlabs" || !s.Tsnet || s.Hub {
		t.Fatalf("loaded = %+v", s)
	}
	if loadSavedInstall(filepath.Join(t.TempDir(), "absent.json")) != nil {
		t.Fatal("absent file loaded as config")
	}
}

// Explicit flags always beat remembered answers; everything else is filled in.
func TestApplySaved_ExplicitFlagsWin(t *testing.T) {
	prev := &savedInstall{Addr: "127.0.0.1:7900", Hostname: "sanlabs", Tsnet: true, Hub: true}

	o := &installOpts{Hostname: "renamed", Tsnet: true}
	applySaved(o, prev, map[string]bool{"hostname": true})
	if o.Hostname != "renamed" {
		t.Fatalf("explicit hostname overridden: %q", o.Hostname)
	}
	if !o.Hub || o.Addr != "127.0.0.1:7900" {
		t.Fatalf("unset fields not filled from previous: %+v", o)
	}

	// Turning the hub role off explicitly must stick even though prev has it on.
	o2 := &installOpts{Tsnet: true, Hub: false}
	applySaved(o2, prev, map[string]bool{"hub": true})
	if o2.Hub {
		t.Fatal("explicit -hub=false overridden by remembered hub role")
	}
}

// The merge CAN produce hub-without-tsnet (explicit -hub onto a saved no-tsnet
// install, or -no-tsnet onto a saved hub) — which is why cmdInstall re-checks
// the invariant AFTER applySaved instead of at parse time. buildServiceConfig
// would otherwise silently drop the hub role: -hub is only emitted inside the
// tsnet branch.
func TestApplySaved_CanProduceHubWithoutTsnet(t *testing.T) {
	o := &installOpts{Tsnet: true, Hub: true} // parsed from: -hub
	applySaved(o, &savedInstall{Tsnet: false}, map[string]bool{"hub": true})
	if o.Tsnet || !o.Hub {
		t.Fatalf("merge = %+v — scenario changed, revisit cmdInstall's invariant check", o)
	}
	args := buildServiceConfig(o, "/bin/ccmuxd").Args
	for _, a := range args {
		if a == "-hub" {
			t.Fatalf("hub emitted without tsnet: %q — the invariant check may now be removable", args)
		}
	}
}

package walls

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Host is what this host allows, as the Sandbox tab of Settings shows it.
type Host struct {
	// Caps: the memory, CPU and process limits hold for a user's units.
	Caps    bool   `json:"caps"`
	CapsWhy string `json:"capsWhy,omitempty"`
	// Walls: a trial unit really ran behind walls.
	Walls    bool   `json:"walls"`
	WallsWhy string `json:"wallsWhy,omitempty"`
	// Bwrap and Socat are what Claude Code's own sandbox for shell commands
	// needs: bubblewrap that can build a sandbox here, and socat installed.
	Bwrap    bool   `json:"bwrap"`
	BwrapWhy string `json:"bwrapWhy,omitempty"`
	Socat    bool   `json:"socat"`
	// Fix is the commands a person runs to get what is missing, in order.
	Fix []string `json:"fix,omitempty"`
	// CheckedAt is when the host was last asked, in Unix milliseconds.
	CheckedAt int64 `json:"checkedAt"`
}

// ClaudeSandbox reports whether Claude Code's own sandbox can run here.
func (h Host) ClaudeSandbox() bool { return h.Bwrap && h.Socat }

// usernsSwitch is the kernel setting Ubuntu ships switched on, which keeps
// an unprivileged program (systemd's user manager, bubblewrap) from the
// rights it needs inside a user namespace: walls then silently do nothing.
const usernsSwitch = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// The commands that fix a host, as a person pastes them.
const (
	fixUserns = "sudo sysctl kernel.apparmor_restrict_unprivileged_userns=0 && echo 'kernel.apparmor_restrict_unprivileged_userns = 0' | sudo tee /etc/sysctl.d/60-ccmux-userns.conf"
	fixTools  = "sudo apt install bubblewrap socat"
)

// ProbeVerb is the daemon binary's verb the trial unit runs: Check, inside
// walls made the way a real unit's are.
const ProbeVerb = "walls-probe"

// Prober asks the host what it allows, and remembers the answer.
type Prober struct {
	// Self is the daemon's binary, whose ProbeVerb the trial unit runs.
	Self string
	// Home is the folder the trial unit must not be able to write.
	Home string
	// Run runs a command and returns what it printed; nil is the real one.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Now is the clock; nil is the real one.
	Now func() time.Time

	mu   sync.Mutex
	last *Host
}

// retryAfter is how long a "no" is believed before the host is asked
// again: a person who just ran the fix does not restart the daemon for it.
const retryAfter = 20 * time.Second

func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prober) run(ctx context.Context, name string, args ...string) (string, error) {
	if p.Run != nil {
		return p.Run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Probe asks the host now.
func (p *Prober) Probe() Host {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var h Host
	h.Caps, h.CapsWhy = capsDelegated()
	h.Walls, h.WallsWhy = p.trial(ctx)
	h.Bwrap, h.BwrapWhy = p.bwrap(ctx)
	_, err := exec.LookPath("socat")
	h.Socat = err == nil
	h.Fix = fixes(h, usernsRestricted())
	h.CheckedAt = p.now().UnixMilli()
	p.mu.Lock()
	p.last = &h
	p.mu.Unlock()
	return h
}

// Last is the newest answer, asking first when there is none yet.
func (p *Prober) Last() Host {
	p.mu.Lock()
	last := p.last
	p.mu.Unlock()
	if last == nil {
		return p.Probe()
	}
	return *last
}

// Ready is nil when this host walls a unit, and says why not otherwise. A
// "no" older than retryAfter is asked again.
func (p *Prober) Ready() error {
	h := p.Last()
	if !h.Walls && p.now().Sub(time.UnixMilli(h.CheckedAt)) > retryAfter {
		h = p.Probe()
	}
	if h.Walls {
		return nil
	}
	return fmt.Errorf("the sandbox is on, and this host does not wall a unit: %s. %s", h.WallsWhy, Fix)
}

// trial runs a real unit behind walls and has it check them: the one
// honest answer to "do walls work here". systemd-run exits with the
// unit's own status, and what the check printed is why not.
func (p *Prober) trial(ctx context.Context) (bool, string) {
	ns, err := MountNS()
	if err != nil {
		return false, "the daemon's mount namespace cannot be read: " + err.Error()
	}
	out, err := p.run(ctx, "systemd-run", "--user", "--wait", "--pipe", "--quiet", "--collect",
		"-p", "ProtectSystem=strict", "-p", "ProtectHome=read-only", "-p", "PrivateTmp=yes",
		p.Self, ProbeVerb, ns, p.Home)
	if err == nil {
		return true, ""
	}
	if out == "" {
		out = err.Error()
	}
	return false, lastLine(out)
}

// bwrap has bubblewrap build the smallest sandbox there is.
func (p *Prober) bwrap(ctx context.Context) (bool, string) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return false, "bubblewrap is not installed"
	}
	out, err := p.run(ctx, "bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "true")
	if err == nil {
		return true, ""
	}
	if out == "" {
		out = err.Error()
	}
	return false, lastLine(out)
}

// RunProbe is ProbeVerb: check that this process is walled off from home,
// print why not, and exit with ExitRefused then.
func RunProbe(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: ccmuxd "+ProbeVerb+" HOST-MOUNT-NAMESPACE HOME")
		return 2
	}
	if err := Check(Expect{HostNS: args[0], Refused: []string{args[1]}}); err != nil {
		fmt.Println(err.Error())
		return ExitRefused
	}
	return 0
}

// fixes lists what a person runs for what the host lacks.
func fixes(h Host, restricted bool) []string {
	var out []string
	if restricted && (!h.Walls || !h.Bwrap) {
		out = append(out, fixUserns)
	}
	if !h.Socat || h.BwrapWhy == "bubblewrap is not installed" {
		out = append(out, fixTools)
	}
	return out
}

// usernsRestricted reports whether the kernel setting that stops walls is
// switched on. A host without the setting has no such restriction.
func usernsRestricted() bool {
	b, err := os.ReadFile(usernsSwitch)
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// capsDelegated reports whether the user's systemd manager was given the
// controllers the caps need. Without them systemd accepts MemoryMax and
// the rest and applies nothing.
func capsDelegated() (bool, string) {
	own, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false, "the daemon's cgroup cannot be read: " + err.Error()
	}
	return delegated(string(own), os.ReadFile)
}

func delegated(own string, read func(string) ([]byte, error)) (bool, string) {
	mgr := managerCgroup(own)
	if mgr == "" {
		return false, "the daemon does not run under a user's systemd manager, so its units' limits cannot be told"
	}
	b, err := read(filepath.Join("/sys/fs/cgroup", mgr, "cgroup.controllers"))
	if err != nil {
		return false, "the user manager's controllers cannot be read: " + err.Error()
	}
	have := strings.Fields(string(b))
	var missing []string
	for _, want := range []string{"memory", "cpu", "pids"} {
		if !contains(have, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return false, "the user's systemd manager was not given the " + strings.Join(missing, ", ") + " controller, so that limit is not applied"
	}
	return true, ""
}

// managerCgroup is the cgroup of the user's systemd manager, read off the
// daemon's own ("0::/user.slice/user-1000.slice/user@1000.service/app.slice/…").
func managerCgroup(own string) string {
	for _, line := range strings.Split(own, "\n") {
		path, ok := strings.CutPrefix(line, "0::")
		if !ok {
			continue
		}
		parts := strings.Split(path, "/")
		for i, part := range parts {
			if strings.HasPrefix(part, "user@") && strings.HasSuffix(part, ".service") {
				return strings.Join(parts[:i+1], "/")
			}
		}
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// lastLine is the last line of text that says anything.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

//go:build linux

// Linux service writer: a systemd --user unit, matching the Mac's per-user model
// (runs as the login user, tmux sessions and projects-root under $HOME). Linger
// is enabled so it survives logout; stdout goes to the journal.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const linuxUnit = "ccmuxd.service"

// A sane PATH for a non-login service context; the daemon shells out to tmux/git.
const linuxPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", linuxUnit)
}

// installHint is distro-agnostic: we can't know the package manager.
func installHint(pkg string) string {
	return "install " + pkg + " with your package manager (apt/dnf/pacman/…)"
}

func writeAndStartService(cfg serviceConfig) error {
	p := unitPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(renderUnit(cfg)), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	enableLinger()
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", linuxUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable --now: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Printf("  systemd user service %s installed (logs: journalctl --user -u ccmuxd -f)\n", linuxUnit)
	return nil
}

func stopAndRemoveService(purge bool) error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", linuxUnit).Run()
	if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Printf("  removed systemd user service %s\n", linuxUnit)
	if purge {
		purgeState()
	}
	return nil
}

// enableLinger lets the user service keep running after logout / at boot. Many
// systems allow a user to enable it for themselves; if policy refuses, we tell
// the user the sudo command rather than fail the install.
func enableLinger() {
	u, err := user.Current()
	if err != nil {
		return
	}
	if out, err := exec.Command("loginctl", "enable-linger", u.Username).CombinedOutput(); err != nil {
		fmt.Printf("  note: could not enable linger (service may stop at logout). Run:\n    sudo loginctl enable-linger %s\n    (%s)\n", u.Username, strings.TrimSpace(string(out)))
	}
}

const unitTemplate = `[Unit]
Description=ccmux daemon (ccmuxd)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
%sRestart=on-failure
RestartSec=10
WorkingDirectory=%s

[Install]
WantedBy=default.target
`

func renderUnit(cfg serviceConfig) string {
	var env strings.Builder
	fmt.Fprintf(&env, "Environment=PATH=%s\n", linuxPATH)
	for k, v := range cfg.Env {
		fmt.Fprintf(&env, "Environment=%s=%s\n", k, v)
	}
	return fmt.Sprintf(unitTemplate, execLine(cfg.BinPath, cfg.Args), env.String(), cfg.WorkingDir)
}

// execLine renders the ExecStart command, double-quoting any argument with a
// space (e.g. a projects-root path) as systemd requires.
func execLine(bin string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, bin)
	for _, a := range args {
		if strings.ContainsAny(a, " \t") {
			a = `"` + a + `"`
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

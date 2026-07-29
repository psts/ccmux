//go:build darwin

// macOS service writer: a per-user LaunchAgent, matching the plist ccmux has run
// by hand until now (same label, KeepAlive, Homebrew-inclusive PATH). Loaded into
// the GUI domain so it starts at login and survives crashes.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const darwinLabel = "com.ccmux.ccmuxd"

// launchd's default PATH lacks Homebrew; the daemon shells out to tmux and git.
const darwinPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", darwinLabel+".plist")
}

func defaultLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Logs", "ccmuxd.log")
}

// installHint is the macOS package-manager suggestion for a missing dependency.
func installHint(pkg string) string { return "install it with: brew install " + pkg }

func writeAndStartService(cfg serviceConfig) error {
	logPath := defaultLogPath()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	p := plistPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(renderPlist(cfg, logPath)), 0o644); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	// bootout first so a re-install replaces cleanly (bootstrap fails if loaded).
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+darwinLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid, p).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = exec.Command("launchctl", "kickstart", "-k", "gui/"+uid+"/"+darwinLabel).Run()
	fmt.Printf("  launchd agent %s installed (logs: %s)\n", darwinLabel, logPath)
	return nil
}

func stopAndRemoveService(purge bool) error {
	uid := strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+darwinLabel).Run()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("  removed launchd agent %s\n", darwinLabel)
	if purge {
		purgeState()
	}
	return nil
}

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>EnvironmentVariables</key>
    <dict>
%s    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <!-- Don't restart-flap if it crashes at boot before the network is up. -->
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
</dict>
</plist>
`

func renderPlist(cfg serviceConfig, logPath string) string {
	var args strings.Builder
	for _, a := range append([]string{cfg.BinPath}, cfg.Args...) {
		fmt.Fprintf(&args, "        <string>%s</string>\n", xmlEsc(a))
	}
	var env strings.Builder
	fmt.Fprintf(&env, "        <key>PATH</key>\n        <string>%s</string>\n", darwinPATH)
	for k, v := range cfg.Env {
		fmt.Fprintf(&env, "        <key>%s</key>\n        <string>%s</string>\n", xmlEsc(k), xmlEsc(v))
	}
	return fmt.Sprintf(plistTemplate, darwinLabel, args.String(), env.String(), xmlEsc(logPath), xmlEsc(logPath), xmlEsc(cfg.WorkingDir))
}

func xmlEsc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

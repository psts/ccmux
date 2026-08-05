// Previous-install memory, so a re-run of the installer is an UPDATE: it keeps
// the answers the user already gave instead of re-asking with wrong defaults
// (the hostname prompt offered the machine's OS hostname, and accepting it
// would rewrite the service with the wrong tailnet node name).
//
// Source of truth is install.json in the config dir, written on every
// successful install. Installs made before that file existed still get update
// behavior through a fallback: the flags are recovered from the service file
// the previous install wrote (systemd ExecStart= line, or the plist's
// ProgramArguments strings).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// savedInstall is the subset of installOpts worth remembering. The auth key is
// deliberately absent: it is consumed into tailnet state at first join and
// must never be persisted here.
type savedInstall struct {
	Addr         string `json:"addr"`
	Hostname     string `json:"hostname"`
	ProjectsRoot string `json:"projects_root,omitempty"`
	Tsnet        bool   `json:"tsnet"`
	Hub          bool   `json:"hub"`
}

func installConfigPath() string { return filepath.Join(configDir(), "install.json") }

// loadPreviousInstall finds the last install's configuration: install.json
// first, else recovered from the existing service file. Returns nil (with an
// empty source) on a genuinely fresh host.
func loadPreviousInstall() (*savedInstall, string) {
	if s := loadSavedInstall(installConfigPath()); s != nil {
		return s, "install.json"
	}
	data, err := os.ReadFile(serviceFilePath())
	if err != nil {
		return nil, ""
	}
	if s := savedFromServiceArgs(parseServiceArgs(string(data))); s != nil {
		return s, "existing service file"
	}
	return nil, ""
}

func loadSavedInstall(path string) *savedInstall {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var s savedInstall
	if json.Unmarshal(data, &s) != nil {
		return nil
	}
	return &s
}

// saveInstallConfig records the resolved options after a successful install.
// Best-effort: a write failure only costs the next update its silent defaults.
func saveInstallConfig(o *installOpts) {
	s := savedInstall{Addr: o.Addr, Hostname: o.Hostname,
		ProjectsRoot: o.ProjectsRoot, Tsnet: o.Tsnet, Hub: o.Hub}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	path := installConfigPath()
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// applySaved fills every option the caller did NOT set explicitly from the
// previous install. Explicit flags always win — that is how settings change.
func applySaved(o *installOpts, prev *savedInstall, set map[string]bool) {
	if !set["addr"] && prev.Addr != "" {
		o.Addr = prev.Addr
	}
	if !set["hostname"] && prev.Hostname != "" {
		o.Hostname = prev.Hostname
	}
	if !set["projects-root"] && prev.ProjectsRoot != "" {
		o.ProjectsRoot = prev.ProjectsRoot
	}
	if !set["no-tsnet"] {
		o.Tsnet = prev.Tsnet
	}
	if !set["hub"] {
		o.Hub = prev.Hub
	}
}

var plistStringRe = regexp.MustCompile(`<string>([^<]*)</string>`)

// parseServiceArgs extracts the daemon invocation's argument tokens from a
// service file body, whichever format it is. For a plist this returns every
// <string> value (environment values included) — harmless, because the
// consumer only matches exact flag tokens and their following value.
func parseServiceArgs(body string) []string {
	if i := strings.Index(body, "ExecStart="); i >= 0 {
		line := body[i+len("ExecStart="):]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		return splitExecLine(line)
	}
	var out []string
	for _, m := range plistStringRe.FindAllStringSubmatch(body, -1) {
		out = append(out, xmlUnesc(m[1]))
	}
	return out
}

// splitExecLine is the inverse of execLine: whitespace-separated tokens, with
// double quotes grouping (a projects-root path with spaces).
func splitExecLine(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// savedFromServiceArgs recovers the install options from the previous service
// invocation's tokens. Returns nil when the tokens carry no recognizable
// install flag at all (not a ccmuxd service file worth trusting).
func savedFromServiceArgs(tokens []string) *savedInstall {
	s := &savedInstall{}
	found := false
	for i, tok := range tokens {
		next := ""
		if i+1 < len(tokens) {
			next = tokens[i+1]
		}
		switch tok {
		case "-addr":
			s.Addr, found = next, true
		case "-tsnet":
			s.Tsnet, found = true, true
		case "-tsnet-hostname":
			s.Hostname, found = next, true
		case "-hub":
			s.Hub, found = true, true
		case "-projects-root":
			s.ProjectsRoot, found = next, true
		}
	}
	if !found {
		return nil
	}
	return s
}

func xmlUnesc(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`)
	return r.Replace(s)
}

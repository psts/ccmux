package portdetect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// Port flags in dev/start scripts, tried in order. Only these two scripts are
// parsed — e2e/analyze/etc. mention URLs and would false-positive.
var portFlags = []*regexp.Regexp{
	regexp.MustCompile(`--port[= ](\d{2,5})`),
	regexp.MustCompile(`(?:^|\s)-p[= ]?(\d{2,5})`),
	regexp.MustCompile(`\bPORT=(\d{2,5})`),
}

// Framework defaults when a dev script names the tool but no port.
var frameworkDefaults = []struct {
	marker *regexp.Regexp
	port   int
}{
	{regexp.MustCompile(`\bnext dev\b`), 3000},
	{regexp.MustCompile(`\breact-scripts start\b`), 3000},
	{regexp.MustCompile(`\bvite\b(?:\s|$)`), 5173},
}

// parsePackages reads the root package.json plus one level of monorepo
// packages (apps/*, packages/* — the turbo/workspaces convention), returning
// at most one port per package. Root gets Name "" (the repo itself); nested
// packages are labeled by folder name.
func parsePackages(dir string) []Suggestion {
	var out []Suggestion
	if port := packagePort(filepath.Join(dir, "package.json")); port > 0 {
		out = append(out, Suggestion{Name: "", Port: port, Source: "package.json"})
	}
	var nested []string
	for _, pattern := range []string{"apps/*/package.json", "packages/*/package.json"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		nested = append(nested, matches...)
	}
	sort.Strings(nested)
	for _, file := range nested {
		if port := packagePort(file); port > 0 {
			rel, _ := filepath.Rel(dir, file)
			out = append(out, Suggestion{Name: filepath.Base(filepath.Dir(file)), Port: port, Source: rel})
		}
	}
	return out
}

// packagePort extracts the dev-server port from one package.json ("dev" beats
// "start"; explicit flags beat framework defaults). 0 = no signal.
func packagePort(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return 0
	}
	for _, script := range []string{pkg.Scripts["dev"], pkg.Scripts["start"]} {
		if script == "" {
			continue
		}
		for _, flag := range portFlags {
			if m := flag.FindStringSubmatch(script); m != nil {
				port, _ := strconv.Atoi(m[1])
				return port
			}
		}
		for _, fw := range frameworkDefaults {
			if fw.marker.MatchString(script) {
				return fw.port
			}
		}
	}
	return 0
}

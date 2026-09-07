package agent

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// EnvExecPath is the program that loads env files ahead of an agent's
// harness: ccmuxd's own binary, whose env-exec verb parses the files and
// execs the harness with the variables set. main sets it to the running
// executable; the default is for tests and reads as intent.
var EnvExecPath = "ccmuxd"

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnvFile reads KEY=VALUE lines from path: blank lines and # comments
// skipped, an "export " prefix tolerated, matching single or double quotes
// around a value stripped. Nothing is expanded or executed — the file is
// DATA, which is what makes it safe for any agent to write: a .env can set
// variables and nothing else. Lines that are not that are reported, not
// applied. A missing file is empty, not an error.
func ParseEnvFile(path string) (vars map[string]string, problems []string, err error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	vars = map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !envKey.MatchString(key) {
			problems = append(problems, fmt.Sprintf("%s:%d: not a KEY=VALUE line, skipped", path, n))
			continue
		}
		vars[key] = unquote(strings.TrimSpace(val))
	}
	return vars, problems, sc.Err()
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// LoadEnvFiles parses each file in order, a later file's value winning,
// and returns the variables as KEY=VALUE plus every problem found.
func LoadEnvFiles(paths []string) (vars map[string]string, problems []string, err error) {
	vars = map[string]string{}
	for _, p := range paths {
		got, probs, err := ParseEnvFile(p)
		if err != nil {
			return nil, nil, err
		}
		problems = append(problems, probs...)
		for k, v := range got {
			vars[k] = v
		}
	}
	return vars, problems, nil
}

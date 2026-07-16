package portdetect

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// composeService is the slice of a compose service we care about. Port entries
// are kept as raw nodes: compose allows scalars ("3001:3001") and mappings
// ({target, published}) in the same list.
type composeService struct {
	Ports []yaml.Node `yaml:"ports"`
}

type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
}

// parseCompose reads every compose file variant present and returns the
// host-published ports labeled by service name (extra ports of a service get
// name-<port>). Files and services are sorted so output is deterministic.
func parseCompose(dir string) []Suggestion {
	var files []string
	for _, pattern := range []string{"docker-compose*.yml", "docker-compose*.yaml", "compose.yml", "compose.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		files = append(files, matches...)
	}
	sort.Strings(files)

	var out []Suggestion
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var doc composeDoc
		if yaml.Unmarshal(raw, &doc) != nil {
			continue
		}
		names := make([]string, 0, len(doc.Services))
		for name := range doc.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for i, port := range servicePorts(doc.Services[name].Ports) {
				label := name
				if i > 0 {
					label = name + "-" + strconv.Itoa(port)
				}
				out = append(out, Suggestion{Name: label, Port: port, Source: filepath.Base(file)})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// servicePorts extracts the host-side ports from one service's ports list.
func servicePorts(nodes []yaml.Node) []int {
	var out []int
	for _, node := range nodes {
		var port int
		switch node.Kind {
		case yaml.ScalarNode:
			port = hostPortOf(node.Value)
		case yaml.MappingNode:
			var long struct {
				Published string `yaml:"published"`
			}
			if node.Decode(&long) == nil {
				port, _ = strconv.Atoi(long.Published)
			}
		}
		if port > 0 {
			out = append(out, port)
		}
	}
	return out
}

// envDefault matches compose interpolation; only ${VAR:-default} is resolvable
// without the deploy environment, so bare ${VAR} becomes "" and the entry drops.
var envDefault = regexp.MustCompile(`\$\{[^}:]+(?::-([^}]*))?\}`)

// hostPortOf parses a short-syntax ports entry and returns the HOST port:
// "3001:3001" → 3001, "127.0.0.1:5900:5900" → 5900. A single part publishes to
// a random host port and a /udp mapping isn't a dev server — both are skipped.
func hostPortOf(entry string) int {
	entry = envDefault.ReplaceAllString(entry, "$1")
	if strings.HasSuffix(entry, "/udp") {
		return 0
	}
	entry = strings.TrimSuffix(entry, "/tcp")
	parts := strings.Split(entry, ":")
	switch len(parts) {
	case 2:
		port, _ := strconv.Atoi(parts[0])
		return port
	case 3:
		port, _ := strconv.Atoi(parts[1])
		return port
	}
	return 0
}

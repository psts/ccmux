// Package portdetect guesses a repo's dev-server ports from its config files,
// to prepopulate the Hostnames sheet. Three sources, strongest first:
// docker-compose published ports (host side, with service names), package.json
// dev/start scripts (port flags or framework defaults, one level of monorepo
// recursion), and Dockerfile EXPOSE (weakest — a container port is only a hint
// about the host mapping). Pure file reads; never executes anything.
package portdetect

// Suggestion is one detected port. Name is the compose-service or package
// folder label ("" = the repo itself); the API layer turns it into a hostname.
type Suggestion struct {
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Source string `json:"source"`
}

// nonHTTPPorts are well-known raw-TCP protocol defaults (databases, queues,
// VNC) that compose files publish for infra services. The devhost proxy speaks
// HTTP, so a hostname mapped to one of these could never work — never suggest
// them.
var nonHTTPPorts = map[int]bool{
	1433:  true, // mssql
	3306:  true, // mysql
	5432:  true, // postgres
	5672:  true, // amqp
	5900:  true, // vnc
	6379:  true, // redis
	9092:  true, // kafka
	11211: true, // memcached
	27017: true, // mongodb
}

// Detect scans repoPath and returns deduped suggestions, strongest source
// first. A port seen by a stronger source hides the same port from weaker
// ones (compose knows the service name; EXPOSE knows nothing). Dockerfile
// EXPOSE is consulted only when nothing stronger matched at all: once compose
// or package.json answered, EXPOSE is describing the production container
// (checked against the real repos — it contributed only stale noise there).
func Detect(repoPath string) []Suggestion {
	var out []Suggestion
	seen := map[int]bool{}
	add := func(batch []Suggestion) {
		for _, s := range batch {
			if s.Port < 1 || s.Port > 65535 || seen[s.Port] || nonHTTPPorts[s.Port] {
				continue
			}
			seen[s.Port] = true
			out = append(out, s)
		}
	}
	add(parseCompose(repoPath))
	add(parsePackages(repoPath))
	if len(out) == 0 {
		add(parseDockerfile(repoPath))
	}
	return out
}

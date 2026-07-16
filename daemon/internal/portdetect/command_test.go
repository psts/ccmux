package portdetect

import "testing"

func TestDetectCommand(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		command string
		source  string
	}{
		{
			// admin-style: pnpm lockfile + a dev script → the developer
			// entrypoint beats the compose file that also exists.
			name: "pnpm dev script beats compose",
			files: map[string]string{
				"package.json":       `{"scripts": {"dev": "turbo dev"}}`,
				"pnpm-lock.yaml":     "",
				"docker-compose.yml": "services:\n  web:\n    ports: [\"3001:3001\"]\n",
			},
			command: "pnpm dev", source: "package.json",
		},
		{
			// app/website-style: npm lockfile.
			name: "npm dev script",
			files: map[string]string{
				"package.json":      `{"scripts": {"dev": "next dev -p 4000"}}`,
				"package-lock.json": "",
			},
			command: "npm run dev", source: "package.json",
		},
		{
			name: "yarn dev script",
			files: map[string]string{
				"package.json": `{"scripts": {"dev": "vite"}}`,
				"yarn.lock":    "",
			},
			command: "yarn dev", source: "package.json",
		},
		{
			// CRA convention: no dev script, start is the dev server.
			name: "react-scripts start",
			files: map[string]string{
				"package.json": `{"scripts": {"start": "react-scripts start"}}`,
			},
			command: "npm start", source: "package.json",
		},
		{
			name: "compose only",
			files: map[string]string{
				"docker-compose.yml": "services:\n  web:\n    ports: [\"3000:3000\"]\n",
			},
			command: "docker compose up", source: "docker-compose.yml",
		},
		{
			// backend-style: Procfile only — its web command is a guess (often
			// the production entrypoint), which the source label admits.
			name: "procfile web entry",
			files: map[string]string{
				"Procfile": "web: gunicorn chartlabs.wsgi:application --workers 9\n",
			},
			command: "gunicorn chartlabs.wsgi:application --workers 9", source: "Procfile (verify — often the production command)",
		},
		{
			// A package.json without dev/start scripts falls through to compose.
			name: "portless package.json falls through",
			files: map[string]string{
				"package.json": `{"scripts": {"build": "tsc"}}`,
				"compose.yaml": "services:\n  web:\n    ports: [\"3000:3000\"]\n",
			},
			command: "docker compose up", source: "compose.yaml",
		},
		{
			name:    "nothing",
			files:   map[string]string{},
			command: "", source: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := write(t, c.files)
			command, source := DetectCommand(dir)
			if command != c.command || source != c.source {
				t.Fatalf("got (%q, %q), want (%q, %q)", command, source, c.command, c.source)
			}
		})
	}
}

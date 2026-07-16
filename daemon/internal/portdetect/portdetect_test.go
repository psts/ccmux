package portdetect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// write lays out a fixture repo: map of relative path → content.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Modeled on the `website` repo: a root package.json with an explicit -p flag,
// plus scripts that mention ports in ways that must NOT match (e2e URLs).
func TestDetect_PackageJSONPortFlag(t *testing.T) {
	dir := write(t, map[string]string{
		"package.json": `{"scripts": {
			"dev": "NODE_EXTRA_CA_CERTS='/x/rootCA.pem' next dev -p 3003",
			"e2e:codegen": "playwright codegen http://localhost:4000",
			"analyze": "ANALYZE=true next build"
		}}`,
	})
	want := []Suggestion{{Name: "", Port: 3003, Source: "package.json"}}
	if got := Detect(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDetect_FrameworkDefaultsAndPortEnv(t *testing.T) {
	// "next dev" with no flag → 3000; PORT= wins over the default when present.
	dir := write(t, map[string]string{"package.json": `{"scripts": {"dev": "next dev"}}`})
	if got := Detect(dir); len(got) != 1 || got[0].Port != 3000 {
		t.Fatalf("next default: %+v", got)
	}
	dir = write(t, map[string]string{"package.json": `{"scripts": {"dev": "PORT=5005 node server.js"}}`})
	if got := Detect(dir); len(got) != 1 || got[0].Port != 5005 {
		t.Fatalf("PORT= env: %+v", got)
	}
	dir = write(t, map[string]string{"package.json": `{"scripts": {"dev": "vite"}}`})
	if got := Detect(dir); len(got) != 1 || got[0].Port != 5173 {
		t.Fatalf("vite default: %+v", got)
	}
}

// Modeled on the `admin` repo: compose with two services (one holding a second
// env-interpolated port), a portless turbo root script, and monorepo apps/*
// whose ports duplicate compose (dedup keeps the compose entry and its name).
func TestDetect_ComposeMonorepo(t *testing.T) {
	dir := write(t, map[string]string{
		"docker-compose.yml": `
services:
  web:
    ports:
      - "3001:3001"
  api:
    ports:
      - "8001:8001"
      - "127.0.0.1:${VNC_PORT:-5900}:${VNC_PORT:-5900}"
volumes:
  browser-data:
`,
		"package.json":          `{"scripts": {"dev": "turbo dev"}}`,
		"apps/web/package.json": `{"scripts": {"dev": "next dev --turbopack -p 3001"}}`,
		"apps/api/package.json": `{"scripts": {"dev": "uvicorn app.main:app --reload --port 8001"}}`,
	})
	want := []Suggestion{
		{Name: "api", Port: 8001, Source: "docker-compose.yml"},
		{Name: "api-5900", Port: 5900, Source: "docker-compose.yml"},
		{Name: "web", Port: 3001, Source: "docker-compose.yml"},
	}
	if got := Detect(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDetect_ComposeForms(t *testing.T) {
	// Long-form published ports, container-only strings (skipped), /udp (skipped).
	dir := write(t, map[string]string{
		"compose.yaml": `
services:
  svc:
    ports:
      - target: 80
        published: 8080
      - "3000"
      - "514:514/udp"
`,
	})
	want := []Suggestion{{Name: "svc", Port: 8080, Source: "compose.yaml"}}
	if got := Detect(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Modeled on the `backend` repo: EXPOSE is the only (weak) signal.
func TestDetect_DockerfileExpose(t *testing.T) {
	dir := write(t, map[string]string{
		"Dockerfile": "FROM golang:1.26\n# Expose port\nEXPOSE 7000\nCMD [\"./srv\"]\n",
	})
	want := []Suggestion{{Name: "", Port: 7000, Source: "Dockerfile (EXPOSE)"}}
	if got := Detect(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDetect_PriorityAndDedup(t *testing.T) {
	// Same port in compose and package.json → compose wins (better name); the
	// Dockerfile is skipped entirely once a stronger source answered — its
	// EXPOSE describes the production container, not the dev server (pinned
	// against the real website/app repos, where it only added stale noise).
	dir := write(t, map[string]string{
		"docker-compose.yml": "services:\n  web:\n    ports: [\"4000:4000\"]\n",
		"package.json":       `{"scripts": {"dev": "next dev -p 4000"}}`,
		"Dockerfile":         "EXPOSE 4000 9090\n",
	})
	want := []Suggestion{{Name: "web", Port: 4000, Source: "docker-compose.yml"}}
	if got := Detect(dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDetect_Empty(t *testing.T) {
	if got := Detect(t.TempDir()); len(got) != 0 {
		t.Fatalf("empty repo suggested %+v", got)
	}
	if got := Detect("/nonexistent/nope"); len(got) != 0 {
		t.Fatalf("missing repo suggested %+v", got)
	}
}

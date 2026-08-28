package portdetect

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeFiles_CanonicalPrecedenceAndOverride(t *testing.T) {
	// compose.yaml beats docker-compose.yml; its .override companion rides along.
	dir := write(t, map[string]string{
		"compose.yaml":          "services: {}\n",
		"compose.override.yaml": "services: {}\n",
		"docker-compose.yml":    "services: {}\n",
	})
	got := ComposeFiles(dir)
	want := []string{filepath.Join(dir, "compose.yaml"), filepath.Join(dir, "compose.override.yaml")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}
	if ComposeFiles(t.TempDir()) != nil {
		t.Fatal("empty repo should yield no compose files")
	}
}

func TestComposeOverride_RemapsShortAndLongSyntax(t *testing.T) {
	dir := write(t, map[string]string{
		"docker-compose.yml": `
services:
  web:
    ports:
      - "3000:3000"
      - "127.0.0.1:5900:5900"
  api:
    ports:
      - target: 4000
        published: "4000"
  db:
    ports:
      - "5432:5432"
`,
	})
	files, content, ok := ComposeOverride(dir, map[int]int{3000: 21000, 4000: 21001})
	if !ok {
		t.Fatal("expected an override")
	}
	if len(files) != 1 || files[0] != filepath.Join(dir, "docker-compose.yml") {
		t.Fatalf("files = %v", files)
	}
	for _, wantLine := range []string{
		`ports: !override`,
		`- "21000:3000"`,          // remapped short syntax
		`- "127.0.0.1:5900:5900"`, // untouched entry kept — !override replaces the list
		`- "21001:4000"`,          // remapped long syntax
	} {
		if !strings.Contains(content, wantLine) {
			t.Fatalf("override missing %q:\n%s", wantLine, content)
		}
	}
	// db had no remapped port: it must not appear (its original list stands).
	if strings.Contains(content, "db:") || strings.Contains(content, "5432") {
		t.Fatalf("untouched service leaked into the override:\n%s", content)
	}
}

func TestComposeOverride_NoMatchMeansNoOverride(t *testing.T) {
	dir := write(t, map[string]string{
		"docker-compose.yml": "services:\n  web:\n    ports:\n      - \"3000:3000\"\n",
	})
	if _, _, ok := ComposeOverride(dir, map[int]int{8080: 21000}); ok {
		t.Fatal("no published port matches — no override expected")
	}
	if _, _, ok := ComposeOverride(dir, nil); ok {
		t.Fatal("empty remap — no override expected")
	}
}

func TestPortFlagSuffix_ViteOnly(t *testing.T) {
	// vite ignores PORT env → the runner form grows a --port flag.
	vite := write(t, map[string]string{
		"package.json":   `{"scripts": {"dev": "vite"}}`,
		"pnpm-lock.yaml": "",
	})
	if got := PortFlagSuffix(vite, "pnpm dev"); got != ` --port "$PORT"` {
		t.Fatalf("pnpm+vite: %q", got)
	}
	// npm forwards args only after --.
	npmVite := write(t, map[string]string{"package.json": `{"scripts": {"dev": "vite"}}`})
	if got := PortFlagSuffix(npmVite, "npm run dev"); got != ` -- --port "$PORT"` {
		t.Fatalf("npm+vite: %q", got)
	}
	// next honors PORT env — nothing appended.
	next := write(t, map[string]string{"package.json": `{"scripts": {"dev": "next dev"}}`})
	if got := PortFlagSuffix(next, "npm run dev"); got != "" {
		t.Fatalf("next: %q", got)
	}
	// A command that isn't the detected runner form is never touched.
	if got := PortFlagSuffix(vite, "docker compose up"); got != "" {
		t.Fatalf("foreign command: %q", got)
	}
	// An explicit port anywhere wins.
	pinned := write(t, map[string]string{
		"package.json":   `{"scripts": {"dev": "vite --port 4444"}}`,
		"pnpm-lock.yaml": "",
	})
	if got := PortFlagSuffix(pinned, "pnpm dev"); got != "" {
		t.Fatalf("pinned vite port: %q", got)
	}
}

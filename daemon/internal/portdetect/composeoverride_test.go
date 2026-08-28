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
      - "${WEB_PORT:-8080}:8080"
      - "${GRPC_PORT:-7070}:7070"
      - "5353:5353/udp"
  api:
    ports:
      - target: 4000
        published: "4000"
      - target: 53
        published: "53"
        protocol: udp
  db:
    ports:
      - "5432:5432"
`,
	})
	files, content, err := ComposeOverride(dir, map[int]int{3000: 21000, 4000: 21001, 7070: 21002})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != filepath.Join(dir, "docker-compose.yml") {
		t.Fatalf("files = %v", files)
	}
	for _, wantLine := range []string{
		`ports: !override`,
		`"21000:3000"`, // remapped short syntax
		`"21002:7070"`, // remapped THROUGH env interpolation — detection sees 7070
		// Untouched entries ride along VERBATIM — !override replaces the whole
		// list, so interpolation and /udp suffixes must survive untouched.
		`"127.0.0.1:5900:5900"`,
		`"${WEB_PORT:-8080}:8080"`,
		`"5353:5353/udp"`,
		// Remapped long syntax keeps the mapping form (published swapped).
		`published: "21001"`,
		`target: 4000`,
		// The udp long-syntax neighbour keeps its protocol.
		`protocol: udp`,
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
	if files, _, err := ComposeOverride(dir, map[int]int{8080: 21000}); err != nil || len(files) != 0 {
		t.Fatalf("no published port matches — no override expected (files=%v err=%v)", files, err)
	}
	if files, _, err := ComposeOverride(dir, nil); err != nil || len(files) != 0 {
		t.Fatalf("empty remap — no override expected (files=%v err=%v)", files, err)
	}
}

// TestComposeOverride_MultiFileAccumulation pins the reason entries accumulate
// across the file set: !override replaces a service's WHOLE ports list, so a
// port declared only in the .override companion must survive into the
// generated list or the next `up` silently unpublishes it.
func TestComposeOverride_MultiFileAccumulation(t *testing.T) {
	dir := write(t, map[string]string{
		"compose.yaml":          "services:\n  web:\n    ports:\n      - \"3000:3000\"\n",
		"compose.override.yaml": "services:\n  web:\n    ports:\n      - \"9229:9229\"\n",
	})
	files, content, err := ComposeOverride(dir, map[int]int{3000: 21000})
	if err != nil || len(files) != 2 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	for _, want := range []string{`"21000:3000"`, `"9229:9229"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("override missing %q:\n%s", want, content)
		}
	}
}

// TestComposeOverride_UnparseableFileIsAnError pins the partial-list guard: a
// file in the set that cannot be parsed must fail the whole build — emitting
// an !override list built from the readable files alone would silently
// unpublish the failed file's ports.
func TestComposeOverride_UnparseableFileIsAnError(t *testing.T) {
	dir := write(t, map[string]string{
		"compose.yaml":          "services:\n  web:\n    ports:\n      - \"3000:3000\"\n",
		"compose.override.yaml": "services:\n  web:\n    ports: [unclosed\n",
	})
	if files, _, err := ComposeOverride(dir, map[int]int{3000: 21000}); err == nil || len(files) != 0 {
		t.Fatalf("want an error and no file set, got files=%v err=%v", files, err)
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

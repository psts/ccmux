package api

import (
	"slices"
	"testing"
)

func TestMatchFilesRanksFileNamesBeforePaths(t *testing.T) {
	paths := []string{"src/components/Header.tsx", "src/head.go", "docs/overhead.md", "README.md", "src/hello/main.go", "web/x/h_e_a_d.txt"}
	cases := []struct {
		q    string
		n    int
		want []string
	}{
		// name starts with it, name holds it, letters in order; case aside
		{"head", 10, []string{"src/head.go", "src/components/Header.tsx", "docs/overhead.md", "web/x/h_e_a_d.txt"}},
		{"HEAD", 10, []string{"src/head.go", "src/components/Header.tsx", "docs/overhead.md", "web/x/h_e_a_d.txt"}},
		{"hello/", 10, []string{"src/hello/main.go"}}, // a folder in the path
		{"zzz", 10, []string{}},
		{"", 3, []string{"README.md", "src/head.go", "docs/overhead.md"}}, // shallowest first
		{"head", 1, []string{"src/head.go"}},                              // capped
	}
	for _, c := range cases {
		if got := matchFiles(paths, c.q, c.n); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.q, got, c.want)
		}
	}
}

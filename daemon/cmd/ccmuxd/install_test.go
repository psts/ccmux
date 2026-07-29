package main

import (
	"reflect"
	"testing"
)

func TestParseVer(t *testing.T) {
	cases := []struct {
		in       string
		maj, min int
	}{
		{"3.6b", 3, 6},
		{"3.3", 3, 3},
		{"next-3.7", 3, 7},
		{"2.9", 2, 9},
		{"10.0", 10, 0},
		{"garbage", 0, 0},
	}
	for _, c := range cases {
		if m, n := parseVer(c.in); m != c.maj || n != c.min {
			t.Errorf("parseVer(%q) = %d.%d, want %d.%d", c.in, m, n, c.maj, c.min)
		}
	}
}

func TestTmuxAtLeast(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"tmux 3.6b\n", true},   // newer
		{"tmux 3.3\n", true},    // exactly the floor
		{"tmux 3.2\n", false},   // too old
		{"tmux 2.9\n", false},   // too old
		{"tmux next-3.7", true}, // dev build, newer
		{"weird output", true},  // unparseable → don't block
	}
	for _, c := range cases {
		if ok, _ := tmuxAtLeast(c.out, minTmux); ok != c.want {
			t.Errorf("tmuxAtLeast(%q) = %v, want %v", c.out, ok, c.want)
		}
	}
}

func TestBuildServiceConfig_Args(t *testing.T) {
	self := "/opt/bin/ccmuxd"
	cases := []struct {
		name string
		o    *installOpts
		want []string
	}{
		{
			"host mode",
			&installOpts{Addr: "127.0.0.1:7900", Hostname: "boxb", Tsnet: true},
			[]string{"-addr", "127.0.0.1:7900", "-tsnet", "-tsnet-hostname", "boxb"},
		},
		{
			"hub mode",
			&installOpts{Addr: "127.0.0.1:7900", Hostname: "hub", Tsnet: true, Hub: true},
			[]string{"-addr", "127.0.0.1:7900", "-tsnet", "-tsnet-hostname", "hub", "-hub"},
		},
		{
			"no tsnet",
			&installOpts{Addr: "127.0.0.1:7900", Hostname: "ignored", Tsnet: false},
			[]string{"-addr", "127.0.0.1:7900"},
		},
		{
			"projects root",
			&installOpts{Addr: "127.0.0.1:7900", Hostname: "boxb", Tsnet: true, ProjectsRoot: "/srv/projects"},
			[]string{"-addr", "127.0.0.1:7900", "-tsnet", "-tsnet-hostname", "boxb", "-projects-root", "/srv/projects"},
		},
	}
	for _, c := range cases {
		got := buildServiceConfig(c.o, self)
		if got.BinPath != self {
			t.Errorf("%s: BinPath = %q, want %q", c.name, got.BinPath, self)
		}
		if !reflect.DeepEqual(got.Args, c.want) {
			t.Errorf("%s: Args = %v, want %v", c.name, got.Args, c.want)
		}
	}
}

package listeners

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestPublishedReadsHostPortsOfARun(t *testing.T) {
	cases := []struct {
		cmd  string
		want []int
	}{
		{"docker run --rm -p 8090:80 --name web site", []int{8090}},
		{"docker run -p 127.0.0.1:3000:3000 -p [::1]:3001:3001/tcp site", []int{3000, 3001}},
		{"docker run --publish 5173:5173 --publish=8080:80/udp -p9000:90 site", []int{5173, 8080, 9000}},
		{"docker container run -p 4000:4000 site", []int{4000}},
		{"podman run -p 8000:8000 site", []int{8000}},
		{"docker run -p 80 -P -p 127.0.0.1::80 -p 8000-8002:80-82 site", nil}, // Docker picks, or a range
		{"docker build -p 8090:80 .", nil},                                    // not a run
		{"docker ps", nil},
		{"docker run -p", nil},
		{"", nil},       // no command line at all
		{"docker", nil}, // the client's name and nothing else
	}
	for _, c := range cases {
		if got := published(strings.Fields(c.cmd)); !slices.Equal(got, c.want) { // Fields("") is nil
			t.Errorf("%s: got %v, want %v", c.cmd, got, c.want)
		}
	}
}

// cmdline writes a process's arguments the way /proc does.
func (f *fakeProc) cmdline(pid int, cmd string) {
	must(f.t, os.WriteFile(filepath.Join(f.root, strconv.Itoa(pid), "cmdline"), []byte(strings.ReplaceAll(cmd, " ", "\x00")+"\x00"), 0o644))
}

func TestScan_AContainerAPaneRunsListensForThatPane(t *testing.T) {
	f := newFakeProc(t)
	f.listen(false, "00000000", 8090, 900) // docker-proxy's socket: no pane process holds it
	f.process(40, "docker", []string{EnvKey + "=pane-a"})
	f.cmdline(40, "docker run --rm -p 8090:80 -p 9999:99 site") // 9999 is not listening (yet)
	f.process(41, "docker", nil)                                // a docker outside any pane
	f.cmdline(41, "docker run -p 8090:80 other")
	got, err := f.scanner().Scan()
	if err != nil {
		t.Fatal(err)
	}
	want := []Listener{{Port: 8090, PID: 40, Process: "docker", PaneID: "pane-a"}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestScan_APortThePaneHoldsItselfIsListedOnce(t *testing.T) {
	f := newFakeProc(t)
	f.listen(false, "00000000", 3000, 700)
	f.process(50, "node", []string{EnvKey + "=pane-b"}, 700)
	f.process(51, "docker", []string{EnvKey + "=pane-b"})
	f.cmdline(51, "docker run -p 3000:3000 site")
	got, _ := f.scanner().Scan()
	if len(got) != 1 || got[0].Port != 3000 {
		t.Fatalf("one row per port: %+v", got)
	}
}

// A pane's docker client that exited between two reads (or a zombie) has
// no command line: the scan goes on, and nothing is claimed for it.
func TestScan_ADockerClientWithNoCommandLineIsSkipped(t *testing.T) {
	f := newFakeProc(t)
	f.listen(false, "00000000", 8090, 900)
	f.process(60, "docker", []string{EnvKey + "=pane-c"}) // no cmdline file
	got, err := f.scanner().Scan()
	if err != nil || len(got) != 0 {
		t.Fatalf("nothing claimed, no error: %+v %v", got, err)
	}
}

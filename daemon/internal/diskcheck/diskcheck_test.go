package diskcheck

import (
	"errors"
	"strings"
	"testing"
)

const gb = 1_000_000_000

func TestLowIsUnderTenPercentOrUnderFiveGB(t *testing.T) {
	for _, c := range []struct {
		free, total uint64
		low         bool
		urgency     string
	}{
		{free: 12 * gb, total: 120 * gb, low: false, urgency: "medium"}, // 10% exactly is not under
		{free: 11 * gb, total: 120 * gb, low: true, urgency: "medium"},
		{free: 4 * gb, total: 20 * gb, low: true, urgency: "medium"}, // 20% free, under 5 GB
		{free: 5 * gb, total: 200 * gb, low: true, urgency: "high"},  // 2.5%
		{free: 50 * gb, total: 100 * gb, low: false, urgency: "medium"},
		{free: 0, total: 0, low: false, urgency: "medium"}, // no size, nothing to judge
	} {
		if got := IsLow(c.free, c.total); got != c.low {
			t.Errorf("IsLow(%d, %d) = %v", c.free, c.total, got)
		}
		if got := Urgency(c.free, c.total); c.low && got != c.urgency {
			t.Errorf("Urgency(%d, %d) = %s, want %s", c.free, c.total, got, c.urgency)
		}
	}
}

type filed struct{ prefix, text, urgency string }

// fakeDisks is a Watcher over made-up disks: path → mount, mount → bytes.
func fakeDisks(mounts map[string]string, bytes map[string][2]uint64) (*Watcher, *[]filed) {
	var todos []filed
	w := &Watcher{
		Paths: func() []string { return []string{"/home/u", "/ext/w", "/ext/repo", "/gone"} },
		Mount: func(path string) (string, error) {
			if m, ok := mounts[path]; ok {
				return m, nil
			}
			return "", errors.New("no such folder")
		},
		Statfs: func(mount string) (uint64, uint64, error) { b := bytes[mount]; return b[0], b[1], nil },
		Todo: func(prefix, text, urgency string) error {
			todos = append(todos, filed{prefix, text, urgency})
			return nil
		},
	}
	return w, &todos
}

// Each disk is looked at once, a folder that is gone is skipped, a low
// disk is one warning while it stays low, a more urgent one is filed
// again (to raise the item), and one that recovered and runs low again is
// news again.
func TestALowDiskIsOneWarning(t *testing.T) {
	bytes := map[string][2]uint64{"/": {100 * gb, 200 * gb}, "/ext": {12 * gb, 140 * gb}}
	w, todos := fakeDisks(map[string]string{"/home/u": "/", "/ext/w": "/ext", "/ext/repo": "/ext"}, bytes)
	disks := w.Check()
	if len(disks) != 2 || disks[0].Mount != "/" || disks[0].Low || disks[1].Mount != "/ext" || !disks[1].Low {
		t.Fatalf("disks = %+v", disks)
	}
	if len(*todos) != 1 || (*todos)[0].urgency != "medium" || !strings.HasPrefix((*todos)[0].text, (*todos)[0].prefix) ||
		(*todos)[0].text != "Disk /ext is low on space (92% full, 12.0 GB free)" {
		t.Fatalf("one warning for /ext: %+v", *todos)
	}
	w.Check()
	if len(*todos) != 1 {
		t.Fatalf("the same low disk is not filed every hour: %+v", *todos)
	}
	bytes["/ext"] = [2]uint64{3 * gb, 140 * gb}
	w.Check()
	if len(*todos) != 2 || (*todos)[1].urgency != "high" {
		t.Fatalf("under 5%% it is filed again as high: %+v", *todos)
	}
	bytes["/ext"] = [2]uint64{100 * gb, 140 * gb}
	w.Check()
	bytes["/ext"] = [2]uint64{12 * gb, 140 * gb}
	w.Check()
	if len(*todos) != 3 {
		t.Fatalf("low again after it recovered is news again: %+v", *todos)
	}
	if last := w.Last(); len(last) != 2 || !last[1].Low {
		t.Fatalf("Last is the last look: %+v", last)
	}
}

// A to-do that could not be filed is tried at the next look.
func TestAWarningThatWasNotFiledIsTriedAgain(t *testing.T) {
	w, _ := fakeDisks(map[string]string{"/ext/w": "/ext"}, map[string][2]uint64{"/ext": {1 * gb, 100 * gb}})
	tries := 0
	w.Todo = func(string, string, string) error { tries++; return errors.New("database locked") }
	w.Check()
	w.Check()
	if tries != 2 {
		t.Fatalf("tries = %d, want 2", tries)
	}
}

func TestMountOfARealFolder(t *testing.T) {
	m, err := MountOf(t.TempDir())
	if err != nil || m == "" {
		t.Fatalf("MountOf = %q, %v", m, err)
	}
	free, total, err := Statfs(m)
	if err != nil || total == 0 || free > total {
		t.Fatalf("Statfs(%s) = %d, %d, %v", m, free, total, err)
	}
}

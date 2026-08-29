//go:build !linux

package childproc

import (
	"os/exec"
	"strconv"
	"strings"
)

// count asks ps, the portable way to put the same question where there is no
// /proc (macOS). Output() waits, so the census does not leak a child of its own.
func count(pid int) Counts {
	out, err := exec.Command("ps", "-eo", "ppid=,stat=").Output()
	if err != nil {
		return Counts{}
	}
	res := Counts{Known: true}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if p, err := strconv.Atoi(f[0]); err != nil || p != pid {
			continue
		}
		if strings.HasPrefix(f[1], "Z") {
			res.Defunct++
		} else {
			res.Live++
		}
	}
	return res
}

package childproc

import (
	"strconv"
	"strings"
)

// parsePS reads `ps -o ppid= -o stat=` output and counts pid's children.
//
// Untagged on purpose, so the platform this parser exists for (macOS, which has
// no /proc) can be table-tested from any host. The build-tagged file only runs
// the command. Leaving the parsing inline there is what let a broken ps
// invocation report a confident zero on macOS with nothing able to catch it.
func parsePS(out []byte, pid int) Counts {
	res := Counts{Known: true}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if p, err := strconv.Atoi(f[0]); err != nil || p != pid {
			continue
		}
		// BSD stat is a state letter plus flags ("Z", "Ss", "S+"); only the
		// first character is the state.
		if strings.HasPrefix(f[1], "Z") {
			res.Defunct++
		} else {
			res.Live++
		}
	}
	return res
}

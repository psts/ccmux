//go:build linux

package childproc

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// count walks /proc for processes whose parent is pid. No exec, so taking the
// census cannot add to the thing it is counting.
func count(pid int) Counts {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		// Known:false already tells the lenses to say nothing, so this line is
		// the only way anyone learns the census is off rather than clean.
		log.Printf("childproc: cannot read /proc (%v) — the defunct-child warning is off", err)
		return Counts{}
	}
	var out Counts
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a pid directory
		}
		state, ppid, ok := procStat(e.Name())
		if !ok {
			continue
		}
		// Known is earned, not assumed: setting it before the loop meant a
		// /proc that listed but never resolved reported a confident 0/0 — the
		// false all-clear this package exists to prevent. One readable stat is
		// enough, since it proves /proc answers.
		out.Known = true
		if ppid != pid {
			continue
		}
		if state == "Z" {
			out.Defunct++
		} else {
			out.Live++
		}
	}
	return out
}

// procStat pulls state and ppid out of /proc/<pid>/stat. The comm field is
// parenthesised and may contain spaces AND a ')' — tmux clients are literally
// "(tmux: client)" — so fields are counted from the LAST ')', never by
// splitting the whole line on whitespace.
func procStat(name string) (state string, ppid int, ok bool) {
	b, err := os.ReadFile("/proc/" + name + "/stat")
	if err != nil {
		return "", 0, false // raced a process that exited; not an error here
	}
	return parseStat(string(b))
}

// parseStat is the field-splitting half, kept pure so the awkward part is
// testable without a process that happens to be named right.
func parseStat(line string) (state string, ppid int, ok bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return "", 0, false
	}
	f := strings.Fields(line[i+1:]) // f[0] = state, f[1] = ppid
	if len(f) < 2 {
		return "", 0, false
	}
	p, err := strconv.Atoi(f[1])
	if err != nil {
		return "", 0, false
	}
	return f[0], p, true
}

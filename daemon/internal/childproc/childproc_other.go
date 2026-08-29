//go:build !linux

package childproc

import (
	"log"
	"os/exec"
)

// count asks ps, the portable way to put the same question where there is no
// /proc (macOS). Output() waits, so the census does not leak a child of its own.
//
// One -o per keyword, NOT `-o "ppid=,stat="`. BSD ps treats everything after
// the first '=' as that keyword's replacement HEADER, so the comma-joined form
// parses as a single keyword (ppid) headed ",stat=" and yields one column.
// Every line then fails the two-field check and the census reports a confident
// Known:true, Defunct:0 — the exact false all-clear this package exists to
// prevent, on the platform the Mac lens runs. GNU ps accepts the comma form,
// which is why it looked fine on Linux. -A rather than -e: unambiguous in both
// BSD ps personalities.
func count(pid int) Counts {
	out, err := exec.Command("ps", "-A", "-o", "ppid=", "-o", "stat=").Output()
	if err != nil {
		// Known:false already tells the lenses to say nothing, so this line is
		// the only way anyone learns the census is off rather than clean.
		log.Printf("childproc: ps failed (%v) — the defunct-child warning is off", err)
		return Counts{}
	}
	return parsePS(out, pid)
}

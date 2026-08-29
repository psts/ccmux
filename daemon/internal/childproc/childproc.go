// Package childproc reports this process's own children, split into live and
// defunct (exited but never reaped). It exists so a leaked child shows up in
// the lens instead of only in somebody's ps output: ccmuxd once ran 20 hours
// with 12 defunct tmux clients and nothing anywhere said so.
//
// Read-only by design. Nothing here reaps. A wait4(-1) sweeper would collect
// exit statuses that os/exec is waiting for, turning legitimate Wait calls into
// "no child processes"; reaping stays the job of whoever started the process.
package childproc

import (
	"os"
	"sync"
	"time"
)

// Counts is the census, shaped for JSON in /v1/health. Known is false when the
// platform could not be inspected, so a lens can tell "no zombies" apart from
// "no answer".
type Counts struct {
	Live    int  `json:"live"`
	Defunct int  `json:"defunct"`
	Known   bool `json:"known"`
}

// cacheFor keeps a hub that polls /v1/health from turning this into a hot walk
// of /proc. Short enough that a lens still reflects a leak promptly.
const cacheFor = 2 * time.Second

var (
	mu      sync.Mutex
	cached  Counts
	takenAt time.Time
)

// Count returns the current child census, recomputed at most every cacheFor.
func Count() Counts {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(takenAt) < cacheFor { // zero takenAt is ~2000 years ago, never fresh
		return cached
	}
	cached, takenAt = count(os.Getpid()), time.Now()
	return cached
}

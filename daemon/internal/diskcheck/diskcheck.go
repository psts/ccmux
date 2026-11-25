// Package diskcheck warns before a disk fills: it looks at the free space
// on the disks the daemon's work is on (the worktrees' folder, home, each
// repo) and files a to-do when one runs low. It is cheap (one statfs per
// disk), so the daemon asks once an hour and at every weekly sweep.
package diskcheck

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Low is under 10% free, or under 5 GB free whatever the size; under 5%
// free a warning is urgent.
const (
	lowFreeBytes   = 5_000_000_000
	lowFreePercent = 10
	urgentPercent  = 5
)

// Disk is one disk as the last look found it: GET /v1/health's "disks".
type Disk struct {
	Mount      string `json:"mount"`
	FreeBytes  uint64 `json:"freeBytes"`
	TotalBytes uint64 `json:"totalBytes"`
	Low        bool   `json:"low"`
}

// IsLow says whether free of total is low.
func IsLow(free, total uint64) bool {
	return total > 0 && (free*100 < total*lowFreePercent || free < lowFreeBytes)
}

// Urgency is the to-do's urgency for a low disk.
func Urgency(free, total uint64) string {
	if free*100 < total*urgentPercent {
		return "high"
	}
	return "medium"
}

// Watcher looks at the disks and warns of the low ones.
type Watcher struct {
	// Paths is the folders whose disks are looked at.
	Paths func() []string
	// Mount is the mount point that holds a folder, and Statfs a mount's
	// free and total bytes; nil asks the system.
	Mount  func(path string) (string, error)
	Statfs func(mount string) (free, total uint64, err error)
	// Todo files the warning (prefix names the disk, so a warning seen
	// again is one item whatever its numbers say); nil only logs.
	Todo func(prefix, text, urgency string) error

	mu    sync.Mutex
	last  []Disk
	filed map[string]string // mount → urgency the warning was filed at
}

// Run looks now and then every period until ctx ends.
func (w *Watcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.Check()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Last is what the last look found, by mount.
func (w *Watcher) Last() []Disk {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Disk{}, w.last...)
}

// Check looks at every disk once, warns of each low one, and returns
// what it found. A warning is filed when a disk turns low or grows more
// urgent, not at every look: a person who ticked it off is not asked
// again within the hour.
func (w *Watcher) Check() []Disk {
	w.mu.Lock()
	defer w.mu.Unlock()
	disks := w.measure()
	if w.filed == nil {
		w.filed = map[string]string{}
	}
	for _, d := range disks {
		if !d.Low {
			delete(w.filed, d.Mount)
			continue
		}
		w.warn(d)
	}
	w.last = disks
	return disks
}

// warn logs a low disk and files its to-do when it is news.
func (w *Watcher) warn(d Disk) {
	urgency := Urgency(d.FreeBytes, d.TotalBytes)
	prefix := fmt.Sprintf("Disk %s is low on space (", d.Mount)
	text := fmt.Sprintf("%s%d%% full, %s free)", prefix, percentFull(d), gigabytes(d.FreeBytes))
	log.Printf("disk: %s", text)
	if w.Todo == nil || w.filed[d.Mount] == urgency || w.filed[d.Mount] == "high" {
		return
	}
	if err := w.Todo(prefix, text, urgency); err != nil {
		log.Printf("disk: the to-do for %s was not filed: %v", d.Mount, err)
		return
	}
	w.filed[d.Mount] = urgency
}

// measure is every disk under Paths, once each, by mount.
func (w *Watcher) measure() []Disk {
	seen := map[string]bool{}
	var out []Disk
	for _, path := range w.Paths() {
		mount, err := w.mount(path)
		if err != nil || seen[mount] {
			continue // a folder that is gone has no disk to look at
		}
		seen[mount] = true
		free, total, err := w.statfs(mount)
		if err != nil {
			log.Printf("disk: %s could not be read: %v", mount, err)
			continue
		}
		out = append(out, Disk{Mount: mount, FreeBytes: free, TotalBytes: total, Low: IsLow(free, total)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func (w *Watcher) mount(path string) (string, error) {
	if w.Mount != nil {
		return w.Mount(path)
	}
	return MountOf(path)
}

func (w *Watcher) statfs(mount string) (uint64, uint64, error) {
	if w.Statfs != nil {
		return w.Statfs(mount)
	}
	return Statfs(mount)
}

func percentFull(d Disk) uint64 {
	if d.TotalBytes == 0 {
		return 0
	}
	return 100 - d.FreeBytes*100/d.TotalBytes
}

func gigabytes(n uint64) string { return fmt.Sprintf("%.1f GB", float64(n)/1e9) }

// MountOf is the mount point that holds path: the highest folder above it
// on the same device.
func MountOf(path string) (string, error) {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	dev, err := deviceOf(path)
	if err != nil {
		return "", err
	}
	for {
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		if d, err := deviceOf(parent); err != nil || d != dev {
			return path, nil
		}
		path = parent
	}
}

func deviceOf(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no device number", path)
	}
	return uint64(st.Dev), nil
}

// Statfs is a mount's free bytes (what this user may still write) and its
// size.
func Statfs(mount string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil {
		return 0, 0, err
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), nil
}

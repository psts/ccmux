package taskq

import (
	"path/filepath"
	"sync"
	"time"
)

// A built feature that waits on a repo's own folder (FolderBlock) says why
// in its note, and the runner also keeps what holds it, here, so the task
// page can say since when and ask a person once that has lasted: a lock a
// crashed git left never goes away by itself (seen 2026-11-25, over an hour
// with nobody told). Kept in memory: after a restart a block's clock starts
// again, except a lock's, which is its file's own time.

// MergeBlock is what holds a built feature's merge, as the task page shows
// it.
type MergeBlock struct {
	Kind string `json:"kind"` // BlockLock, BlockElsewhere or BlockUncommitted
	Repo string `json:"repo"` // the repo's folder name
	Lock string `json:"lock,omitempty"`
	// Since is unix ms: the lock file's own time, else when the runner
	// first saw this block.
	Since int64 `json:"since"`
	// GitRunning: a git process works in the repo now; GitUnknown: this
	// host cannot tell (no /proc). Lock blocks only.
	GitRunning bool `json:"gitRunning,omitempty"`
	GitUnknown bool `json:"gitUnknown,omitempty"`
	// NeedsPerson: the block has lasted StaleLockAge. Clearable: it is a
	// lock a person may have removed (ClearStaleLock checks again).
	NeedsPerson bool `json:"needsPerson"`
	Clearable   bool `json:"clearable"`
}

// heldBlock is a block as the runner saw it last.
type heldBlock struct {
	FolderBlock
	since      time.Time
	gitRunning bool
	gitUnknown bool
}

// blockBook is the blocks by task. It has a lock of its own: the list
// route reads it without the runner's.
type blockBook struct {
	mu sync.Mutex
	by map[int64]heldBlock
	// shown is each block as the lenses were last told of it (blocksMoved).
	shown map[int64]MergeBlock
}

// noteBlock keeps what holds feature id's merge (b blocked) or forgets it.
func (r *Runner) noteBlock(id int64, b FolderBlock) {
	r.blocks.mu.Lock()
	defer r.blocks.mu.Unlock()
	if b.Kind == "" {
		delete(r.blocks.by, id)
		return
	}
	if r.blocks.by == nil {
		r.blocks.by = map[int64]heldBlock{}
	}
	h := heldBlock{FolderBlock: b, since: r.Now()}
	if was, ok := r.blocks.by[id]; ok && was.Kind == b.Kind && was.Repo == b.Repo {
		h.since = was.since
	}
	if b.Kind == BlockLock {
		h.since = b.LockTime
		pid, err := GitRunningIn(b.Repo, filepath.Dir(b.Lock))
		h.gitRunning, h.gitUnknown = pid != 0, err != nil
	}
	r.blocks.by[id] = h
}

// ForgetBlock drops what was kept on feature id: a person removed the lock,
// and the next pass looks again.
func (r *Runner) ForgetBlock(id int64) { r.noteBlock(id, FolderBlock{}) }

// keepBlocks drops the blocks of features no longer in line to merge.
func (r *Runner) keepBlocks(waiting map[int64]bool) {
	r.blocks.mu.Lock()
	defer r.blocks.mu.Unlock()
	for id := range r.blocks.by {
		if !waiting[id] {
			delete(r.blocks.by, id)
		}
	}
}

// BlockOf is what holds feature id's merge now, if anything does.
func (r *Runner) BlockOf(id int64) (MergeBlock, bool) {
	r.blocks.mu.Lock()
	defer r.blocks.mu.Unlock()
	h, ok := r.blocks.by[id]
	if !ok {
		return MergeBlock{}, false
	}
	return r.viewOf(h), true
}

// viewOf is a kept block as the page shows it. Under r.blocks.mu.
func (r *Runner) viewOf(h heldBlock) MergeBlock {
	b := MergeBlock{
		Kind: h.Kind, Repo: filepath.Base(h.Repo), Lock: h.Lock, Since: h.since.UnixMilli(),
		GitRunning: h.gitRunning, GitUnknown: h.gitUnknown,
		NeedsPerson: r.Now().Sub(h.since) >= StaleLockAge,
	}
	b.Clearable = b.Kind == BlockLock && b.NeedsPerson && !b.GitRunning && !b.GitUnknown
	return b
}

// blocksMoved reports whether any block reads differently on the page than
// when the lenses were last told: one came or went, or one crossed
// StaleLockAge. No row changes then, so without it a page would never
// learn that a merge started asking for a person.
func (r *Runner) blocksMoved() bool {
	r.blocks.mu.Lock()
	defer r.blocks.mu.Unlock()
	now := make(map[int64]MergeBlock, len(r.blocks.by))
	for id, h := range r.blocks.by {
		now[id] = r.viewOf(h)
	}
	moved := len(now) != len(r.blocks.shown)
	for id, b := range now {
		if was, ok := r.blocks.shown[id]; !ok || was != b {
			moved = true
		}
	}
	r.blocks.shown = now
	return moved
}

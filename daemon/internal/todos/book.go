package todos

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"ccmux.dev/ccmuxd/internal/store"
)

// Store is the slice of the registry the lists need.
type Store interface {
	AddTodo(t store.Todo, norm string) (int64, error)
	Todo(id int64) (*store.Todo, error)
	UnfinishedTodo(repo, norm string) (*store.Todo, error)
	Todos(repo string) ([]store.Todo, error)
	TodosWithTask() ([]store.Todo, error)
	PlanningTodos(repo string) ([]store.Todo, error)
	SaveTodo(t store.Todo, norm string) (bool, error)
	DeleteTodo(id int64) (bool, error)
}

// Book is every repo's to-do list: each change goes through it, one at a
// time, so "is it on the list already?" and the write that follows are one
// step. Changed, when set, is called after every change that was saved.
type Book struct {
	Store   Store
	Now     func() time.Time
	Changed func()

	mu sync.Mutex
}

func (b *Book) now() int64 {
	if b.Now != nil {
		return b.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func (b *Book) changed() {
	if b.Changed != nil {
		b.Changed()
	}
}

// List is a repo's list in the order it is read (Sort); every repo's when
// repo is "".
func (b *Book) List(repo string) ([]store.Todo, error) {
	list, err := b.Store.Todos(repo)
	if err != nil {
		return nil, err
	}
	Sort(list)
	return list, nil
}

// Add puts an item on repo's list and says whether it is new. The same
// thing seen again, while the first sighting is not done, is that item: it
// comes back as it is, its urgency raised when this sighting's is higher,
// and no second row is made.
func (b *Book) Add(repo, text, urgency, by string) (store.Todo, bool, error) {
	return b.add(repo, text, urgency, by, func(norm string) (*store.Todo, error) {
		return b.Store.UnfinishedTodo(repo, norm)
	})
}

// AddOnce is Add for an item that names a thing whose numbers change from
// one sighting to the next ("3 commits, 1.2 GB" this week, "4 commits"
// the next): any unfinished item of repo whose text starts with prefix,
// as written, is that item. prefix should end in a delimiter after the
// name ("worktree /a/foo (") so one name never takes another's
// ("/a/foo-bar").
func (b *Book) AddOnce(repo, prefix, text, urgency, by string) (store.Todo, bool, error) {
	if !strings.HasPrefix(text, prefix) || strings.TrimSpace(prefix) == "" {
		return store.Todo{}, false, problem(Invalid, "an item added once must start with its prefix")
	}
	return b.add(repo, text, urgency, by, func(string) (*store.Todo, error) {
		list, err := b.Store.Todos(repo)
		if err != nil {
			return nil, err
		}
		// Kept text has its spaces folded (CheckText); fold the prefix
		// the same way, or a path with two spaces never finds its item.
		kept := strings.Join(strings.Fields(prefix), " ")
		for _, t := range Unfinished(list) {
			if strings.HasPrefix(t.Text, kept) {
				return &t, nil
			}
		}
		return nil, nil
	})
}

// add files text on repo's list unless same finds it there already. same
// is asked under the book's lock, with the text as compared (Normalize).
func (b *Book) add(repo, text, urgency, by string, same func(norm string) (*store.Todo, error)) (store.Todo, bool, error) {
	text, err := CheckText(text)
	if err != nil {
		return store.Todo{}, false, err
	}
	if urgency, err = CheckUrgency(urgency); err != nil {
		return store.Todo{}, false, err
	}
	if repo == "" {
		return store.Todo{}, false, problem(Invalid, "a to-do item is on a repo's list: name the repo")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	norm := Normalize(text)
	found, err := same(norm)
	if err != nil {
		return store.Todo{}, false, err
	}
	if found != nil {
		return b.seenAgain(*found, urgency)
	}
	now := b.now()
	t := store.Todo{Repo: repo, Text: text, Urgency: urgency, Status: Open, AddedBy: by, CreatedAt: now, UpdatedAt: now}
	if t.ID, err = b.Store.AddTodo(t, norm); err != nil {
		return store.Todo{}, false, err
	}
	b.changed()
	return t, true, nil
}

// seenAgain is Add finding its item on the list: only a higher urgency
// changes it.
func (b *Book) seenAgain(t store.Todo, urgency string) (store.Todo, bool, error) {
	if Rank(urgency) <= Rank(t.Urgency) {
		return t, false, nil
	}
	t.Urgency = urgency
	t, err := b.save(t)
	return t, false, err
}

// Edit is what may change on an item; nil leaves a field as it is. By is
// who is asking: it is kept as who finished an item that turns done.
type Edit struct {
	Text    *string
	Urgency *string
	Status  *string
	Note    *string
	By      string
}

// Change applies e to item id. Nothing is saved when any part of it is
// refused.
func (b *Book) Change(id int64, e Edit) (store.Todo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.item(id)
	if err != nil {
		return store.Todo{}, err
	}
	was := t
	for _, step := range []func(*store.Todo, Edit) error{b.setText, setUrgency, b.setStatus} {
		if err := step(&t, e); err != nil {
			return store.Todo{}, err
		}
	}
	if e.Note != nil {
		t.Note = *e.Note
	}
	if t == was {
		return t, nil
	}
	return b.save(t)
}

// Finish marks item id done by whoever fixed it, with their note on how.
func (b *Book) Finish(id int64, by, note string) (store.Todo, error) {
	done := Done
	e := Edit{Status: &done, By: by}
	if note != "" {
		e.Note = &note
	}
	return b.Change(id, e)
}

// Delete removes item id from its list.
func (b *Book) Delete(id int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	ok, err := b.Store.DeleteTodo(id)
	if err != nil {
		return err
	}
	if !ok {
		return noItem(id)
	}
	b.changed()
	return nil
}

// Get is item id, or the refusal that there is none.
func (b *Book) Get(id int64) (store.Todo, error) { return b.item(id) }

func (b *Book) item(id int64) (store.Todo, error) {
	t, err := b.Store.Todo(id)
	if err != nil {
		return store.Todo{}, err
	}
	if t == nil {
		return store.Todo{}, noItem(id)
	}
	return *t, nil
}

func noItem(id int64) error { return problem(Missing, "no to-do #%d", id) }

// save writes t back, stamped as changed now, and returns it as saved.
func (b *Book) save(t store.Todo) (store.Todo, error) {
	t.UpdatedAt = b.now()
	ok, err := b.Store.SaveTodo(t, Normalize(t.Text))
	if err != nil {
		return store.Todo{}, err
	}
	if !ok {
		return store.Todo{}, noItem(t.ID)
	}
	b.changed()
	return t, nil
}

func (b *Book) setText(t *store.Todo, e Edit) error {
	if e.Text == nil {
		return nil
	}
	text, err := CheckText(*e.Text)
	if err != nil {
		return err
	}
	t.Text = text
	if t.Status == Done {
		return nil // a done item is compared with nothing
	}
	return b.notTwice(*t)
}

// notTwice refuses to leave t unfinished when its repo's list already holds
// the same thing unfinished under another number.
func (b *Book) notTwice(t store.Todo) error {
	same, err := b.Store.UnfinishedTodo(t.Repo, Normalize(t.Text))
	if err != nil {
		return err
	}
	if same != nil && same.ID != t.ID {
		return problem(Conflict, "the same thing is on the list already as #%d", same.ID)
	}
	return nil
}

func setUrgency(t *store.Todo, e Edit) error {
	if e.Urgency == nil {
		return nil
	}
	urgency, err := CheckUrgency(*e.Urgency)
	if err != nil {
		return err
	}
	t.Urgency = urgency
	return nil
}

// setStatus moves an item: done records who finished it and when; open
// takes it back onto the list, with what it had of a task or a finish
// cleared; tasked is an open item handed on to be filed as a task, which
// lets go of any task it had before. Every move takes an item out of its
// repo's planning line; into the line is Plan's alone, since an item is
// only in it once it was sent to the planner.
func (b *Book) setStatus(t *store.Todo, e Edit) error {
	if e.Status == nil || *e.Status == t.Status {
		return nil
	}
	move, ok := moves[*e.Status]
	if !ok {
		return problem(Invalid, "status is open, tasked or done, not %q", *e.Status)
	}
	t.PlanningAt = 0
	return move(b, t, e)
}

// moves is what a change of status does to an item, by the status asked for.
var moves = map[string]func(*Book, *store.Todo, Edit) error{
	Done: func(b *Book, t *store.Todo, e Edit) error {
		t.Status, t.DoneBy, t.DoneAt = Done, e.By, b.now()
		return nil
	},
	Open: func(b *Book, t *store.Todo, _ Edit) error {
		wasDone := t.Status == Done
		t.Status, t.DoneBy, t.DoneAt, t.TaskID = Open, "", 0, 0
		if wasDone {
			return b.notTwice(*t)
		}
		return nil
	},
	Tasked: func(_ *Book, t *store.Todo, _ Edit) error {
		if t.Status == Done {
			return problem(Conflict, "to-do #%d is done; put it back on the list before making it a task", t.ID)
		}
		// Handed on anew: a task it had before (cancelled, since the item
		// was open) and what was noted of that are history.
		t.Status, t.TaskID, t.Note = Tasked, 0, ""
		return nil
	},
	Planning: func(_ *Book, t *store.Todo, _ Edit) error {
		return problem(Invalid, "to-do #%d goes into the planning line with Make task (POST /v1/todos/%d/task), which sends it to the repo's planner; a status change sends nothing", t.ID, t.ID)
	},
}

// Fileable says why item id of repo cannot be filed as a task, or nil. The
// tasks tool asks before it files, so a wrong number refuses the filing
// instead of leaving a task that claims an item it has nothing to do with.
func (b *Book) Fileable(id int64, repo string) error {
	t, err := b.item(id)
	if err != nil {
		return err
	}
	if t.Repo != repo {
		return problem(Missing, "no to-do #%d on this repo's list", id)
	}
	if t.Status == Done {
		return problem(Conflict, "to-do #%d is done already", id)
	}
	return nil
}

// Filed records that item id was filed as task taskID: it is tasked, and
// turns done with that task (Settle).
func (b *Book) Filed(id, taskID int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := b.item(id)
	if err != nil {
		return err
	}
	if t.Status == Done {
		return nil // finished between the check and the filing: it stays done
	}
	// A note on an earlier task is not about this one; filed, it has left
	// the planning line.
	t.Status, t.TaskID, t.Note, t.PlanningAt = Tasked, taskID, "", 0
	_, err = b.save(t)
	return err
}

// What Settle is told of an item's task: on its way (waiting, building,
// merging), merged, cancelled (it may be queued again), or removed. "" is
// a task that could not be read, which changes nothing.
const (
	TaskActive    = "active"
	TaskMerged    = "merged"
	TaskCancelled = "cancelled"
	TaskGone      = "gone"
)

// Settle brings every item filed as a task in line with where that task is
// now, as taskState says it: tasked while the task is on its way, done (by
// that task) once it is merged, open again while it is cancelled or when
// it was removed. It reads the list afresh each time and changes only what
// differs, so calling it again, or late, costs nothing.
func (b *Book) Settle(taskState func(taskID int64) string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	list, err := b.Store.TodosWithTask()
	if err != nil {
		return err
	}
	for _, t := range list {
		next := settled(t, taskState(t.TaskID), b.now())
		if next == t {
			continue
		}
		if _, err := b.save(next); err != nil {
			return err
		}
	}
	return nil
}

// settled is item t as its task's state leaves it. A cancelled task keeps
// its item's link: queued again, it takes the item with it.
func settled(t store.Todo, state string, now int64) store.Todo {
	task := fmt.Sprintf("task #%d", t.TaskID)
	switch state {
	case TaskActive:
		if t.Status != Tasked {
			t.Status, t.Note = Tasked, ""
		}
	case TaskMerged:
		t.Status, t.DoneBy, t.DoneAt, t.Note = Done, task, now, ""
	case TaskCancelled:
		if t.Status != Open {
			t.Status, t.Note = Open, task+" was cancelled before it was built"
		}
	case TaskGone:
		t.Status, t.TaskID, t.Note = Open, 0, task+" was removed before it was built"
	}
	return t
}

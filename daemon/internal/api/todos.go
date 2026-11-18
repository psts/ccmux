package api

// The per-repo to-do lists' lens routes (internal/todos has the rules). A
// list holds what a session noticed and did not handle right away; agents
// add to it through the peers `todo` tool, and the thread page shows each
// repo's list and lets a person add, re-rank, tick off and delete. Like the
// task queue, the lists live on the daemon the lenses talk to, so none of
// this is proxied.
//
// A repo is named here by a folder: the repo's own, a worktree of it, or
// any folder inside either. All of them are one list (todos.Keys).

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

// SetTodos wires the to-do lists to their store. Every change tells the
// lenses (todos-changed on the events feed).
func (s *Server) SetTodos(st todos.Store) {
	s.todos = &todos.Book{Store: st, Changed: s.todosChanged}
	s.todoKeys = &todos.Keys{}
}

func (s *Server) todosChanged() {
	if s.mgr != nil {
		s.mgr.TodosChanged()
	}
}

func (s *Server) todoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/todos", s.listTodos)
	mux.HandleFunc("POST /v1/todos", s.addTodo)
	mux.HandleFunc("PATCH /v1/todos/{id}", s.patchTodo)
	mux.HandleFunc("DELETE /v1/todos/{id}", s.deleteTodo)
	mux.HandleFunc("POST /v1/todos/{id}/task", s.makeTodoTask)
	mux.HandleFunc("POST /v1/panes/{id}/todo", s.paneTodo)
}

// todosReady answers 503 when this daemon has no to-do store.
func (s *Server) todosReady(w http.ResponseWriter) bool {
	if s.todos == nil {
		writeError(w, http.StatusServiceUnavailable, "the to-do lists are not available on this daemon")
		return false
	}
	return true
}

// writeTodoError answers a change the rules refused with their own words,
// and anything else as the daemon's fault.
func writeTodoError(w http.ResponseWriter, err error) {
	writeError(w, todoStatus(err), err.Error())
}

var todoStatuses = map[int]int{todos.Invalid: http.StatusBadRequest, todos.Missing: http.StatusNotFound, todos.Conflict: http.StatusConflict}

func todoStatus(err error) int {
	var p *todos.Problem
	if errors.As(err, &p) {
		return todoStatuses[p.Kind]
	}
	return http.StatusInternalServerError
}

// listTodos: GET /v1/todos[?repo=<folder>] → {todos, repos}. todos is one
// repo's list, or every repo's with none named, in the order it is read
// (todos.Sort). repos maps each repo session on this daemon to its list's
// key, so a page files each item under the right row without comparing
// paths itself: a session opened on a worktree or a subfolder shows its
// repo's list.
func (s *Server) listTodos(w http.ResponseWriter, r *http.Request) {
	if !s.todosReady(w) {
		return
	}
	key, err := s.todoKeys.Of(r.URL.Query().Get("repo"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	list, err := s.todos.List(key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"todos": list, "repos": s.todoRepos()})
}

// todoRepos is each repo session's list key, by workspace id. A role
// agent's folder is no repo and has no list. A session whose repo git
// could not name is left out and logged: its row shows no list, which is
// true, where a guessed key would show an empty one.
func (s *Server) todoRepos() map[string]string {
	out := map[string]string{}
	for _, ws := range s.mgr.List() {
		if ws.Agent != "" || ws.RepoPath == "" {
			continue
		}
		key, err := s.todoKeys.Of(ws.RepoPath)
		if err != nil {
			log.Printf("todos: session %s has no to-do list on the page: %v", ws.Name, err)
			continue
		}
		out[ws.ID] = key
	}
	return out
}

// addTodo: POST /v1/todos {repo, text, urgency, by} → {todo, added}. The
// same thing as an unfinished item of the repo answers that item with
// added false (200), never a second row; a new one is 201.
func (s *Server) addTodo(w http.ResponseWriter, r *http.Request) {
	if !s.todosReady(w) {
		return
	}
	var in struct{ Repo, Text, Urgency, By string }
	if !decodeJSON(w, r, &in) {
		return
	}
	key, err := s.todoKeys.Of(in.Repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	item, added, err := s.todos.Add(key, in.Text, in.Urgency, in.By)
	if err != nil {
		writeTodoError(w, err)
		return
	}
	status := http.StatusOK
	if added {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"todo": item, "added": added})
}

// todoID reads {id}, or writes the error and returns false.
func todoID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad to-do id")
		return 0, false
	}
	return id, true
}

// patchTodo: PATCH /v1/todos/{id} with any of {status, urgency, text,
// note}, and by: who is asking, kept as who finished an item that turns
// done. Status is open, tasked or done.
func (s *Server) patchTodo(w http.ResponseWriter, r *http.Request) {
	if !s.todosReady(w) {
		return
	}
	var in struct {
		Status, Urgency, Text, Note *string
		By                          string
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	id, ok := todoID(w, r)
	if !ok {
		return
	}
	item, err := s.todos.Change(id, todos.Edit{Text: in.Text, Urgency: in.Urgency, Status: in.Status, Note: in.Note, By: in.By})
	if err != nil {
		writeTodoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]store.Todo{"todo": item})
}

// deleteTodo: DELETE /v1/todos/{id}.
func (s *Server) deleteTodo(w http.ResponseWriter, r *http.Request) {
	if !s.todosReady(w) {
		return
	}
	id, ok := todoID(w, r)
	if !ok {
		return
	}
	if err := s.todos.Delete(id); err != nil {
		writeTodoError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

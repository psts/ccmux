package api

// The peers shim's `todo` tool: a session adds what it noticed and is not
// handling now to a repo's to-do list, reads the list, and ticks off what
// it fixed. The repo is the pane's own session's unless the call names
// another. The pane is in the repo's own workspace even when its Claude
// works in a worktree of it (a task builder, a thread's helper), and the
// list is named by the repo's main folder either way (todos.Keys).

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/todos"
)

type paneTodoReq struct {
	Action  string `json:"action"`
	Text    string `json:"text"`
	Urgency string `json:"urgency"`
	// Repo names another repo's list: a repo session in the caller's window
	// by its folder name, or any repo by a folder of it.
	Repo string `json:"repo"`
	ID   int64  `json:"id"`
	Note string `json:"note"`
	// By is the caller's name on the bus, which the shim knows; a caller
	// that sends none is named after its session.
	By string `json:"by"`
}

// todoCaller is who is asking and about which list: the list's key, the
// repo's short name for the answer, and the name stamped on what it adds
// or finishes.
type todoCaller struct{ repo, name, by string }

// paneTodo: POST /v1/panes/{id}/todo {action, ...} → {message, repo, todos}:
// what the action did in one line, and for a list the repo's unfinished
// items in the order they are read. Same access as the tasks tool's route.
func (s *Server) paneTodo(w http.ResponseWriter, r *http.Request) {
	if !s.todosReady(w) {
		return
	}
	var req paneTodoReq
	if !decodeJSON(w, r, &req) {
		return
	}
	who, status, err := s.todoCallerOfPane(r.PathValue("id"), req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	act, ok := paneTodoActions[req.Action]
	if !ok {
		writeError(w, http.StatusBadRequest, "action must be one of add, list, done")
		return
	}
	message, list, err := act(s, who, req)
	if err != nil {
		writeTodoError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": message, "repo": who.name, "todos": list})
}

// What each action of the tool does, by its name.
var paneTodoActions = map[string]func(*Server, todoCaller, paneTodoReq) (string, []store.Todo, error){
	"add":  (*Server).paneTodoAdd,
	"list": (*Server).paneTodoList,
	"done": (*Server).paneTodoDone,
}

// todoCallerOfPane works out whose list a call is about. A session on
// another host is unknown here: the lists hold only this daemon's repos.
func (s *Server) todoCallerOfPane(paneID string, req paneTodoReq) (todoCaller, int, error) {
	ws := s.mgr.Workspace(s.mgr.WorkspaceForPane(paneID))
	if ws == nil {
		return todoCaller{}, http.StatusNotFound, errors.New("unknown pane: the to-do lists only take items from sessions hosted on this daemon")
	}
	by := strings.TrimSpace(req.By)
	if by == "" {
		by = ws.Name
	}
	folder := ws.RepoPath
	if name := strings.TrimSpace(req.Repo); name != "" {
		named, err := s.todoRepoNamed(paneID, name)
		if err != nil {
			return todoCaller{}, http.StatusNotFound, err
		}
		folder = named
	} else if ws.Agent != "" {
		return todoCaller{}, http.StatusBadRequest, errors.New("a role agent has no repo of its own: name the repo the item is about with repo")
	}
	key, err := s.todoKeys.Of(folder)
	if err != nil {
		return todoCaller{}, http.StatusInternalServerError, err
	}
	return todoCaller{repo: key, name: filepath.Base(key), by: by}, 0, nil
}

// todoRepoNamed is the folder a call's repo names: itself when it is a
// path, else the repo session of that folder name in the caller's window.
func (s *Server) todoRepoNamed(paneID, name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	folder, err := repoNamed(s.mgr.WindowPeers(paneID), name)
	if err != nil {
		return "", fmt.Errorf("%w (repo is the folder name of a repo in this session's ccmux window, or a repo's full path)", err)
	}
	return folder, nil
}

func (s *Server) paneTodoAdd(who todoCaller, req paneTodoReq) (string, []store.Todo, error) {
	item, added, err := s.todos.Add(who.repo, req.Text, req.Urgency, who.by)
	if err != nil {
		return "", nil, err
	}
	if !added {
		return fmt.Sprintf("Already on %s's to-do list as #%d [%s, %s]; not added again.", who.name, item.ID, item.Urgency, item.Status), nil, nil
	}
	return fmt.Sprintf("Added to-do #%d [%s] to %s's list.", item.ID, item.Urgency, who.name), nil, nil
}

func (s *Server) paneTodoList(who todoCaller, _ paneTodoReq) (string, []store.Todo, error) {
	list, err := s.todos.List(who.repo)
	if err != nil {
		return "", nil, err
	}
	return "", todos.Unfinished(list), nil
}

// paneTodoDone ticks off an item of the caller's list (or the one repo
// names). An item of any other list is not found: a mistyped number must
// not finish work the session knows nothing of.
func (s *Server) paneTodoDone(who todoCaller, req paneTodoReq) (string, []store.Todo, error) {
	item, err := s.todos.Get(req.ID)
	if err != nil {
		return "", nil, err
	}
	if item.Repo != who.repo {
		return "", nil, &todos.Problem{Kind: todos.Missing, Msg: fmt.Sprintf("no to-do #%d on %s's list", req.ID, who.name)}
	}
	if item.Status == todos.Done {
		return fmt.Sprintf("To-do #%d was done already.", item.ID), nil, nil
	}
	if _, err := s.todos.Finish(item.ID, who.by, strings.TrimSpace(req.Note)); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Marked to-do #%d done.", item.ID), nil, nil
}

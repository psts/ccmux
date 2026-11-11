package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"ccmux.dev/ccmuxd/internal/model"
	"ccmux.dev/ccmuxd/internal/store"
	"ccmux.dev/ccmuxd/internal/taskq"
)

// workerPart is the part the caller's pane was started to build, if any.
func (s *Server) workerPart(who taskFiler) (store.Part, bool, error) {
	parts, err := s.tasks.Parts()
	if err != nil {
		return store.Part{}, false, err
	}
	for _, p := range parts {
		if who.pane != "" && p.PaneID == who.pane {
			return p, true, nil
		}
	}
	return store.Part{}, false, nil
}

// addPart is add_part, a worker's alone: a part of its own running task for
// another repo (repo), which starts at once. A session files every part
// with the feature instead.
func (s *Server) addPart(who taskFiler, req paneTaskReq) (string, int, error) {
	mine, isWorker, err := s.workerPart(who)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if !isWorker {
		return "", http.StatusConflict, errors.New("add_part is for a Claude building a part; file a feature with every repo's part (file with parts), or cancel a filed one and file it again with the repo added")
	}
	return s.addPartFromWorker(who, mine, req)
}

// addPartFromWorker adds a part for another repo to the worker's own,
// running task. The runner starts it on its next pass.
func (s *Server) addPartFromWorker(who taskFiler, mine store.Part, req paneTaskReq) (string, int, error) {
	repo, err := s.repoByName(who, req.Repo)
	if err != nil {
		return "", http.StatusBadRequest, err
	}
	return s.joinPart(who, mine, repo, req.Plan)
}

// joinPart adds a part in repo to the worker's task.
func (s *Server) joinPart(who taskFiler, mine store.Part, repo, plan string) (string, int, error) {
	t, err := s.tasks.Task(mine.TaskID)
	if err != nil || t == nil {
		return "", http.StatusInternalServerError, fmt.Errorf("task #%d unreadable: %v", mine.TaskID, err)
	}
	if t.Status != taskq.Running && t.Status != taskq.NeedsYou {
		return "", http.StatusConflict, fmt.Errorf("task #%d is %s, so no part can join it now", t.ID, t.Status)
	}
	parts, err := s.tasks.Parts()
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	for _, p := range taskq.PartsOf(parts, t.ID) {
		if p.RepoPath == repo {
			return "", http.StatusConflict, fmt.Errorf("%s already has a part in this task; message %s on the bus", filepath.Base(repo), taskq.BusName(repo, t.ID))
		}
	}
	name := taskq.BusName(repo, t.ID)
	return s.newPart(*t, repo, plan, taskq.Pending, taskq.BusName(who.repo, t.ID),
		fmt.Sprintf("Added a part in %s. A Claude starts on it within a minute as %s; tell it what you need from it.", filepath.Base(repo), name))
}

func (s *Server) newPart(t store.Task, repo, plan, status, by, msg string) (string, int, error) {
	if err := taskq.CheckPlan(plan); err != nil {
		return "", http.StatusBadRequest, err
	}
	now := s.taskNow()
	if _, err := s.tasks.AddPart(store.Part{TaskID: t.ID, RepoPath: repo, Plan: plan, Status: status,
		FiledBy: by, CreatedAt: now, UpdatedAt: now}); err != nil {
		return "", http.StatusInternalServerError, err
	}
	s.mgr.TasksChanged()
	return msg, 0, nil
}

// repoByName finds the repo a worker names (its folder name) among the
// sessions sharing its ccmux window. Not any repo on the daemon: a worker
// runs unattended in auto mode, and one misled by text it read must not be
// able to start work in a repo outside the project it was given.
func (s *Server) repoByName(who taskFiler, name string) (string, error) {
	repo, err := repoNamed(s.mgr.WindowPeers(who.pane), name)
	if err != nil {
		return "", fmt.Errorf("%w (a worker can add parts only in repos in its own ccmux window)", err)
	}
	return repo, nil
}

// repoNamed picks the repo session whose folder is called name: a git repo,
// never an agent's session.
func repoNamed(list []*model.Workspace, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name the repo (its folder name) the new part is for")
	}
	for _, ws := range list {
		if ws.Agent != "" || filepath.Base(ws.RepoPath) != name {
			continue
		}
		if _, err := os.Stat(filepath.Join(ws.RepoPath, ".git")); err == nil {
			return ws.RepoPath, nil
		}
	}
	return "", fmt.Errorf("no repo session named %s on this daemon", name)
}

// reportPart takes a worker's report on the part its pane was started for.
// The pane is the identity: a session cannot report on work it is not doing.
func (s *Server) reportPart(who taskFiler, req paneTaskReq) (string, int, error) {
	mine, isWorker, err := s.workerPart(who)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if !isWorker {
		return "", http.StatusNotFound, errors.New("this session is not building a part of a queued task, so there is nothing to report on")
	}
	next, err := taskq.Report(mine, req.Status, strings.TrimSpace(req.Note), s.taskNow())
	if err != nil {
		return "", http.StatusConflict, err
	}
	if err := s.savePart(next); err != nil {
		return "", http.StatusInternalServerError, err
	}
	return reportReply(mine.TaskID, next.Status), 0, nil
}

// reportReply is what a worker reads back after a report. A built part is
// about to be checked where it stands, so its worker is told to stop there:
// one that went on to run the repo's checks again failed its own merge.
func reportReply(taskID int64, status string) string {
	if status == taskq.Ready {
		return fmt.Sprintf("Reported your part of task #%d as %s. Stop now and run nothing more in this folder: ccmux runs the repo's checks here once your turn has ended.", taskID, status)
	}
	return fmt.Sprintf("Reported your part of task #%d as %s. You can stop now.", taskID, status)
}

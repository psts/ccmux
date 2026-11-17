// threadteam.js: what the thread page says about a thread's team. A thread
// with helpers is one piece of work with its own sessions: the one that
// leads it, in its repo, and a helper per other repo it needs. The daemon
// serves every session with its thread, its role and where it works
// (/v1/sessions), the thread's own row (/v1/threads), and for one thread
// each member's changes (/v1/threads/{id}); this turns them into the
// sidebar's line under a thread, the header's line, the Helpers tab and the
// changes by repo.
//
// Plain functions over plain data, so threadteam_test.js runs them without
// a browser. It builds on threadmodel.js (where a session is, a repo's name).
(function (root) {
  "use strict";
  const Model = typeof module !== "undefined" && module.exports ? require("./threadmodel.js") : root.ccmuxThreadModel;

  const isHelper = (s) => !!s && s.role === "helper";

  // membersOf is the sessions of session's thread that have not ended, the
  // lead first; just the session itself when it is in no thread.
  function membersOf(session, sessions) {
    if (!session || !session.threadId) return session ? [session] : [];
    const mine = (sessions || []).filter((s) => s.threadId === session.threadId && s.state !== "done");
    return mine.sort((a, b) => (a.role === "lead" ? 0 : 1) - (b.role === "lead" ? 0 : 1) || a.createdAt - b.createdAt);
  }

  // repoOf is the name of the repo a member works for: its repo session's
  // name, else its repo folder's.
  function repoOf(session, workspaces) {
    const ws = (workspaces || []).find((w) => w.id === session.workspaceId);
    if (ws) return Model.repoName(ws);
    return String(session.repo || session.cwd || "").split("/").filter(Boolean).pop() || "repo";
  }

  // teamState is where a thread is, as one word: it needs you when any
  // member does, it runs while any member runs, and otherwise it is where
  // its lead is.
  function teamState(lead, sessions) {
    const states = membersOf(lead, sessions).map(Model.stateOf);
    if (states.includes("waiting")) return "waiting";
    const own = Model.stateOf(lead);
    if (own !== "ended" && own !== "unknown" && states.includes("running")) return "running";
    return own;
  }

  const words = { waiting: "needs you", running: "running", done: "done", idle: "idle", asleep: "asleep", ended: "ended", unknown: "unknown" };

  // subLine is the line under a thread in the sidebar: who it works with,
  // then where it stands ("with backend, admin · needs you"). thread is its
  // row from the daemon, when the page has it: a merge shows there.
  function subLine(lead, sessions, workspaces, thread) {
    const helpers = membersOf(lead, sessions).filter(isHelper).map((s) => repoOf(s, workspaces));
    const state = teamState(lead, sessions);
    let stands = words[state] || state;
    if (thread && thread.state === "merging") stands = "merging";
    else if (thread && thread.state === "merged" && state !== "waiting" && state !== "running") stands = "merged";
    return (helpers.length ? "with " + helpers.join(", ") + " · " : "") + stands;
  }

  // decorate gives each thread row of the sidebar's tree (threadmodel.tree)
  // its team's state and line.
  function decorate(tree, sessions, workspaces, threads) {
    const rows = new Map((threads || []).map((t) => [t.id, t]));
    for (const group of tree) for (const repo of group.repos) for (const t of repo.threads) {
      t.state = teamState(t.session, sessions);
      t.sub = subLine(t.session, sessions, workspaces, rows.get(t.session.threadId));
    }
    return tree;
  }

  const doing = { waiting: "Waiting for your answer.", running: "Working.", done: "Finished its turn.", idle: "Idle.", asleep: "Asleep. A message wakes it.", ended: "Ended.", unknown: "State unknown." };

  // helperRows is the Helpers tab: every member of the picked session's
  // thread with its role, what it is doing (the goal it noted, else where
  // it is) and where it works: its repo's own folder, reading, or its
  // worktree on the thread's branch.
  function helperRows(session, sessions, workspaces) {
    if (!session || !session.threadId) return [];
    return membersOf(session, sessions).map((s) => {
      const state = Model.stateOf(s);
      return {
        id: s.id, name: repoOf(s, workspaces), bus: s.name || "", role: s.role || "helper", state,
        doing: s.goal || doing[state] || "",
        where: s.worktree ? "worktree · " + (s.branch || "") : "repo folder · read only",
      };
    });
  }

  // metaLine is the header's line for a thread member: its window, who
  // leads, and who helps.
  function metaLine(session, sessions, workspaces, group) {
    const members = membersOf(session, sessions);
    const lead = members.find((s) => s.role === "lead");
    const helpers = members.filter(isHelper).map((s) => repoOf(s, workspaces));
    const leadRepo = lead ? repoOf(lead, workspaces) : "?";
    if (isHelper(session)) return `${group} · helper in ${repoOf(session, workspaces)} · lead: ${leadRepo}`;
    const team = helpers.length ? ` · helper${helpers.length === 1 ? "" : "s"}: ${helpers.join(", ")}` : "";
    return `${group} · lead: ${leadRepo}${team}`;
  }

  // changeGroups is the Changes tab by repo: each member's branch, the one
  // line on what it holds, and its files with lines added and removed.
  function changeGroups(detail) {
    return ((detail && detail.members) || []).map((m) => ({
      repo: m.repo, role: m.session.role || "", branch: m.session.branch || "no branch yet", status: m.status || "",
      files: ((m.changes && m.changes.files) || []).map((f) => ({ path: f.path, added: f.added || 0, removed: f.removed || 0 })),
    }));
  }

  const api = { isHelper, membersOf, repoOf, teamState, subLine, decorate, helperRows, metaLine, changeGroups };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadTeam = api;
})(typeof window !== "undefined" ? window : this);

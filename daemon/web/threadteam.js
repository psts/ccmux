// threadteam.js: what the thread page says about a thread's team. A thread
// with helpers is one piece of work with its own sessions: the one that
// leads it, in its repo, and a helper per other repo it needs. The daemon
// serves every session with its thread, its role and where it works
// (/v1/sessions), the thread's own row (/v1/threads), and one thread whole
// (/v1/threads/{id}); this turns them into the sidebar's line under a
// thread, the header's line, the Helpers tab and the merge bar.
//
// Plain functions over plain data, so threadteam_test.js runs them without
// a browser. It builds on threadmodel.js (where a session is, a repo's name).
(function (root) {
  "use strict";
  const Model = typeof module !== "undefined" && module.exports ? require("./threadmodel.js") : root.ccmuxThreadModel;

  const isHelper = (s) => !!s && s.role === "helper";

  // leadsSide says session leads a side thread of its own: a repo's main
  // session, or its planner (the session that turns the repo's to-do items
  // into tasks). Both are in no thread themselves.
  const leadsSide = (s) => !!s && !s.threadId && (s.kind === "lead" || s.kind === "planner");

  // sideOf is a main session's or a planner's side thread: the helpers it
  // asked for when it named another repo (the daemon's threads/side.go, a
  // thread row of kind "side" led by that session, which is itself in no
  // thread). null for any other session.
  function sideOf(session, threads) {
    if (!leadsSide(session)) return null;
    return (threads || []).find((t) => t.kind === "side" && t.leadSession === session.id) || null;
  }

  // teamId is the thread whose team session is in: its own, or a main
  // session's side thread; 0 for none.
  function teamId(session, threads) {
    if (!session) return 0;
    if (session.threadId) return session.threadId;
    const side = sideOf(session, threads);
    return side ? side.id : 0;
  }

  // leads says session leads its team: a thread's lead, a main session, or
  // a planner.
  const leads = (s) => s.role === "lead" || leadsSide(s);

  // membersOf is the sessions of session's team that have not ended, the
  // lead first; just the session itself when it has none. A side thread's
  // lead is the main session, which carries no thread id of its own.
  function membersOf(session, sessions, threads) {
    const id = teamId(session, threads);
    if (!id) return session ? [session] : [];
    const row = (threads || []).find((t) => t.id === id);
    const main = row && row.kind === "side" ? row.leadSession : "";
    const mine = (sessions || []).filter((s) => (s.threadId === id || (main && s.id === main)) && s.state !== "done");
    const rank = (s) => (leads(s) ? 0 : 1);
    return mine.sort((a, b) => rank(a) - rank(b) || a.createdAt - b.createdAt);
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
  function teamState(lead, sessions, threads) {
    const states = membersOf(lead, sessions, threads).map(Model.stateOf);
    if (states.includes("waiting")) return "waiting";
    const own = Model.stateOf(lead);
    if (own !== "ended" && own !== "unknown" && states.includes("running")) return "running";
    return own;
  }

  const words = { waiting: "needs you", running: "running", done: "done", idle: "idle", asleep: "asleep", ended: "ended", unknown: "unknown", stuck: "could not start" };

  // subLine is the line under a thread in the sidebar: who it works with,
  // then where it stands ("with backend, admin · needs you"). thread is its
  // row from the daemon, when the page has it: a merge shows there.
  function subLine(lead, sessions, workspaces, thread, threads) {
    const helpers = membersOf(lead, sessions, threads).filter(isHelper).map((s) => repoOf(s, workspaces));
    const state = teamState(lead, sessions, threads);
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
      t.state = teamState(t.session, sessions, threads);
      t.sub = subLine(t.session, sessions, workspaces, rows.get(t.session.threadId), threads);
    }
    return tree;
  }

  const doing = { waiting: "Waiting for your answer.", running: "Working.", done: "Finished its turn.", idle: "Idle.", asleep: "Asleep. A message wakes it.", ended: "Ended.", unknown: "State unknown." };

  // helperRows is the Helpers tab: every member of the picked session's
  // team with its role, what it is doing (the goal it noted, else where it
  // is) and where it works: its repo's own folder, reading, or its worktree
  // on the thread's branch, or for a main session's helper its scratch
  // copy, which is never merged.
  function helperRows(session, sessions, workspaces, threads) {
    const id = teamId(session, threads);
    if (!id) return [];
    const row = (threads || []).find((t) => t.id === id);
    const side = !!row && row.kind === "side";
    return membersOf(session, sessions, threads).map((s) => {
      const state = Model.stateOf(s);
      return {
        id: s.id, name: repoOf(s, workspaces), bus: s.name || "", role: leads(s) ? "lead" : s.role || "helper", state,
        doing: s.goal || doing[state] || "",
        where: whereOf(s, side),
      };
    });
  }

  // whereOf is where one member of a team works, in a few words.
  function whereOf(s, side) {
    if (!s.threadId) return s.kind === "planner" ? "repo folder · planner" : "repo folder · main session";
    if (!s.worktree) return "repo folder · read only";
    return side ? "scratch copy · never merged" : "worktree · " + (s.branch || "");
  }

  const isBuilder = (s) => !!s && s.role === "builder";

  // builderLine is the header's line for a queued task's builder: the task
  // queue leads its thread, so there is no lead to name, only the repo it
  // builds in and the repos its teammates build in.
  function builderLine(session, members, workspaces, group) {
    const mates = members.filter((s) => s.id !== session.id).map((s) => repoOf(s, workspaces));
    return `${group} · task builder in ${repoOf(session, workspaces)}` + (mates.length ? ` · with: ${mates.join(", ")}` : "");
  }

  // metaLine is the header's line for a thread member: its window, who
  // leads, and who helps.
  function metaLine(session, sessions, workspaces, group, threads) {
    const members = membersOf(session, sessions, threads);
    if (isBuilder(session)) return builderLine(session, members, workspaces, group);
    const lead = members.find(leads);
    const helpers = members.filter(isHelper).map((s) => repoOf(s, workspaces));
    const leadRepo = lead ? repoOf(lead, workspaces) + (lead.kind === "planner" ? "'s planner" : "") : "?";
    if (isHelper(session)) return `${group} · helper in ${repoOf(session, workspaces)} · lead: ${leadRepo}`;
    const team = helpers.length ? ` · helper${helpers.length === 1 ? "" : "s"}: ${helpers.join(", ")}` : "";
    return `${group} · lead: ${leadRepo}${team}`;
  }

  // elapsed is how long something has run, as a person reads it: "45s",
  // "3m 05s", "1h 02m".
  function elapsed(ms) {
    const s = Math.max(0, Math.floor(ms / 1000));
    const two = (n) => String(n).padStart(2, "0");
    if (s < 60) return s + "s";
    if (s < 3600) return `${Math.floor(s / 60)}m ${two(s % 60)}s`;
    return `${Math.floor(s / 3600)}h ${two(Math.floor((s % 3600) / 60))}m`;
  }

  const ended = {
    failed: (m) => "The last merge failed." + (m.checkLog ? " The failed check's whole output is in " + m.checkLog : ""),
    lost: (m) => "The last merge did not finish. Its log: journalctl --user -u " + m.unit,
  };

  // mergeLine is the bar about a thread's merge job (the daemon's
  // threads.MergeView), null for nothing to say. A merge runs in a service
  // of its own, for minutes: while it does, the bar says which step it is in
  // and for how long, and that it was started again when it was cut off.
  // One that did not land says where to read why. A merge that was stopped
  // on purpose says nothing: whoever stopped it knows.
  function mergeLine(merge, now) {
    if (!merge) return null;
    if (merge.state === "stopping") return { running: true, stopping: true, text: "Stopping the merge…" };
    if (merge.state === "running") {
      const parts = ["Merging", merge.doing || "between steps", elapsed(now - merge.startedAt)];
      if (merge.attempts > 1) parts.push(`started again after it was cut off (try ${merge.attempts})`);
      return { running: true, stopping: false, text: parts.join(" · ") };
    }
    const say = ended[merge.state];
    return say ? { running: false, stopping: false, text: say(merge) } : null;
  }

  const api = { isHelper, leadsSide, sideOf, teamId, membersOf, repoOf, teamState, subLine, decorate, helperRows, metaLine, elapsed, mergeLine };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadTeam = api;
})(typeof window !== "undefined" ? window : this);

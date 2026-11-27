// threadmodel.js: what the thread page's sidebar and header say, worked out
// from what the daemon serves: the windows, their repos (workspaces), the
// runner sessions filed under each repo, and where each session is.
//
// A repo's own session is its lead (kind "lead"); every other session of the
// repo is a thread under it. Plain functions over plain data, so
// threadmodel_test.js runs them without a browser; threads.js owns the DOM.
(function (root) {
  "use strict";

  // Flash is the terminal lens's rule for how long a green lasts (flash.js).
  const Flash = typeof module !== "undefined" && module.exports ? require("./flash.js") : root.ccmuxFlash;

  // stateOf is where a session is, as one word the page colours and labels:
  // running (a turn is going), waiting (it asked something), done (a turn
  // finished and nobody has looked), idle (up, nothing to say), asleep,
  // ended, unknown (the daemon could not say; never guessed), or stuck
  // (asleep because the daemon gave up starting it: store/stuck.go; its
  // note says why, and it tries again by itself while it has work).
  function stateOf(session) {
    if (!session) return "asleep";
    if (session.state === "asleep" && session.stuck) return "stuck";
    const where = { done: "ended", asleep: "asleep", unknown: "unknown" }[session.state];
    if (where) return where;
    return { needs_input: "waiting", running: "running", done: "done" }[session.attention] || "idle";
  }

  const labels = { running: "Running", waiting: "Needs you", done: "Done", idle: "Idle", asleep: "Asleep", ended: "Ended", unknown: "Unknown", stuck: "Could not start" };
  const stateLabel = (state) => labels[state] || state;

  // ab is "↑2 ↓1" for a branch ahead and behind, "" when it is neither.
  function ab(ahead, behind) {
    return [ahead ? "↑" + ahead : "", behind ? "↓" + behind : ""].filter(Boolean).join(" ");
  }

  // shipOf is how many commits the branch has that its trunk ref (the
  // daemon's defaultRef: origin/main, or local main with no remote) has not,
  // which is what a ship would send. On the trunk itself that is its own
  // ahead count, when it tracks that ref.
  function shipOf(git, offDefault, ref) {
    if (offDefault) return git.aheadOfDefault || 0;
    return git.trackingBranch && git.trackingBranch === ref ? git.ahead || 0 : 0;
  }

  // gitView is a folder's git state as the Git tab writes it, or null when it
  // is not known (never read, or not a git repo).
  function gitView(git) {
    if (!git || !git.isGitRepo) return null;
    const dirty = ["stagedFiles", "modifiedFiles", "deletedFiles", "untrackedFiles"].reduce((n, k) => n + (git[k] || []).length, 0);
    const offDefault = !!git.defaultBranch && git.branch !== git.defaultBranch;
    const ref = git.defaultRef || git.defaultBranch || "";
    const ship = shipOf(git, offDefault, ref);
    const vs = offDefault ? "vs " + ref + " " + (ab(git.aheadOfDefault, git.behindDefault) || "even") : "";
    return {
      branch: git.branch || "",
      dirty,
      // On the trunk the ship number is its ahead count: not shown twice.
      origin: !offDefault && ship ? ab(0, git.behind) : ab(git.ahead, git.behind),
      ship,
      shipBehind: offDefault ? git.behindDefault || 0 : 0,
      shipRef: ref,
      vs,
      track: git.trackingBranch ? `${git.branch} → ${git.trackingBranch} ${ab(git.ahead, git.behind) || "up to date"}` : `${git.branch} (no upstream)`,
      clean: dirty ? `${dirty} changed file${dirty === 1 ? "" : "s"}` : "Clean",
    };
  }

  const repoName = (ws) => ws.name || String(ws.repoPath || "").split("/").filter(Boolean).pop() || "repo";

  // titleOf is what a session's row and header show: a lead is its repo, a
  // repo's planner (the session Make task sends to-do items to) is
  // "Planner" and never its first prompt, a thread its title (its first
  // prompt names it; until then it is new).
  function titleOf(session, ws) {
    if (session.kind === "agent") return ws ? repoName(ws) : session.title || session.name || "agent";
    if (session.kind === "lead") return ws ? repoName(ws) : session.title || "lead";
    if (session.kind === "planner") return session.title || "Planner";
    return session.title || "New thread";
  }

  // repoRow is one repo with its lead and its threads, oldest thread first.
  // A thread's helper in this repo is no thread of the repo: it is filed
  // under its own thread, in the repo that leads it (threadteam.js).
  function repoRow(ws, sessions) {
    const mine = sessions.filter((s) => s.workspaceId === ws.id && s.state !== "done" && s.role !== "helper");
    const lead = mine.find((s) => s.kind === "lead") || null;
    const threads = mine.filter((s) => s !== lead).sort((a, b) => a.createdAt - b.createdAt);
    return {
      id: ws.id, name: repoName(ws), path: ws.repoPath || "", lead, state: stateOf(lead),
      threads: threads.map((s) => ({ session: s, title: titleOf(s, ws), state: stateOf(s) })),
    };
  }

  // agentRow is a role agent's own workspace, listed after its group's
  // repos. An agent that runs as a session (kind "agent") carries it: its
  // row says where that session is, and opens its conversation here. One
  // that still runs in a terminal has none, and only says whether it is up.
  function agentRow(ws, sessions) {
    const session = sessions.find((s) => s.workspaceId === ws.id && s.kind === "agent" && s.state !== "done");
    if (session) return { id: ws.id, name: repoName(ws), state: agentState(session), session };
    return { id: ws.id, name: repoName(ws), state: ws.status === "live" ? "live" : "asleep" };
  }

  // agentState is an agent session's state, with the terminal lens's rule
  // for its green (flash.js): an agent finishes often and on its own, so its
  // done lasts a minute from when it finished and then reads idle. Orange
  // lasts until answered, as for anything.
  function agentState(session, now = Date.now()) {
    const state = stateOf(session);
    if (state !== "done" || !Flash) return state;
    return Flash.doneShows(true, session.attentionSince || 0, now) ? state : "idle";
  }

  const byName = (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" });

  // groupOf is one sidebar group: its repos, then its agents, each by name.
  function groupOf(id, name, list, sessions) {
    return {
      id, name,
      repos: list.filter((ws) => !ws.agent).map((ws) => repoRow(ws, sessions)).sort(byName),
      agents: list.filter((ws) => ws.agent).map((ws) => agentRow(ws, sessions)).sort(byName),
    };
  }

  // tree is the sidebar: each window that holds a repo or an agent, in the
  // daemon's order, then the ones in no window under "Other".
  function tree(windows, workspaces, sessions) {
    const all = workspaces || [];
    const byId = new Map(all.map((ws) => [ws.id, ws]));
    const placed = new Set();
    const groups = [];
    for (const win of windows || []) {
      const mine = (win.workspaceIds || []).map((id) => byId.get(id)).filter(Boolean);
      mine.forEach((ws) => placed.add(ws.id));
      if (mine.length) groups.push(groupOf(win.id, win.name, mine, sessions || []));
    }
    const loose = all.filter((ws) => !placed.has(ws.id));
    if (loose.length) groups.push(groupOf("", "Other", loose, sessions || []));
    return groups;
  }

  // The four models the picker offers, by Claude Code's own aliases: the
  // list a session has before it says which it offers (before its first
  // start), and the ones kept from what it does offer. A login can run a
  // dozen older models too; the picker is for choosing among the current
  // ones, and a session already on another model still shows it.
  // The names here carry no version: an alias always runs the newest model
  // of its family, so a release must not leave this list naming the old
  // one. Once a session has started, the names are its own, versions and
  // all. The ids only match these to what a session offers.
  const fallbackModels = [
    { value: "opus", name: "Opus", model: "claude-opus-5-5" }, { value: "fable", name: "Fable", model: "claude-fable-5-2" },
    { value: "sonnet", name: "Sonnet", model: "claude-sonnet-5-5" }, { value: "haiku", name: "Haiku", model: "claude-haiku-5-5" },
  ];

  // bare is a model id without the context-window tag some logins add
  // ("claude-fable-5-2[1m]"), so two spellings of one model compare equal.
  const bare = (id) => String(id || "").replace(/\[.*\]$/, "");

  // modelView is the header's model picker: the options, the one the row
  // asks for (wanted), the one the session is running on, and whether the
  // two differ, which is when "applies on the next restart" shows. The
  // running model is what the session's last turn said, not what it was
  // launched with: unknown before a turn, and then nothing is pending.
  function modelView(session, info) {
    const offered = info && info.models && info.models.length ? info.models : fallbackModels;
    const dflt = offered.find((m) => m.value === "default") || offered[0];
    const option = (m) => ({ value: m.value, label: m.name + (bare(m.model) === bare(dflt.model) ? " (default)" : ""), model: m.model });
    const all = offered.filter((m) => m.value !== "default").map(option);
    const options = currentOnly(all);
    const within = (list, value) => list.find((o) => o.value === value) || list.find((o) => bare(o.model) === bare(value) || bare(o.value) === bare(value));
    const find = (value) => within(options, value) || within(all, value);
    const wanted = (session && session.model ? find(session.model) : options.find((o) => bare(o.model) === bare(dflt.model))) || options[0];
    if (wanted && !options.includes(wanted)) options.push(wanted);
    const runningId = info && info.model ? info.model : "";
    const running = runningId ? find(runningId) || { value: runningId, label: runningId, model: runningId } : null;
    const pending = !!running && !!wanted && bare(running.model) !== bare(wanted.model);
    const name = (o) => (o ? o.label.replace(" (default)", "") : "");
    return { options, wanted: wanted ? wanted.value : "", wantedName: name(wanted), runningName: name(running), pending };
  }

  // currentOnly keeps one option per current model, in the picker's order;
  // every option when none of them is a current model (a login this page
  // does not know the names of).
  function currentOnly(options) {
    const kept = fallbackModels.map((cur) => options.find((o) => o.value === cur.value) || options.find((o) => bare(o.model) === cur.model)).filter(Boolean);
    return kept.length ? kept : options;
  }

  // ago is "just now", "4 min ago", "2 h ago" or "3 d ago".
  function ago(since, now) {
    const s = Math.max(0, Math.round((now - since) / 1000));
    if (!since || s < 45) return "just now";
    if (s < 3600) return Math.round(s / 60) + " min ago";
    if (s < 86400) return Math.round(s / 3600) + " h ago";
    return Math.round(s / 86400) + " d ago";
  }

  const api = { stateOf, stateLabel, ab, gitView, repoName, titleOf, tree, modelView, ago };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadModel = api;
})(typeof window !== "undefined" ? window : this);

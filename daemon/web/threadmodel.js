// threadmodel.js: what the thread page's sidebar and header say, worked out
// from what the daemon serves: the windows, their repos (workspaces), the
// runner sessions filed under each repo, and where each session is.
//
// A repo's own session is its lead (kind "lead"); every other session of the
// repo is a thread under it. Plain functions over plain data, so
// threadmodel_test.js runs them without a browser; threads.js owns the DOM.
(function (root) {
  "use strict";

  // stateOf is where a session is, as one word the page colours and labels:
  // running (a turn is going), waiting (it asked something), done (a turn
  // finished and nobody has looked), idle (up, nothing to say), asleep,
  // ended, or unknown (the daemon could not say; never guessed).
  function stateOf(session) {
    if (!session) return "asleep";
    const where = { done: "ended", asleep: "asleep", unknown: "unknown" }[session.state];
    if (where) return where;
    return { needs_input: "waiting", running: "running", done: "done" }[session.attention] || "idle";
  }

  const labels = { running: "Running", waiting: "Needs you", done: "Done", idle: "Idle", asleep: "Asleep", ended: "Ended", unknown: "Unknown" };
  const stateLabel = (state) => labels[state] || state;

  // ab is "↑2 ↓1" for a branch ahead and behind, "" when it is neither.
  function ab(ahead, behind) {
    return [ahead ? "↑" + ahead : "", behind ? "↓" + behind : ""].filter(Boolean).join(" ");
  }

  // gitView is a repo's git state as the sidebar writes it, or null when it
  // is not known (never read, or not a git repo).
  function gitView(git) {
    if (!git || !git.isGitRepo) return null;
    const dirty = ["stagedFiles", "modifiedFiles", "deletedFiles", "untrackedFiles"].reduce((n, k) => n + (git[k] || []).length, 0);
    const offDefault = !!git.defaultBranch && git.branch !== git.defaultBranch;
    const vs = offDefault ? "vs " + git.defaultBranch + " " + (ab(git.aheadOfDefault, git.behindDefault) || "even") : "";
    return {
      branch: git.branch || "",
      dirty,
      origin: ab(git.ahead, git.behind),
      vs,
      track: git.trackingBranch ? `${git.branch} → ${git.trackingBranch} ${ab(git.ahead, git.behind) || "up to date"}` : `${git.branch} (no upstream)`,
      clean: dirty ? `${dirty} changed file${dirty === 1 ? "" : "s"}` : "Clean",
    };
  }

  const repoName = (ws) => ws.name || String(ws.repoPath || "").split("/").filter(Boolean).pop() || "repo";

  // titleOf is what a session's row and header show: a lead is its repo, a
  // thread its title (its first prompt names it; until then it is new).
  function titleOf(session, ws) {
    if (session.kind === "lead") return ws ? repoName(ws) : session.title || "lead";
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
      id: ws.id, name: repoName(ws), path: ws.repoPath || "", git: gitView(ws.git), lead, state: stateOf(lead),
      threads: threads.map((s) => ({ session: s, title: titleOf(s, ws), state: stateOf(s) })),
    };
  }

  // tree is the sidebar: each window that holds a repo, in the daemon's
  // order, then the repos in no window under "Other". Role agents' own
  // workspaces are not repos and are left out.
  function tree(windows, workspaces, sessions) {
    const repos = (workspaces || []).filter((ws) => !ws.agent);
    const byId = new Map(repos.map((ws) => [ws.id, ws]));
    const placed = new Set();
    const groups = [];
    for (const win of windows || []) {
      const mine = (win.workspaceIds || []).map((id) => byId.get(id)).filter(Boolean);
      mine.forEach((ws) => placed.add(ws.id));
      if (mine.length) groups.push({ id: win.id, name: win.name, repos: mine.map((ws) => repoRow(ws, sessions || [])) });
    }
    const loose = repos.filter((ws) => !placed.has(ws.id));
    if (loose.length) groups.push({ id: "", name: "Other", repos: loose.map((ws) => repoRow(ws, sessions || [])) });
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

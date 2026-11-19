// threadcmds.js: the slash commands a draft offers before its session
// exists. A session says its commands only once it runs, so the page
// remembers the last list each repo's sessions said (per repo, lead and
// thread apart, since they may load different things) and a draft of that
// repo shows it. A repo with none yet borrows the other kind's list, then
// the newest list of any repo: the built-in commands are the same
// everywhere, and a command that is not there is still sent as typed.
//
// Plain functions over plain data (threadcmds_test.js), plus load and save
// against the browser's storage, which may be missing or blocked: then the
// page simply remembers nothing between loads.
(function (root) {
  "use strict";

  const KEEP = 30; // lists kept; the oldest go first
  const STORE = "ccmux-thread-commands";

  const keyOf = (ws, lead) => `${ws}:${lead ? "lead" : "thread"}`;

  // remember is memo with what a session of ws said of itself, when it named
  // commands, as of now (so "newest" is the last seen, not the first). A
  // list of terminal-only commands comes later than the commands (with a
  // turn's init), so one already known is kept until a new one comes.
  function remember(memo, ws, lead, info, now) {
    const commands = (info && info.commands) || [];
    if (!ws || !commands.length) return memo || {};
    const key = keyOf(ws, lead);
    const had = (memo || {})[key] || {};
    const terminal = (info.terminalCommands || []).length ? info.terminalCommands : had.terminalCommands || [];
    const next = { ...(memo || {}), [key]: { commands, terminalCommands: terminal, at: now } };
    const keys = Object.keys(next).sort((a, b) => next[b].at - next[a].at);
    return Object.fromEntries(keys.slice(0, KEEP).map((k) => [k, next[k]]));
  }

  // forDraft is the info a draft of ws shows: its own kind's list, else the
  // other kind's in the same repo, else the newest of any; {} when none.
  function forDraft(memo, ws, lead) {
    const m = memo || {};
    const hit = m[keyOf(ws, lead)] || m[keyOf(ws, !lead)];
    if (hit) return { commands: hit.commands, terminalCommands: hit.terminalCommands };
    const newest = Object.values(m).sort((a, b) => b.at - a.at)[0];
    return newest ? { commands: newest.commands, terminalCommands: newest.terminalCommands } : {};
  }

  function load(storage) {
    try {
      const v = JSON.parse(storage.getItem(STORE) || "{}");
      return v && typeof v === "object" && !Array.isArray(v) ? v : {};
    } catch (_) {
      return {}; // storage blocked or the value unreadable: start empty
    }
  }

  function save(storage, memo) {
    try {
      storage.setItem(STORE, JSON.stringify(memo));
    } catch (_) {
      /* storage blocked or full: kept for this page load only */
    }
  }

  const api = { remember, forDraft, load, save };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadCmds = api;
})(typeof window !== "undefined" ? window : this);

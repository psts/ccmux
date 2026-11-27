// threadwork.js: a conversation as the thread page lays it out. What the
// human said and what Claude said stay as they are; the tool calls between
// them fold into one line per run ("Edited 3 files in app, ran 2 commands ·
// 42 passed") that opens to its steps. Also what the right-hand tabs list:
// the files a session touched, with their line counts.
//
// Plain functions over the turns threadstream.js holds (threadwork_test.js).
(function (root) {
  "use strict";

  // rel is a path as the page writes it: inside the repo, without the repo.
  // repo is the repo's folder, or several folders for a session that works
  // in more than one (a thread member reads in its repo's folder and writes
  // in its worktree): the first that holds the path is left out.
  function rel(path, repo) {
    const p = String(path || "");
    for (const root of Array.isArray(repo) ? repo : [repo]) {
      if (root && p.startsWith(root + "/")) return p.slice(root.length + 1);
    }
    return p;
  }

  // threadRoots is the folders a thread member's paths are written without:
  // its repo's own, the worktree its row names, and the worktree its
  // conversation names. The last is read off the paths themselves, by the
  // folder name ccmux gives a thread's worktree (thread-<id>): once a thread
  // has merged the worktree is gone from the row, and the conversation still
  // shows what was changed in it.
  function threadRoots(session, repoPath, turns) {
    const roots = [session && session.worktree, repoPath].filter(Boolean);
    if (!session || !session.threadId) return roots;
    const mark = "/thread-" + session.threadId + "/";
    for (const t of turns || []) for (const part of t.parts || []) {
      const path = String((part.state && part.state.input && part.state.input.file_path) || "");
      const at = path.indexOf(mark);
      if (at > 0 && !roots.includes(path.slice(0, at + mark.length - 1))) roots.unshift(path.slice(0, at + mark.length - 1));
    }
    return roots;
  }

  // busMessage reads a prompt that came over the peers bus: who sent it and
  // what they said, without the envelope the daemon wraps it in.
  function busMessage(text) {
    const m = /^\[claude-peers message from (.+?) \(id [^)]*\)\]\n([\s\S]*)$/.exec(String(text || ""));
    if (!m) return null;
    return { from: m[1], text: m[2].replace(/\n+Reply with send_message to_id=\S+\s*$/, "").trim() };
  }

  // machineLine is a user turn nobody typed: the runner's own notice to a
  // session it restarted mid-turn, and Claude Code's mark where a turn was
  // stopped. Both are part of the conversation Claude reads; on the page
  // they are notes, not the human's words.
  const machineLine = /^\[(ccmux\] |Request interrupted by user)/;

  const firstLine = (s) => String(s || "").split("\n")[0];
  const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

  const writers = new Set(["Edit", "Write", "MultiEdit", "NotebookEdit"]);
  const searchers = new Set(["Grep", "Glob", "ToolSearch", "WebSearch", "WebFetch"]);

  // family sorts a tool into what the summary counts it as.
  function family(tool) {
    if (writers.has(tool)) return "edit";
    if (tool === "Read") return "read";
    if (tool === "Bash") return "run";
    if (tool === "AskUserQuestion") return "ask";
    return searchers.has(tool) ? "search" : "other";
  }

  // outcome is what a command's output says of its tests, when it says so
  // in the usual words; "" otherwise. A hint for the eye, not a verdict.
  function outcome(output) {
    const text = String(output || "");
    const failed = /(\d+)\s+(?:tests?\s+)?fail(?:ed|ures?|ing)?\b/i.exec(text);
    if (failed && Number(failed[1]) > 0) return failed[1] + " failed";
    const passed = /(\d+)\s+(?:tests?\s+)?pass(?:ed|ing)?\b/i.exec(text);
    return passed ? passed[1] + " passed" : "";
  }

  // step is one tool call as a line of an opened run.
  function step(part, repo) {
    const st = part.state || {};
    const input = st.input || {};
    const what = input.file_path ? rel(input.file_path, repo) : firstLine(input.command || input.pattern || input.url || input.query || input.description || st.title || "");
    let note = "";
    if (st.status === "error") note = "failed";
    else if (st.diff) note = [st.diff.added ? "+" + st.diff.added : "", st.diff.removed ? "−" + st.diff.removed : ""].filter(Boolean).join(" ");
    else if (part.tool === "Bash") note = outcome(st.output);
    return { id: part.id, kind: part.tool || "tool", what, note, status: st.status || "", part };
  }

  // The words for each family, in the order a summary says them.
  const phrases = [
    ["edit", (n) => "edited " + plural(n, "file")], ["run", (n) => "ran " + plural(n, "command")],
    ["read", (n) => "read " + plural(n, "file")], ["search", (n) => "searched " + plural(n, "time")],
    ["ask", (n) => "asked you " + plural(n, "question")],
    ["other", (n) => "took " + plural(n, "other step")],
  ];

  // summarize is a run's one line, its outcome (the last command's tests, or
  // how many steps failed) and whether it is still going.
  function summarize(parts, repoLabel) {
    const count = { edit: new Set(), run: 0, read: 0, search: 0, ask: 0, other: 0 };
    for (const p of parts) {
      const f = family(p.tool);
      if (f === "edit") count.edit.add((p.state && p.state.input && p.state.input.file_path) || p.id);
      else count[f]++;
    }
    const said = phrases.map(([f, say]) => [f, f === "edit" ? count.edit.size : count[f], say]).filter(([, n]) => n > 0)
      .map(([f, n, say]) => say(n) + (f === "edit" && repoLabel ? " in " + repoLabel : ""));
    const line = said.join(", ");
    const failed = parts.filter((p) => p.state && p.state.status === "error").length;
    const running = parts.some((p) => p.state && p.state.status === "running");
    const lastRun = parts.filter((p) => p.tool === "Bash").pop();
    return {
      summary: line ? line[0].toUpperCase() + line.slice(1) : "Worked",
      outcome: failed ? plural(failed, "step") + " failed" : outcome(lastRun && lastRun.state && lastRun.state.output),
      status: running ? "running" : failed ? "error" : "done",
    };
  }

  // userBlock is a user turn as the page shows it: the human's own words, a
  // message from the bus, or something Claude Code wrote itself.
  function userBlock(turn) {
    const text = turn.parts.filter((p) => p.type === "text").map((p) => p.text).join("\n");
    const files = turn.parts.filter((p) => p.type === "file").map((p) => ({ name: p.name, mime: p.mime }));
    if (turn.info.synthetic) return { kind: "note", id: turn.info.id, text, level: "info", own: true };
    if (machineLine.test(text)) return { kind: "note", id: turn.info.id, text, level: "notice" };
    const bus = busMessage(text);
    if (bus) return { kind: "bus", id: turn.info.id, from: bus.from, text: bus.text };
    return { kind: "user", id: turn.info.id, text, files };
  }

  // blocks lays the turns out in order. A run of tool calls ends at the next
  // thing said (by Claude, the human, or a note); empty text and thinking
  // are not things said.
  function blocks(turns, repo, repoLabel) {
    const out = [];
    let run = null;
    const close = () => {
      if (run) out.push({ kind: "work", id: run[0].id, steps: run.map((p) => step(p, repo)), ...summarize(run, repoLabel) });
      run = null;
    };
    const say = (b) => { close(); out.push(b); };
    for (const turn of turns) {
      if (turn.info.role === "user") say(userBlock(turn));
      else if (turn.info.role === "note") say({ kind: "note", id: turn.info.id, text: turn.parts.map((p) => p.text || "").join("\n"), level: turn.info.level || "notice" });
      else {
        const at = (turn.info.time && turn.info.time.created) || 0; // when the message began
        for (const p of turn.parts) {
          if (p.type === "tool") (run = run || []).push(p);
          else if (p.type === "text" && p.text) say({ kind: "claude", id: p.id, text: p.text, at });
        }
        const err = turn.info.error;
        if (err) say({ kind: "error", id: turn.info.id + ".error", text: (err.data && err.data.message) || err.name || "error" });
      }
    }
    close();
    return out;
  }

  const api = { rel, threadRoots, busMessage, outcome, step, summarize, blocks };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadWork = api;
})(typeof window !== "undefined" ? window : this);

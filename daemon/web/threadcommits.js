// threadcommits.js: the thread page's Git tab. A line at its top says how
// the session's folder stands (branch, changed files, ahead and behind its
// upstream and its trunk); under it, it lists the commits in
// the picked session's repo that have not shipped yet, in the groups the
// daemon reads (GET /v1/sessions/{id}/commits: on this branch, not pushed,
// pushed and not shipped, or since the last release). A click on a commit
// opens its whole message under its row, and a second click closes it;
// any number can be open at once. No diff: the message is what the tab is
// for. Which groups a repo has is the daemon's to say
// (internal/api/sessioncommits.go); this file only draws.
//
// The list is read when the session changes, when the tab is shown, and
// when a turn ends (a turn may have committed). Never on a timer.
//
// create() takes the page:
//   call(method, path, body)  the page's daemon call; it throws with the reason
//   changed()                 something to draw: redraw the page
//   now()                     the time, in ms
(function (root) {
  "use strict";

  const node = typeof module !== "undefined" && module.exports;
  const Model = () => (node ? require("./threadmodel.js") : root.ccmuxThreadModel);
  const ago = (t, now) => Model().ago(t, now);

  const commits = (k) => `${k} commit${k === 1 ? "" : "s"}`;

  // gitLine is the line over the groups: the folder's branch, its changed
  // files, how it stands against its upstream, and the pill for what a ship
  // would send against the trunk (↑) and what the trunk has that it has not
  // (↓). Each part says itself in full on hover. null when the daemon sent
  // no status (a daemon from before it did, or a status that failed).
  function gitLine(status) {
    const g = Model().gitView(status);
    if (!g) return null;
    return {
      branch: g.branch || "(no branch)",
      dirty: g.dirty ? "●" + g.dirty : "", dirtyTip: g.clean,
      origin: g.origin, originTip: g.track,
      ship: Model().ab(g.ship, g.shipBehind),
      shipTip: [g.ship ? `${commits(g.ship)} not on ${g.shipRef} yet: ready to ship` : "", g.shipBehind ? `${commits(g.shipBehind)} on ${g.shipRef} not in this branch` : ""].filter(Boolean).join(". "),
    };
  }

  // listView is what the tab says of the daemon's answer: a quiet line, or
  // the groups to draw. An answer that is not a list of groups (a daemon
  // from before the route) is nothing to show.
  function listView(data, now) {
    if (data && data.repo === false) return { quiet: "Not a git repo." };
    const groups = ((data && Array.isArray(data.groups)) ? data.groups : []).filter((g) => g && (g.commits || []).length);
    if (!groups.length) return { quiet: "Nothing waiting: every commit has shipped." };
    return {
      groups: groups.map((g) => ({
        key: g.key, title: g.title, count: g.commits.length + (g.more || 0),
        more: g.more ? `and ${g.more} more` : "",
        rows: g.commits.map((c) => ({
          sha: c.sha, short: c.short || String(c.sha).slice(0, 7), subject: c.subject || "(no message)", when: ago(c.time, now),
          body: c.body || "", meta: metaLine(c, now),
        })),
      })),
    };
  }

  // metaLine is the line under an open commit's message.
  function metaLine(d, now) {
    return [String(d.sha || "").slice(0, 10), d.author, ago(d.time, now)].filter(Boolean).join(" · ");
  }

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };
  const button = (cls, text, onclick) => Object.assign(el("button", cls, text), { type: "button", onclick });
  const tip = (e, title) => { e.title = title; return e; };

  // drawGit is gitLine on screen; nothing when there is no line.
  function drawGit(box, status) {
    const l = gitLine(status);
    if (!l) return;
    add(box, add(el("div", "git-line"),
      tip(el("span", "branch", l.branch), l.originTip),
      l.dirty ? tip(el("span", "dirty", l.dirty), l.dirtyTip) : null,
      l.origin ? tip(el("span", "ab", l.origin), l.originTip) : null,
      l.ship ? tip(el("span", "ship", l.ship), l.shipTip) : null));
  }

  function create(page) {
    // for: the session the tab shows; list: the last answer (null while
    // read); open: the shas whose message is out, kept across reads of the
    // list; ver counts changes, so a redraw that changed nothing leaves the
    // tab (and its scroll) alone.
    const q = { for: "", list: null, listErr: "", open: new Set(), busy: false, ver: 0, drawn: -1 };
    const bump = () => { q.ver++; page.changed(); };

    async function loadList() {
      const id = q.for;
      if (!id) return;
      try {
        const d = await page.call("GET", `/v1/sessions/${encodeURIComponent(id)}/commits`);
        if (q.for !== id) return; // the session changed while it was read
        Object.assign(q, { list: d, listErr: "" });
      } catch (e) {
        if (q.for !== id) return;
        q.listErr = e.message;
      }
      bump();
    }

    const toggle = (sha) => { if (!q.open.delete(sha)) q.open.add(sha); bump(); };

    // follow is called at every draw: a new session starts over, a turn
    // that ended reads the list again.
    function follow(id, busy) {
      if (id !== q.for) {
        Object.assign(q, { for: id, list: null, listErr: "", open: new Set(), busy });
        q.ver++;
        loadList();
        return;
      }
      if (q.busy && !busy) loadList();
      q.busy = busy;
    }

    // drawRow is one commit: its row, and under it its message when open.
    function drawRow(sec, r) {
      const open = q.open.has(r.sha);
      const row = button("cm-row" + (open ? " open" : ""), null, () => toggle(r.sha));
      row.title = open ? "Hide the message" : "Show the whole message";
      row.setAttribute("aria-expanded", String(open));
      add(sec, add(row, el("span", "cm-sha", r.short), el("span", "cm-subject", r.subject), el("span", "cm-when", r.when)));
      if (!open) return;
      const msg = add(el("div", "cm-msg"), r.body ? el("pre", "cm-body", r.body) : null, el("div", "cm-meta", r.meta));
      add(sec, msg);
    }

    function drawList(box) {
      if (q.listErr) return add(box, el("div", "quiet error", "The commits could not be read: " + q.listErr));
      if (!q.list) return add(box, el("div", "quiet", "Reading the commits…"));
      drawGit(box, q.list.git);
      const v = listView(q.list, page.now());
      if (v.quiet) return add(box, el("div", "quiet", v.quiet));
      for (const g of v.groups) {
        const sec = add(el("div", "cg"), add(el("div", "cg-head"), el("span", "cg-title", g.title), el("span", "cg-count", String(g.count))));
        for (const r of g.rows) drawRow(sec, r);
        if (g.more) add(sec, el("div", "quiet", g.more));
        add(box, sec);
      }
      return box;
    }

    // draw puts the tab in box for session id (busy: a turn is running).
    function draw(box, id, busy) {
      follow(id || "", !!busy);
      if (q.drawn === q.ver) return;
      q.drawn = q.ver;
      box.textContent = "";
      if (!q.for) add(box, el("div", "quiet", "Pick a session to see its repo's commits."));
      else drawList(box);
    }

    // shown is the tab coming on screen: read the list again.
    const shown = () => { if (q.for) loadList(); };

    return { draw, shown };
  }

  const api = { listView, metaLine, gitLine, create };
  if (node) module.exports = api;
  else root.ccmuxThreadCommits = api;
})(typeof window !== "undefined" ? window : this);

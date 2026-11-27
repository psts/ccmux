// threadcommits.js: the thread page's Commits tab. It lists the commits in
// the picked session's repo that have not shipped yet, in the groups the
// daemon reads (GET /v1/sessions/{id}/commits: on this branch, not pushed,
// pushed and not shipped, or since the last release), and opens one in
// place of the list, with its message and its diff per file
// (GET /v1/sessions/{id}/commits/{sha}). Which groups a repo has is the
// daemon's to say (internal/api/sessioncommits.go); this file only draws.
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
  const ago = (t, now) => (node ? require("./threadmodel.js") : root.ccmuxThreadModel).ago(t, now);

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
        rows: g.commits.map((c) => ({ sha: c.sha, short: c.short || String(c.sha).slice(0, 7), subject: c.subject || "(no message)", when: ago(c.time, now) })),
      })),
    };
  }

  // fileLabel is one file's line over its diff: its path (a rename says
  // where it came from) and its lines added and removed.
  function fileLabel(f) {
    const where = f.from ? `${f.from} → ${f.path}` : f.path;
    if (f.binary) return `${where} · binary`;
    return `${where} · +${f.added || 0} −${f.removed || 0}`;
  }

  // metaLine is the line under a commit's subject.
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

  function create(page) {
    // for: the session the tab shows; list and one: the last answers (null
    // while read); open: the sha on show; ver counts changes, so a redraw
    // that changed nothing leaves the tab (and an open diff's scroll) alone.
    const q = { for: "", list: null, listErr: "", open: "", one: null, oneErr: "", busy: false, ver: 0, drawn: -1 };
    const bump = () => { q.ver++; page.changed(); };
    const base = (id) => `/v1/sessions/${encodeURIComponent(id)}/commits`;

    async function loadList() {
      const id = q.for;
      if (!id) return;
      try {
        const d = await page.call("GET", base(id));
        if (q.for !== id) return; // the session changed while it was read
        Object.assign(q, { list: d, listErr: "" });
      } catch (e) {
        if (q.for !== id) return;
        q.listErr = e.message;
      }
      bump();
    }

    async function openCommit(sha) {
      const id = q.for;
      Object.assign(q, { open: sha, one: null, oneErr: "" });
      bump();
      try {
        const d = await page.call("GET", `${base(id)}/${encodeURIComponent(sha)}`);
        if (q.for !== id || q.open !== sha) return;
        q.one = d;
      } catch (e) {
        if (q.for !== id || q.open !== sha) return;
        q.oneErr = e.message;
      }
      bump();
    }

    const back = () => { Object.assign(q, { open: "", one: null, oneErr: "" }); bump(); };

    // follow is called at every draw: a new session starts over, a turn
    // that ended reads the list again.
    function follow(id, busy) {
      if (id !== q.for) {
        Object.assign(q, { for: id, list: null, listErr: "", open: "", one: null, oneErr: "", busy });
        q.ver++;
        loadList();
        return;
      }
      if (q.busy && !busy) loadList();
      q.busy = busy;
    }

    function drawList(box) {
      if (q.listErr) return add(box, el("div", "quiet error", "The commits could not be read: " + q.listErr));
      if (!q.list) return add(box, el("div", "quiet", "Reading the commits…"));
      const v = listView(q.list, page.now());
      if (v.quiet) return add(box, el("div", "quiet", v.quiet));
      for (const g of v.groups) {
        const sec = add(el("div", "cg"), add(el("div", "cg-head"), el("span", "cg-title", g.title), el("span", "cg-count", String(g.count))));
        for (const r of g.rows) {
          const row = button("cm-row", null, () => openCommit(r.sha));
          row.title = "Show this commit";
          add(sec, add(row, el("span", "cm-sha", r.short), el("span", "cm-subject", r.subject), el("span", "cm-when", r.when)));
        }
        if (g.more) add(sec, el("div", "quiet", g.more));
        add(box, sec);
      }
      return box;
    }

    function drawOne(box) {
      add(box, button("plain small cm-back", "← All commits", back));
      if (q.oneErr) return add(box, el("div", "quiet error", "The commit could not be read: " + q.oneErr));
      const d = q.one;
      if (!d) return add(box, el("div", "quiet", "Reading the commit…"));
      add(box, el("div", "cm-head", d.subject || "(no message)"), el("div", "cm-meta", metaLine(d, page.now())));
      if (d.body) add(box, el("pre", "cm-body", d.body));
      const files = d.files || [];
      if (!files.length) add(box, el("div", "quiet", "No files changed."));
      const View = root.ccmuxThreadView;
      for (const f of files) add(box, View.diff(f.diff || { hunks: [] }, fileLabel(f)));
      return box;
    }

    // draw puts the tab in box for session id (busy: a turn is running).
    function draw(box, id, busy) {
      follow(id || "", !!busy);
      if (q.drawn === q.ver) return;
      q.drawn = q.ver;
      box.textContent = "";
      if (!q.for) add(box, el("div", "quiet", "Pick a session to see its repo's commits."));
      else if (q.open) drawOne(box);
      else drawList(box);
    }

    // shown is the tab coming on screen: read the list again.
    const shown = () => { if (q.for) loadList(); };

    return { draw, shown };
  }

  const api = { listView, fileLabel, metaLine, create };
  if (node) module.exports = api;
  else root.ccmuxThreadCommits = api;
})(typeof window !== "undefined" ? window : this);

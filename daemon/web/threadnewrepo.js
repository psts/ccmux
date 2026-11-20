// threadnewrepo.js: the sidebar's "+" dialog, which adds a repo. Like the
// lens's "New workspace" it lists the folders under the daemon's projects
// root (GET /v1/projects), goes into one on a click, makes a new folder
// there (POST /v1/projects, git init if asked), and "+" on a folder makes
// it a repo session (POST /v1/workspaces). Its group is picked here: no
// group, one there is, or a new one by name (threadgroups.js). The new
// repo then opens, ready for its first message.
(() => {
  "use strict";
  const G = window.ccmuxThreadGroups;
  const el = (tag, cls, text) => window.ccmuxThreadView.el(tag, cls, text);
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };
  const enc = encodeURIComponent;

  function button(label, cls, onclick, title) {
    const b = el("button", cls, label);
    b.type = "button";
    b.onclick = onclick;
    if (title) b.title = title;
    return b;
  }

  // create wires it to the page: d.call is its fetch, d.windows() the
  // groups, d.changed() reads the lists again (a promise), d.opened(wsId)
  // goes to the new repo, d.overlay() and d.sheet() are its elements.
  function create(d) {
    const st = { path: "", list: null, note: "", group: "", newName: "", folder: "", git: true, busy: false, gen: 0 };

    // open shows the dialog with group picked, if there is one by that name.
    function open(group) {
      Object.assign(st, { path: "", list: null, note: "", group: G.groupChoices(d.windows(), group).value, newName: "", folder: "", git: true, busy: false });
      d.overlay().classList.remove("hidden");
      browse("");
    }
    const close = () => { st.gen++; d.overlay().classList.add("hidden"); };
    const isOpen = () => !d.overlay().classList.contains("hidden");
    const escape = () => { if (!isOpen()) return false; close(); return true; };

    // browse lists one folder; an answer for a folder left since is dropped.
    async function browse(rel) {
      const gen = ++st.gen;
      Object.assign(st, { path: rel, list: null, note: "Loading…" });
      draw();
      try {
        const got = await d.call("GET", "/v1/projects?path=" + enc(rel));
        if (gen !== st.gen) return;
        st.list = got;
        st.note = (got.projects || []).length ? "" : "No folders in here.";
      } catch (e) {
        if (gen !== st.gen) return;
        st.note = "Couldn't list folders: " + e.message;
      }
      draw();
    }

    // group is the picked group's name, or {error} for a new one that
    // cannot be.
    const group = () => (st.group === G.NEW ? G.groupName(st.newName, d.windows()) : { name: st.group });

    async function addRepo(p) {
      const g = group();
      if (g.error) { st.note = g.error; draw(); return; }
      await busy(async () => {
        const ws = await d.call("POST", "/v1/workspaces", G.createBody(p, g.name));
        close();
        await d.changed();
        d.opened(ws.id);
      }, "Adding " + p.name);
    }

    async function makeFolder() {
      const name = st.folder.trim();
      if (!name) return;
      const rel = st.path ? st.path + "/" + name : name;
      await busy(async () => {
        await d.call("POST", "/v1/projects", { path: rel, git: st.git });
        st.folder = "";
        browse(st.path);
      }, "Making " + name);
    }

    // busy runs one change at a time and says what failed.
    async function busy(fn, what) {
      if (st.busy) return;
      st.busy = true;
      try { await fn(); } catch (e) { st.note = `${what} failed: ${e.message}`; } finally { st.busy = false; }
      if (isOpen()) draw();
    }

    function draw() {
      const sheet = d.sheet();
      sheet.textContent = "";
      const head = add(el("div", "sheet-head"), el("h2", "sheet-title", "Add a repo"), button("✕", "sheet-close", close, "Close"));
      const body = add(el("div", "sheet-body nr-body"), groupRow(), el("div", "nr-path", "/" + st.path), folderList(),
        st.note ? el("div", "nr-note", st.note) : null, folderRow());
      add(sheet, head, body);
    }

    // groupRow is the group picker, and the name box while "New group…"
    // is picked.
    function groupRow() {
      const row = add(el("label", "nr-group"), el("span", "nr-label", "Group"));
      const sel = el("select", "nr-select");
      const choices = G.groupChoices(d.windows(), st.group === G.NEW ? "" : st.group);
      for (const o of choices.options) { const opt = el("option", "", o.label); opt.value = o.value; add(sel, opt); }
      sel.value = st.group === G.NEW ? G.NEW : choices.value;
      sel.onchange = () => { st.group = sel.value; draw(); };
      add(row, sel);
      if (st.group !== G.NEW) return row;
      const name = el("input", "nr-input");
      name.placeholder = "New group's name";
      name.value = st.newName;
      name.oninput = () => { st.newName = name.value; };
      return add(row, name);
    }

    // folderList is the folders here: a click goes in, "+" adds it.
    function folderList() {
      const list = el("ul", "nr-list");
      if (st.path) add(list, add(el("li", "nr-up"), button("‹ Back", "nr-name", () => browse((st.list && st.list.parent) || ""))));
      for (const p of (st.list && st.list.projects) || []) {
        const into = button(p.name, "nr-name", () => browse(st.path ? st.path + "/" + p.name : p.name), "Open " + p.name);
        const plus = button("+", "nr-add", () => addRepo(p), "Add " + p.name + " as a repo");
        plus.setAttribute("aria-label", "Add " + p.name + " as a repo");
        add(list, add(el("li", "nr-item"), into, p.git ? el("span", "nr-git", "git") : null, plus));
      }
      return list;
    }

    // folderRow makes a new folder where the list is.
    function folderRow() {
      const name = el("input", "nr-input");
      name.placeholder = "New folder here";
      name.value = st.folder;
      name.oninput = () => { st.folder = name.value; };
      name.onkeydown = (e) => { if (e.key === "Enter") makeFolder(); };
      const git = el("input", "");
      git.type = "checkbox";
      git.checked = st.git;
      git.onchange = () => { st.git = git.checked; };
      const gitLabel = add(el("label", "nr-check"), git, el("span", "", "git init"));
      return add(el("div", "nr-folder"), name, gitLabel, button("Create", "primary", makeFolder));
    }

    d.overlay().onclick = (e) => { if (e.target === d.overlay()) close(); };
    return { open, close, isOpen, escape };
  }

  window.ccmuxThreadNewRepo = { create };
})();

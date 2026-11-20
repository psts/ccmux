// threadcapsctl.js: the thread page's skills and plugins screens, wired.
// There are two: the whole list, which is this page's own tab in the
// Settings window (settingsmodal.js), and the panel of one repo or group.
// It reads the catalog and the choices from the daemon, keeps what the open
// screen shows, and sends what the human picks: Update, Add source, Pin, a
// repo's or group's choice, and Restart. threads.js makes one with the
// page's own fetch and lists and the Settings window (create) and calls it
// from the sidebar, the Escape key and each new read of the lists. Meaning
// is threadcaps.js's, the look threadcapsview.js's.
(() => {
  "use strict";
  const Caps = window.ccmuxThreadCaps, CV = window.ccmuxThreadCapsView;
  const enc = encodeURIComponent;

  const $ = (id) => document.getElementById(id);
  let deps = null;
  let tabBox = null; // this page's page in the Settings window
  const C = { mode: "", target: null, caps: [], sources: [], choices: {}, data: null, busy: "", note: "", error: "", adding: false, url: "", loading: false, staleSig: "" };

  const windowName = (id) => { const w = deps.lists().windows.find((x) => x.id === id); return w ? w.name : ""; };

  // members are the repos whose sessions the open panel is about.
  function members() {
    if (C.mode === "repo") return [C.target.id];
    const w = deps.lists().windows.find((x) => x.id === C.target.id);
    return w ? w.workspaceIds || [] : [];
  }

  // A panel is the sheet of one repo or group; the other screen ("settings")
  // is the tab in the Settings window.
  const isPanel = () => C.mode === "repo" || C.mode === "group";
  const staleNow = () => (isPanel() ? Caps.stale(deps.lists().sessions, members()) : []);

  // --- reading ---

  function scopePath() {
    if (C.mode === "repo") return `/v1/workspaces/${enc(C.target.id)}/capabilities`;
    if (C.mode === "group") return `/v1/windows/${enc(C.target.id)}/capabilities`;
    return "/v1/capabilities/choices";
  }

  // load reads the catalog and what the open sheet is about, then draws.
  const onSheet = (mode, target) => C.mode === mode && C.target === target;

  // A read that lands after another sheet was opened, answered or failed,
  // is dropped: it belongs to the sheet that is gone.
  async function load() {
    const mode = C.mode, target = C.target;
    C.loading = true;
    draw();
    const read = await Promise.all([deps.call("GET", "/v1/capabilities"), deps.call("GET", scopePath())]).catch((e) => e);
    if (!onSheet(mode, target)) return;
    if (read instanceof Error) C.error = read.message;
    else {
      Object.assign(C, { caps: read[0].capabilities || [], sources: read[0].sources || [] });
      if (mode === "settings") C.choices = read[1]; else C.data = read[1];
    }
    C.loading = false;
    draw();
  }

  // --- drawing ---

  function settingsView() {
    const names = Object.fromEntries(deps.lists().windows.map((w) => [w.id, w.name]));
    return {
      sources: C.sources.map((s) => Caps.sourceRow(s, C.caps, Date.now())),
      entries: C.caps.map((c) => Caps.entryRow(c, C.choices, names)),
      adding: C.adding, url: C.url, note: C.note, error: C.error, loading: C.loading,
    };
  }

  function panelView() {
    const d = C.data || {};
    const repo = C.mode === "repo";
    const stale = staleNow();
    const rows = !C.data ? [] : repo ? Caps.repoRows(C.caps, d, windowName(d.window)) : Caps.groupRows(C.caps, d);
    const total = Caps.cost(C.caps, repo ? d.effective : d.on);
    return {
      repo, rows, loading: C.loading, note: C.note, error: C.error,
      title: `${C.target.name} · skills and plugins`,
      foot: Caps.footLine(total, repo ? "This repo's" : "This group's"), stale: Caps.staleLine(stale.length),
    };
  }

  const handlers = {
    close, update, startAdd: () => { C.adding = true; draw(); }, cancelAdd: () => { Object.assign(C, { adding: false, url: "" }); draw(); },
    typed: (v) => { C.url = v; }, addSource, pin, set, restart,
  };

  function draw() {
    $("caps-overlay").classList.toggle("hidden", !isPanel());
    if (!C.mode) return;
    const ctx = { ...handlers, busy: C.busy };
    if (isPanel()) CV.panel($("caps-sheet"), panelView(), ctx);
    else CV.tab(tabBox, settingsView(), ctx);
    C.staleSig = staleNow().map((s) => s.id).join(",");
  }

  // --- opening and closing ---

  function open(mode, target) {
    Object.assign(C, { mode, target, data: null, busy: "", note: "", error: "", adding: false, url: "" });
    closeMenu();
    load();
  }

  function close() {
    C.mode = "";
    draw();
  }

  function closeMenu() { $("caps-menu").classList.add("hidden"); }

  // menu opens the right-click menu of a repo row (kind "repo") or a
  // group's name ("group") where the click was, or under the button. After
  // Skills and plugins come the page's own entries: a group's
  // (deps.groupItems: move it, open it alone, add a repo to it) or a
  // repo's (deps.repoItems: move it to another group).
  function menu(kind, item, ev) {
    const box = $("caps-menu");
    const pick = kind === "repo" ? () => open("repo", { id: item.id, name: item.name }) : () => open("group", { id: item.id, name: item.name });
    const own = kind === "group" ? deps.groupItems : deps.repoItems;
    CV.menu(box, [["Skills and plugins…", pick], ...(own ? own(item) : [])], pointOf(ev), closeMenu);
    box.classList.remove("hidden");
  }

  // escape closes the menu, else the panel; false when neither is open.
  // The Settings window closes itself (its own escape), and with it the tab.
  function escape() {
    if (!$("caps-menu").classList.contains("hidden")) { closeMenu(); return true; }
    if (!isPanel()) return false;
    close();
    return true;
  }

  // listsChanged redraws an open panel when who runs on the old list
  // changed: a restart landed, or a choice made another session stale.
  function listsChanged() {
    if (isPanel() && staleNow().map((s) => s.id).join(",") !== C.staleSig) draw();
  }

  // --- acting ---

  // run does one thing the human asked, with the sheet saying it is busy,
  // then what came of it. An answer that lands after another sheet was
  // opened is about the sheet that is gone, so it is not shown on this one.
  async function run(busy, work) {
    const mode = C.mode, target = C.target;
    Object.assign(C, { busy, error: "", note: "" });
    draw();
    const out = await Promise.resolve().then(work).then((note) => ({ note: note || "" }), (e) => ({ error: e.message }));
    if (!onSheet(mode, target)) return;
    Object.assign(C, { busy: "", note: out.note || "", error: out.error || "" });
    await load();
  }

  function update(id) {
    return run(id, async () => Caps.updateNote(await deps.call("POST", `/v1/capabilities/sources/${enc(id)}/update`)));
  }

  function addSource(url) {
    const id = Caps.sourceId(url);
    if (!id) { C.error = "That is not a git address."; draw(); return Promise.resolve(); }
    return run("+", async () => {
      const res = await deps.call("POST", "/v1/capabilities/sources", { id, url: url.trim() });
      Object.assign(C, { adding: false, url: "" });
      return "Added. " + Caps.updateNote(res);
    });
  }

  function pin(id, pinned) {
    return run(id, async () => { await deps.call("POST", `/v1/capabilities/${enc(id)}/pin`, { pinned }); });
  }

  function set(capId, state) {
    return run(capId, async () => { await deps.call("PUT", `${scopePath()}/${enc(capId)}`, { state }); deps.changed(); });
  }

  // restart starts every session on the old list again, after the human
  // said yes; it never runs without that click.
  async function restart() {
    const list = staleNow();
    if (!list.length || C.busy || !deps.confirm(`Restart ${list.length} session${list.length === 1 ? "" : "s"} now? Each stops what it is doing and starts again on the new list.`)) return;
    await run("restart", async () => {
      const failed = [];
      for (const s of list) await deps.call("POST", `/v1/sessions/${enc(s.id)}/restart`).catch((e) => failed.push(`${s.id}: ${e.message}`));
      deps.changed();
      if (failed.length) throw new Error("Not restarted: " + failed.join("; "));
      return `Restarting ${list.length} session${list.length === 1 ? "" : "s"}.`;
    });
  }

  function wire() {
    const box = $("caps-overlay");
    box.onclick = (e) => { if (e.target === box) close(); };
  }

  // create wires the screens to the page: deps.call is its fetch,
  // deps.lists() its windows, workspaces and sessions, deps.changed() has
  // them read again, deps.confirm asks the human, and deps.settings is the
  // Settings window, which gets this page's tab at its front;
  // deps.groupItems(group) and deps.repoItems(repo), optional, add entries
  // to a group's or a repo's menu ([label, fn], or [label, null, entries]
  // for a list of its own). The tab reads its list each time the window opens, and lets go
  // when it closes. One page, one of these.
  function create(d) {
    deps = d;
    wire();
    tabBox = d.settings.addTab({
      page: "caps", label: "Skills and plugins", first: true,
      opened: () => open("settings", null),
      closed: () => { if (C.mode === "settings") close(); },
    });
    tabBox.classList.add("caps-tab");
    return { menu, escape, listsChanged };
  }

  // pointOf is where a menu opens: at a right-click, else under the
  // element that was tapped.
  function pointOf(ev) {
    if (ev && typeof ev.clientX === "number" && (ev.clientX || ev.clientY)) return { x: ev.clientX, y: ev.clientY };
    const t = ev && ev.currentTarget ? ev.currentTarget : ev && ev.target;
    if (t && t.getBoundingClientRect) { const r = t.getBoundingClientRect(); return { x: r.left, y: r.bottom + 2 }; }
    return { x: 0, y: 0 };
  }

  window.ccmuxThreadCapsCtl = { create, pointOf };
})();

// threadcapsview.js: the drawing of the thread page's skills and plugins
// screens: its tab in the Settings window, the panel a right-click on a
// repo or a group opens, and that right-click's menu. Each function takes the rows
// threadcaps.js made and the callbacks threadcapsctl.js gives; nothing
// here fetches or keeps state. Text always goes in as text.
(() => {
  "use strict";
  const el = (tag, cls, text) => window.ccmuxThreadView.el(tag, cls, text);
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };

  function button(label, cls, onclick, title) {
    const b = el("button", cls, label);
    b.type = "button";
    b.onclick = onclick;
    if (title) b.title = title;
    return b;
  }

  // head is a sheet's title line with its close button.
  const head = (title, close) => add(el("div", "sheet-head"), el("h2", "sheet-title", title), button("✕", "sheet-close", close, "Close"));

  const tag = (t) => (t ? el("span", "cap-tag " + t.tone, t.text) : null);

  // item is one entry: its name with its kind and tags, a line under it,
  // and its controls at the right.
  function item(row, right, tags) {
    const box = el("div", "cap-item");
    box.dataset.cap = row.id;
    const name = add(el("div", "cap-name"), el("span", "", row.name), row.kind ? el("span", "cap-kind", row.kind) : null, ...(tags || []));
    return add(box, name, row.meta || row.note ? el("div", "cap-meta", row.meta || row.note) : null, right ? add(el("div", "cap-right"), right) : null);
  }

  // seg is a row's choice: a button per state, the current one lit.
  function seg(states, current, pick) {
    const box = el("span", "seg");
    for (const [state, label] of states) {
      const b = button(label, "seg-opt" + (state === current ? " on" : ""), () => { if (state !== current) pick(state); });
      b.dataset.state = state;
      b.setAttribute("aria-pressed", String(state === current));
      add(box, b);
    }
    return box;
  }

  function sourceEl(s, ctx) {
    const box = el("div", "cap-src");
    box.dataset.source = s.id;
    const busy = ctx.busy === s.id;
    const up = button(busy ? "Updating…" : "Update", s.behind ? "primary small" : "plain small", () => ctx.update(s.id), "Pull the newest from git");
    up.disabled = !!ctx.busy;
    return add(box, add(el("div", "cap-src-body"), el("strong", "", s.name), el("div", "cap-where", s.where)), tag(s.tag) || el("span"), up);
  }

  // addForm is "+ Add source": a button that opens a field for a git URL.
  function addForm(v, ctx) {
    if (!v.adding) return add(el("div"), button("+ Add source", "plain small", ctx.startAdd));
    const form = el("form", "cap-add");
    const input = el("input", "cap-url");
    input.id = "cap-url";
    input.placeholder = "https://github.com/you/your-plugins";
    input.value = v.url || "";
    input.oninput = () => ctx.typed(input.value);
    form.onsubmit = (e) => { e.preventDefault(); ctx.addSource(input.value); };
    const go = button(ctx.busy === "+" ? "Adding…" : "Add", "primary small", () => ctx.addSource(input.value));
    go.disabled = !!ctx.busy;
    return add(form, input, go, button("Cancel", "plain small", ctx.cancelAdd));
  }

  const lines = (v) => add(el("div", "cap-lines"), v.note ? el("div", "cap-note", v.note) : null, v.error ? el("div", "cap-error", v.error) : null);

  // capsTab is the Skills and plugins tab: the sources, then every entry.
  function capsTab(box, v, ctx) {
    add(box, el("div", "cap-label", "Sources"));
    for (const s of v.sources) add(box, sourceEl(s, ctx));
    add(box, addForm(v, ctx), lines(v), el("div", "cap-label", `Everything you can turn on (${v.entries.length})`));
    for (const g of v.groups) add(box, groupEl(g, ctx));
    if (!v.entries.length) add(box, add(el("div", "cap-list"), el("div", "quiet", v.loading ? "Reading the list…" : "Nothing in the list yet. Add a source, or update one.")));
    return box;
  }

  // groupEl is one source's section of the list: a heading that folds and
  // unfolds it, with how many entries it holds, and its entries.
  function groupEl(g, ctx) {
    const wrap = el("section", "cap-group" + (g.folded ? " folded" : ""));
    wrap.dataset.source = g.id;
    const headBtn = add(button("", "cap-group-head", () => ctx.fold(g.id), g.folded ? "Show these" : "Hide these"),
      el("span", "chev", "›"), el("span", "cap-group-name", g.name), el("span", "cap-group-count", String(g.rows.length)));
    headBtn.setAttribute("aria-expanded", String(!g.folded));
    add(wrap, headBtn);
    if (g.folded) return wrap;
    const list = el("div", "cap-list");
    for (const r of g.rows) add(list, entryEl(r, ctx));
    return add(wrap, list);
  }

  function entryEl(r, ctx) {
    const pin = r.pin ? button(r.pin, "plain small", () => ctx.pin(r.id, r.pin === "Pin"), r.pin === "Pin" ? "Stay at this version" : "Follow the newest version") : null;
    if (pin) pin.disabled = !!ctx.busy;
    return item(r, pin, [r.update ? el("span", "cap-tag warn", r.update) : null]);
  }

  // tab draws the Skills and plugins tab into box, the page the Settings
  // window gave it. The window's title, tab bar and close button are the
  // window's own (settingsmodal.js).
  function tab(box, v, ctx) {
    box.textContent = "";
    return capsTab(box, v, ctx);
  }

  const repoStates = [["group", "As group"], ["on", "On"], ["off", "Off"]];
  const groupStates = [["on", "On"], ["off", "Off"]];

  // panel is one repo's or group's list, with what it loads under it and
  // the sessions that still run on the old list.
  function panel(box, v, ctx) {
    box.textContent = "";
    const body = el("div", "sheet-body");
    add(body, el("div", "cap-base", "Always on: CLAUDE.md, memory, the bus, thread limits."));
    const list = el("div", "cap-list");
    for (const r of v.rows) add(list, item(r, seg(v.repo ? repoStates : groupStates, r.state, (state) => ctx.set(r.id, state))));
    if (!v.rows.length) add(list, el("div", "quiet", v.loading ? "Reading the list…" : "Nothing in the list yet. Add a source in Settings."));
    const foot = add(el("div", "cap-foot"), el("span", "", v.foot));
    if (v.stale) add(foot, el("span", "cap-stale", " " + v.stale + " "), button(ctx.busy === "restart" ? "Restarting…" : "Restart them now", "apply", ctx.restart));
    add(body, list, foot, lines(v));
    return add(box, head(v.title, ctx.close), body);
  }

  // menu is the right-click menu at a point on the page, over a backdrop
  // that closes it. An item is [label, fn], or [label, null, items] for one
  // that opens its own list beside it (subItem).
  function menu(box, items, at, close) {
    box.textContent = "";
    box.onclick = (e) => { if (e.target === box) close(); };
    box.oncontextmenu = (e) => { e.preventDefault(); close(); };
    const list = el("div", "ctx-menu");
    list.setAttribute("role", "menu");
    list.style.left = Math.max(4, at.x) + "px";
    list.style.top = Math.max(4, at.y) + "px";
    for (const it of items) add(list, it[2] ? subItem(it, close) : menuItem(it, close));
    return add(box, list);
  }

  function menuItem([label, fn], close) {
    const b = button(label, "ctx-item", () => { close(); fn(); });
    b.setAttribute("role", "menuitem");
    return b;
  }

  // subItem is an entry with its own list ("Move to group ›"). A mouse
  // opens it by pointing at it; a click or a tap opens it too, and never
  // shuts it (a tap is also a pointer entering). It opens to the right, or
  // to the left where the right has no room; on a phone it opens in place
  // (threads.css).
  function subItem([label, , items], close) {
    const wrap = el("div", "ctx-wrap");
    const sub = el("div", "ctx-menu ctx-sub hidden");
    sub.setAttribute("role", "menu");
    for (const it of items) add(sub, menuItem(it, close));
    const b = add(button("", "ctx-item ctx-parent", () => show(true)), el("span", "", label), el("span", "ctx-arrow", "›"));
    b.setAttribute("role", "menuitem");
    b.setAttribute("aria-haspopup", "menu");
    b.setAttribute("aria-expanded", "false");
    function show(on) {
      sub.classList.toggle("hidden", !on);
      b.setAttribute("aria-expanded", String(on));
      if (on && wrap.getBoundingClientRect && typeof innerWidth === "number") sub.classList.toggle("flip", wrap.getBoundingClientRect().right + 220 > innerWidth);
    }
    wrap.onpointerenter = (e) => { if (e.pointerType === "mouse") show(true); };
    wrap.onpointerleave = (e) => { if (e.pointerType === "mouse") show(false); };
    return add(wrap, b, sub);
  }

  window.ccmuxThreadCapsView = { tab, panel, menu };
})();

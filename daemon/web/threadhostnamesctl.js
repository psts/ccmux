// threadhostnamesctl.js: a repo's "Hostnames…" dialog on the thread page,
// from its right-click menu. It lists the repo's dev host names and their
// ports, what the repo's processes listen on right now (read again every
// two seconds while it is open), and saves the names. What a row means is
// threadhostnames.js. The dev command is not here: nothing on this page
// starts a dev server, and a save that leaves it out keeps the saved one.
(() => {
  "use strict";
  const H = window.ccmuxThreadHostnames;
  const el = (tag, cls, text) => window.ccmuxThreadView.el(tag, cls, text);
  const add = (parent, ...kids) => { for (const k of kids) if (k) parent.appendChild(k); return parent; };
  const enc = encodeURIComponent;
  const POLL_MS = 2000;

  function button(label, cls, onclick, title) {
    const b = el("button", cls, label);
    b.type = "button";
    b.onclick = onclick;
    if (title) b.title = title;
    return b;
  }

  // create wires it to the page: d.call is its fetch, d.workspace(id) the
  // repo's workspace as last read, d.changed() reads the lists again,
  // d.overlay() and d.sheet() are its elements, d.later(fn, ms) its timer.
  function create(d) {
    const st = { ws: null, rows: [], live: [], error: "", busy: false, gen: 0 };

    async function open(r) {
      const ws = d.workspace(r.id) || { id: r.id, name: r.name };
      const gen = ++st.gen;
      Object.assign(st, { ws, rows: H.startRows(ws, null), live: [], error: "", busy: false });
      d.overlay().classList.remove("hidden");
      draw();
      const got = await d.call("GET", `/v1/workspaces/${enc(ws.id)}/port-suggestions`).catch(() => null);
      if (gen !== st.gen) return;
      if (!(ws.hostnames || []).length && got) st.rows = H.startRows(ws, got);
      st.live = got ? got.listening || [] : null;
      draw();
      poll(gen);
    }
    const close = () => { st.gen++; d.overlay().classList.add("hidden"); };
    const isOpen = () => !d.overlay().classList.contains("hidden");
    const escape = () => { if (!isOpen()) return false; close(); return true; };

    // poll reads what listens again until the dialog closes or opens on
    // another repo; only that list is drawn again, never the rows being typed.
    function poll(gen) {
      d.later(async () => {
        if (gen !== st.gen) return;
        st.live = await d.call("GET", `/v1/workspaces/${enc(st.ws.id)}/listeners`).then((x) => x.listening || [], () => null);
        if (gen !== st.gen) return;
        drawLive();
        poll(gen);
      }, POLL_MS);
    }

    async function save() {
      const parsed = H.parseRows(st.rows);
      if (parsed.error) { st.error = parsed.error; drawError(); return; }
      if (st.busy) return;
      st.busy = true;
      try {
        await d.call("PUT", `/v1/workspaces/${enc(st.ws.id)}/hostnames`, { hostnames: parsed.hostnames });
        close();
        d.changed();
      } catch (e) {
        st.error = "Saving failed: " + e.message;
        drawError();
      } finally { st.busy = false; }
    }

    function draw() {
      const sheet = d.sheet();
      sheet.textContent = "";
      const head = add(el("div", "sheet-head"), el("h2", "sheet-title", `${st.ws.name} · hostnames`), button("✕", "sheet-close", close, "Close"));
      const body = add(el("div", "sheet-body hn-body"),
        el("p", "hn-crumb", "Dev host names served over the tailnet. Each name goes to the port its app binds on its own."),
        rowList(), button("+ Add a name", "hn-link", () => { st.rows = [...st.rows, { name: "", port: "" }]; draw(); }),
        el("div", "hn-label", "Listening now"), add(el("ul", "hn-live")), el("p", "hn-error hidden"),
        add(el("div", "hn-actions"), button("Save", "primary", save)));
      add(sheet, head, body);
      drawLive();
      drawError();
    }

    function rowList() {
      const list = el("ul", "hn-rows");
      st.rows.forEach((r, i) => {
        const name = el("input", "nr-input hn-name");
        Object.assign(name, { placeholder: "name", spellcheck: false, value: r.name });
        name.oninput = () => { r.name = name.value; };
        const port = el("input", "nr-input hn-port");
        Object.assign(port, { placeholder: "port", inputMode: "numeric", value: r.port, title: "The port the app binds on its own" });
        port.oninput = () => { r.port = port.value; drawLive(); };
        const del = button("✕", "hn-del", () => { st.rows = st.rows.filter((_, j) => j !== i); draw(); }, "Remove this name");
        add(list, add(el("li", "hn-row"), name, port, del));
      });
      return list;
    }

    function drawLive() {
      const ul = d.sheet().querySelector(".hn-live");
      if (!ul) return;
      ul.textContent = "";
      const note = H.liveNote(st.live);
      if (note) { add(ul, el("li", "hn-note", note)); return; }
      for (const l of H.liveRows(st.live, st.rows)) {
        const map = () => { st.rows = H.mapRow(st.rows, st.ws.name, l.port); draw(); };
        const tail = l.mapped ? el("span", "hn-mapped", "mapped") : button("map", "hn-link", map, `Add a name for port ${l.port}`);
        add(ul, add(el("li", "hn-live-row"), el("span", "hn-live-port", String(l.port)), el("span", "hn-proc", l.process), tail));
      }
    }

    function drawError() {
      const p = d.sheet().querySelector(".hn-error");
      if (!p) return;
      p.textContent = st.error;
      p.classList.toggle("hidden", !st.error);
    }

    d.overlay().onclick = (e) => { if (e.target === d.overlay()) close(); };
    return { open, close, isOpen, escape };
  }

  window.ccmuxThreadHostnamesCtl = { create };
})();

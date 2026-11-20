// threadhostnames.js: the rules of a repo's Hostnames dialog on the thread
// page, with no page in them (threadhostnamesctl.js draws it). A row is
// {name, port}: the port is whatever the app binds on its own, and the
// daemon routes the name to it over the tailnet (PUT
// /v1/workspaces/{id}/hostnames). Same rules as the lens's sheet (app.js).
(() => {
  "use strict";

  // startRows is what the dialog opens with: the saved names, else what
  // the repo suggests, else one empty row to type in.
  function startRows(ws, suggested) {
    let rows = ((ws && ws.hostnames) || []).map((h) => ({ name: h.name, port: String(h.port) }));
    if (!rows.length && suggested) rows = (suggested.suggestions || []).map((s) => ({ name: s.name, port: String(s.port) }));
    return rows.length ? rows : [{ name: "", port: "" }];
  }

  // parseRows is the body to save, or {error}. A row with neither a name
  // nor a port is an unused editor row, not a mapping.
  function parseRows(rows) {
    const hostnames = [];
    for (const r of rows) {
      const name = String(r.name || "").trim();
      const text = String(r.port || "").trim();
      if (!name && !text) continue;
      const port = /^\d+$/.test(text) ? parseInt(text, 10) : 0;
      if (!name) return { error: `Port ${text}: the name is missing.` };
      if (port < 1 || port > 65535) return { error: `${name}: the port must be 1 to 65535, the port the app binds itself.` };
      hostnames.push({ name, port });
    }
    return { hostnames };
  }

  // liveRows marks each port something listens on: mapped when a row
  // names it already.
  function liveRows(listening, rows) {
    const named = new Set(rows.map((r) => String(r.port).trim()));
    return (listening || []).map((l) => ({ port: l.port, process: l.process || "", mapped: named.has(String(l.port)) }));
  }

  // liveNote is the line in place of the list: a failed read is not
  // "nothing listens".
  function liveNote(listening) {
    if (!listening) return "Could not read what listens from the daemon.";
    if (!listening.length) return "Nothing yet. Start the dev server from this repo's session and it shows up here.";
    return "";
  }

  // mapRow adds a row for a listening port, in place of a lone empty one.
  function mapRow(rows, wsName, port) {
    const lone = rows.length === 1 && !rows[0].name && !rows[0].port;
    const kept = lone ? [] : rows;
    return [...kept, { name: suggestLabel(wsName, kept.length), port: String(port) }];
  }

  // suggestLabel is the repo's name as a host name for the first row,
  // then name-2, name-3.
  function suggestLabel(wsName, existing) {
    const slug = String(wsName || "app").toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "") || "app";
    return existing ? `${slug}-${existing + 1}` : slug;
  }

  // where is what follows "name : port" when the name routes elsewhere
  // (the server moved up a port) or cannot answer (another repo holds it).
  function where(h) {
    if (h.livePort) return ` → ${h.livePort}`;
    if (h.heldBy) return ` (held by ${h.heldBy})`;
    return "";
  }

  // links are the menu's entries that open a saved name in a new tab.
  function links(ws) {
    return ((ws && ws.hostnames) || []).filter((h) => h.url).map((h) => ({ label: `${h.listening ? "●" : "○"} ${h.name} : ${h.port}${where(h)}`, url: h.url }));
  }

  const api = { startRows, parseRows, liveRows, liveNote, mapRow, suggestLabel, where, links };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxThreadHostnames = api;
})();

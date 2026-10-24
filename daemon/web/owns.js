// ccmux web lens — "What It Owns…": the one line every session in a window
// is told about a repo, so work there is handed to its session. Haiku
// writes it from the repo's CLAUDE.md and README; a typed line overrides it
// and "Use summary" hands it back. The Mac app's OwnsSheet is the same
// editor over the same route (GET/PUT /v1/workspaces/{id}/owns).
(function () {
  "use strict";

  let wsId = null;
  // ready: the current line was read. Until then the box is not the line,
  // so saving it would overwrite a typed line with whatever the box holds.
  let ready = false;

  // The two buttons that write are off until the line is read; the Mac
  // sheet greys out the same two.
  function setReady(v) {
    ready = v;
    $("owns-save").disabled = !v;
    $("owns-clear").disabled = !v;
  }

  async function open(ws) {
    wsId = ws.id;
    setReady(false);
    $("owns-title").textContent = `What ${ws.name} owns`;
    $("owns-manual").value = "";
    $("owns-auto").textContent = "Reading…";
    hideError();
    $("owns-modal").classList.remove("hidden");
    const r = await call("GET");
    if (wsId !== ws.id) return;
    if (r) { show(r); setReady(true); } else $("owns-auto").textContent = "Could not read the current line, so it cannot be changed now.";
  }

  function show(o) {
    $("owns-auto").textContent = o.auto
      ? `Summary: ${o.auto}`
      : "No summary yet. It is written the next time a session in this window starts, if the repo has a CLAUDE.md or README.";
    $("owns-manual").value = o.manual || "";
  }

  async function save(manual) {
    if (!ready) return;
    const r = await call("PUT", { manual });
    if (r) $("owns-modal").classList.add("hidden");
  }

  // call answers the route's {auto, manual, line}, or null with the reason
  // shown in the sheet.
  async function call(method, body) {
    try {
      const r = await fetch(`/v1/workspaces/${wsId}/owns`, {
        method, headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined,
      });
      if (r.ok) return await r.json();
      let msg = "HTTP " + r.status;
      try { msg = (await r.json()).error || msg; } catch (_) { /* not JSON: the status says it */ }
      showError(msg);
    } catch (e) {
      showError(String(e));
    }
    return null;
  }

  function showError(msg) {
    $("owns-error").textContent = msg;
    $("owns-error").classList.remove("hidden");
  }

  function hideError() { $("owns-error").classList.add("hidden"); }

  const close = () => $("owns-modal").classList.add("hidden");
  $("owns-close").onclick = close;
  $("owns-modal").onclick = (e) => { if (e.target.id === "owns-modal") close(); };
  $("owns-save").onclick = () => save($("owns-manual").value.trim());
  $("owns-clear").onclick = () => save("");
  $("owns-manual").addEventListener("keydown", (e) => { if (e.key === "Enter") save($("owns-manual").value.trim()); });

  window.ccmuxOwns = { open, save };
})();

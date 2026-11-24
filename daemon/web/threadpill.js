// threadpill.js: the header's settings pill (Model, Advisor, Effort, Mode). Each
// picker is a native select laid over a short label of its choice: the open
// list keeps its full words ("Opus 5.5 (default)", "Settings (Fable)"), the
// closed one reads "Opus 5.5", "Fable". shortLabel is pure, so
// threadpill_test.js drives it.
(function (root) {
  "use strict";

  const PICKERS = ["model", "advisor", "effort", "mode"];

  // shortLabel is an option's words as the closed picker shows them.
  function shortLabel(text) {
    const t = String(text || "").replace(/ \(default\)$/, "");
    const m = /^Settings \((.+)\)$/.exec(t);
    if (!m) return t;
    return m[1] === "none" ? "None" : m[1];
  }

  // chosen is the select's picked option's words, "" when none is picked.
  function chosen(select) {
    const opt = Array.from(select.children).find((o) => o.value === select.value);
    return opt ? opt.textContent : "";
  }

  // sync sets every picker's shown label from its pick, and keeps it in step
  // while the person picks (oninput, so the page's own onchange stays its own).
  function sync($) {
    for (const id of PICKERS) {
      const select = $(id), shown = $(id + "-shown");
      if (!select || !shown) continue;
      const draw = () => { shown.textContent = shortLabel(chosen(select)); };
      select.oninput = draw;
      draw();
    }
  }

  const api = { shortLabel, sync };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadPill = api;
})(typeof window !== "undefined" ? window : this);

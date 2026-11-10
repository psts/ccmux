// sidebarresize.js: drag the left menu's right edge to make it narrower or
// wider. Desktop only: on a phone the menu is a drawer (style.css hides the
// grip there). The width is this browser's own choice, kept in localStorage
// (a per-viewer convenience: if storage is blocked the menu just opens at
// its default). Double-click the grip to go back to the default.
// Runs after app.js and uses its scheduleFit at call time, so the terminal
// refits to the width the menu leaves it.
(function (root) {
  "use strict";

  const MIN = 160, MAX = 480, KEY = "ccmux-sidebar-width";

  // clampWidth keeps a width between MIN and MAX, and never more than half
  // the window, so the terminal always keeps the larger share.
  function clampWidth(w, windowWidth) {
    const most = Math.max(MIN, Math.min(MAX, Math.floor(windowWidth / 2)));
    return Math.round(Math.min(most, Math.max(MIN, w)));
  }

  // storedWidth reads a saved width, or null when there is none worth using.
  function storedWidth(raw, windowWidth) {
    const n = Number.parseInt(raw, 10);
    return Number.isFinite(n) ? clampWidth(n, windowWidth) : null;
  }

  const api = { clampWidth, storedWidth, MIN, MAX };
  if (typeof module !== "undefined" && module.exports) { module.exports = api; return; }
  root.ccmuxSidebarResize = api;

  const bar = document.getElementById("sidebar");
  if (!bar) return;
  const refit = () => { if (typeof scheduleFit === "function") scheduleFit(); };
  const apply = (w) => { if (w === null) bar.style.removeProperty("--sidebar-w"); else bar.style.setProperty("--sidebar-w", w + "px"); };
  const save = (w) => { try { if (w === null) localStorage.removeItem(KEY); else localStorage.setItem(KEY, String(w)); } catch (_) { /* storage blocked */ } };
  let saved = null;
  try { saved = localStorage.getItem(KEY); } catch (_) { /* storage blocked */ }
  apply(storedWidth(saved, window.innerWidth));

  const grip = document.createElement("div");
  grip.id = "sidebar-grip";
  grip.title = "Drag to resize, double-click to reset";
  bar.appendChild(grip);

  let dragging = false;
  grip.addEventListener("pointerdown", (e) => {
    dragging = true;
    try { grip.setPointerCapture(e.pointerId); } catch (_) { /* no real pointer (a synthetic event) */ }
    document.body.classList.add("sidebar-resizing");
    e.preventDefault();
  });
  grip.addEventListener("pointermove", (e) => {
    if (!dragging) return;
    apply(clampWidth(e.clientX - bar.getBoundingClientRect().left, window.innerWidth));
    refit();
  });
  const end = () => {
    if (!dragging) return;
    dragging = false;
    document.body.classList.remove("sidebar-resizing");
    save(Math.round(bar.getBoundingClientRect().width));
    refit();
  };
  grip.addEventListener("pointerup", end);
  grip.addEventListener("pointercancel", end);
  grip.addEventListener("dblclick", () => { apply(null); save(null); refit(); });
})(typeof window !== "undefined" ? window : this);

// drawerswipe.js: on a phone, a swipe in from the left edge opens the session
// drawer, as the ☰ button does, and a swipe to the left closes it again.
//
// A swipe that starts at the edge is the drawer's alone: it is kept from the
// terminal under it, so opening the drawer never scrolls Claude or a shell.
// Safari's own back gesture owns the very edge in a browser tab, and a page
// cannot take it; in the installed app there is no browser chrome and this is
// what the edge does.
(function (root) {
  "use strict";
  const EDGE_PX = 24; // an opening swipe starts this close to the left edge
  const SWIPE_PX = 50; // and travels at least this far, mostly sideways

  // gesture says what a finished one-finger swipe means: "open", "close" or "".
  // start and end are {x, y}; open is whether the drawer was open at the start.
  function gesture(start, end, open) {
    const dx = end.x - start.x;
    if (Math.abs(dx) < SWIPE_PX || Math.abs(end.y - start.y) > Math.abs(dx)) return "";
    if (!open && dx > 0 && start.x <= EDGE_PX) return "open";
    if (open && dx < 0) return "close";
    return "";
  }

  // attach listens on doc in the capture phase, ahead of the terminal's own
  // touch handling. ui: {isMobile, isOpen, open, close}, read at touch time.
  function attach(doc, ui) {
    let start = null;
    const edgeSwipe = () => start && !start.open;
    doc.addEventListener("touchstart", (e) => {
      start = null;
      if (e.touches.length !== 1 || !ui.isMobile()) return;
      const t = e.touches[0];
      const open = ui.isOpen();
      if (!open && t.clientX > EDGE_PX) return;
      start = { x: t.clientX, y: t.clientY, open };
      if (edgeSwipe()) e.stopPropagation();
    }, { capture: true, passive: true });
    doc.addEventListener("touchmove", (e) => {
      if (edgeSwipe()) e.stopPropagation();
    }, { capture: true, passive: true });
    const finish = (e) => {
      if (!start) return;
      const t = e.changedTouches[0];
      const g = gesture(start, { x: t.clientX, y: t.clientY }, start.open);
      if (edgeSwipe()) e.stopPropagation();
      start = null;
      if (g === "open") ui.open();
      else if (g === "close") ui.close();
    };
    doc.addEventListener("touchend", finish, { capture: true, passive: true });
    doc.addEventListener("touchcancel", () => { start = null; }, { capture: true, passive: true });
  }

  const api = { gesture, attach, EDGE_PX, SWIPE_PX };
  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
    return;
  }
  root.ccmuxDrawerSwipe = api;
  // The page: the drawer is app.js's, and only exists as such on a phone
  // (style.css, max-width: 700px).
  const phone = root.matchMedia("(max-width: 700px)");
  const app = () => root.document.getElementById("app");
  attach(root.document, {
    isMobile: () => phone.matches,
    isOpen: () => app().classList.contains("drawer-open"),
    open: () => root.openDrawer(),
    close: () => root.closeDrawer(),
  });
})(typeof window !== "undefined" ? window : this);

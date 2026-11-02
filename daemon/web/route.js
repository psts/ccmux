// route.js: which ccmux window THIS browser window shows, and bringing a set of
// browser windows back after the app is quit and relaunched.
//
// The Mac app gives each of its windows one ccmux window; this is the same rule
// for the browser. The page's address holds it (/?w=<window>&ws=<session>), so
// a reload keeps it, two browser windows can show two different ccmux windows,
// and a browser that restores its own windows restores the right ones for free.
//
// Everything here is plain functions over a Storage, so route_test.js can drive
// the rules without a browser. app.js owns the DOM and the daemon calls.
(function (root) {
  "use strict";

  const LAST_WS = "ccmux-last-ws:"; // + window id -> the session last shown in it
  const PAGES = "ccmux-pages";      // page id -> {w, seen, closedAt}: one per browser window
  // Windows that stopped this close together were stopped by one quit. Every
  // window of an app hears pagehide in the same instant when it quits; a window
  // closed by hand earlier is minutes older and is not brought back.
  const BURST_MS = 10000;
  // A notification tap the service worker stashed for a page that may have
  // missed its message is good for this long; after that it is a tap nobody
  // took, and replaying it later would yank a window somewhere unasked.
  const NAV_TTL_MS = 60000;

  function parse(search) {
    const p = new URLSearchParams(search || "");
    return { w: p.get("w") || "", ws: p.get("ws") || "" };
  }

  function href(w, ws) {
    const p = new URLSearchParams();
    if (w) p.set("w", w);
    if (ws) p.set("ws", ws);
    const s = p.toString();
    return "/" + (s ? "?" + s : "");
  }

  // bootTarget is the window a page opened at w/ws should show, "" for none.
  // A browser window keeps its own window unless the session it is asked to
  // show belongs to a different one: the window in a link can be out of date
  // (the session moved, the window was deleted). A session in no window, live
  // (AVAILABLE) or deleted, pins none, so the address's window stands while it
  // exists; pagewin.js decides whether that session can be shown in it.
  function bootTarget(windows, w, ws) {
    const byWs = ws ? windowOf(windows, ws) : "";
    if (byWs) return byWs;
    return (windows || []).some((x) => x.id === w) ? w : "";
  }

  // takesNav says what a page does with a stashed tap (a plain URL, so a page
  // from before this rule still reads its ws): "drop" a stale one, else
  // "take" it. Whichever window takes a tap by message clears the stash, so it
  // is only read when that message was missed (a page the phone had frozen),
  // and the window reading it is the one the tap brought back.
  function takesNav(url, now) {
    const p = new URLSearchParams(String(url).split("?")[1] || "");
    const at = Number(p.get("t") || 0);
    return at && now - at > NAV_TTL_MS ? "drop" : "take";
  }

  // windowOf is the shared window a session belongs to, "" when none.
  function windowOf(windows, wsId) {
    const w = (windows || []).find((x) => (x.workspaceIds || []).includes(wsId));
    return w ? w.id : "";
  }

  // Storage can throw (private mode, blocked site data); every reader falls back
  // to "nothing remembered", which is the state a first launch is in anyway.
  function readPages(storage) {
    try {
      const v = JSON.parse(storage.getItem(PAGES) || "{}");
      return v && typeof v === "object" ? v : {};
    } catch (_) {
      return {};
    }
  }

  function writePages(storage, pages) {
    try { storage.setItem(PAGES, JSON.stringify(pages)); } catch (_) {}
  }

  function remember(storage, w, ws) {
    if (!w || !ws) return;
    try { storage.setItem(LAST_WS + w, ws); } catch (_) {}
  }

  function recall(storage, w) {
    try { return (w && storage.getItem(LAST_WS + w)) || ""; } catch (_) { return ""; }
  }

  // known says whether this page id was here before: true after a reload of the
  // same browser window, false in a new one. Read before the first beat.
  function known(storage, pageId) {
    return Object.prototype.hasOwnProperty.call(readPages(storage), pageId);
  }

  // beat records that this browser window is open on window w (w may be "" for
  // the picker). Called on load, on every refresh and when it is shown again.
  function beat(storage, pageId, w, now) {
    const pages = readPages(storage);
    pages[pageId] = { w: w || "", seen: now, closedAt: 0 };
    writePages(storage, pages);
  }

  // ended stamps the moment this browser window went away (pagehide). A reload
  // beats again straight after, which clears the stamp.
  function ended(storage, pageId, now) {
    const pages = readPages(storage);
    if (!pages[pageId]) return;
    pages[pageId].closedAt = now;
    writePages(storage, pages);
  }

  // takeRestoreSet answers a launch with no window in its address: the windows
  // that were on screen when the app last went away, oldest first. `live` holds
  // the page ids that answered just now; they are other windows still open, so
  // they are neither restored nor forgotten. A page that never said goodbye (a
  // crash) counts as gone at its last beat. The entries it reads are removed:
  // one relaunch restores once.
  function takeRestoreSet(storage, pageId, live) {
    const pages = readPages(storage);
    const gone = Object.entries(pages)
      .filter(([id]) => id !== pageId && !live.has(id))
      .map(([id, p]) => ({ id, w: p.w || "", at: p.closedAt || p.seen || 0 }));
    for (const g of gone) delete pages[g.id];
    writePages(storage, pages);
    if (!gone.length) return [];
    const last = Math.max(...gone.map((g) => g.at));
    const set = [];
    for (const g of gone.sort((a, b) => a.at - b.at)) {
      if (g.w && last - g.at <= BURST_MS && !set.includes(g.w)) set.push(g.w);
    }
    return set;
  }

  const api = {
    parse, href, windowOf, bootTarget, takesNav, remember, recall, known, beat, ended, takeRestoreSet,
    BURST_MS, NAV_TTL_MS,
  };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxRoute = api;
})(typeof window !== "undefined" ? window : this);

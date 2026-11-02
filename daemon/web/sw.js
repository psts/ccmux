// ccmux service worker: makes the web lens installable (a cached app shell)
// and receives Web Push notifications. It never caches the API or WebSocket —
// live session data must always hit the network.
"use strict";

// The shell cache is named after the lens build it holds (web.go's Build),
// and holds that build whole or not at all: see refreshShell.
const SHELL_PREFIX = "ccmux-shell-";
// A pending deep-link target, stashed on notificationclick so a client that was
// frozen/backgrounded (and may drop the postMessage) can still pick it up when it
// regains visibility. Kept in its own cache so shell-cache cleanup never purges it.
const NAV_CACHE = "ccmux-nav";
const PENDING_NAV = "/__ccmux_pending_nav";
// ws id -> window id, written by every page (pagewin.js) so a tap can find the
// browser window that shows the session's window.
const WS_WINDOWS = "/__ccmux_ws_windows";
// The build whose shell cache is complete, kept in NAV_CACHE beside the two
// above. Only refreshShell writes it, and only after the whole set landed.
const SHELL_BUILD = "/__ccmux_shell_build";

// The app shell: everything needed to boot the lens offline. Session bytes are
// never cached (they come over /v1/attach, which the fetch handler leaves alone).
const SHELL = [
  "/",
  "/index.html",
  "/app.js",
  "/route.js",
  "/pagewin.js",
  "/touchscroll.js",
  "/drawerswipe.js",
  "/agentchat.js",
  "/board.js",
  "/owns.js",
  "/agentcaps.js",
  "/agentmodal.js",
  "/peers.js",
  "/push.js",
  "/update.js",
  "/build.js",
  "/style.css",
  "/manifest.webmanifest",
  "/vendor/xterm.js",
  "/vendor/xterm.css",
  "/vendor/addon-fit.js",
  "/icons/icon-192.png",
  "/icons/icon-512.png",
];

self.addEventListener("install", (e) => {
  e.waitUntil(refreshShell().then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(self.clients.claim());
});

// Network first for the shell; network-only for the API (/v1/*) and anything
// that isn't a same-origin GET. Network first, so a deploy lands on the very
// next load instead of the one after: the daemon tags every shell file with
// its build (web.go), so an unchanged file costs a 304, not the bytes.
// WebSocket upgrades never reach fetch, so /v1/attach and /v1/events are
// unaffected either way.
//
// The cache is the fallback when the daemon cannot be reached, and when it is
// slow to send the page itself: past SLOW_MS the cached page is served. A page
// is ALL cache or ALL network, and the cache holds ONE build. Mixed per file,
// a slow link would hand a page the new tiny build.js beside the old big
// app.js, and that page would run old code while believing it is current, so
// update.js would never move it on. So a page from the network takes nothing
// from the cache, a page from the cache takes everything from it, and nothing
// a page loads is written to the cache: only refreshShell writes it, whole.
const SLOW_MS = 3000;
const cachedPages = new Set(); // client ids of pages that booted from the cache

self.addEventListener("fetch", (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== location.origin || url.pathname.startsWith("/v1/")) {
    return; // default network handling
  }
  if (req.mode === "navigate") e.respondWith(pageFetch(e));
  else if (cachedPages.has(e.clientId)) e.respondWith(cacheFirst(req));
  else e.respondWith(fetch(req, { cache: "no-cache" }));
});

async function pageFetch(e) {
  const net = fetch(e.request, { cache: "no-cache" });
  // Whenever the network answers, even after the cache already served this
  // page, the cache catches up with the build the daemon serves now.
  e.waitUntil(net.then(() => refreshShell(), () => {}));
  const copy = await slowOrFailed(net, fromShell(e.request));
  if (!copy) return net; // the network answered first, or there is nothing else
  if (e.resultingClientId) cachedPages.add(e.resultingClientId);
  return copy;
}

// slowOrFailed settles with the cached copy when the network fails or has not
// answered in SLOW_MS, and with null when the network answers first. A copy
// that cannot be read counts as none, so a load fails instead of hanging.
function slowOrFailed(net, cached) {
  const copy = cached.catch((err) => {
    console.warn("ccmux sw: the shell cache is unreadable:", err);
    return undefined;
  });
  return new Promise((resolve) => {
    net.then(() => resolve(null), () => copy.then(resolve));
    setTimeout(() => copy.then((c) => { if (c) resolve(c); }), SLOW_MS);
  });
}

async function cacheFirst(req) {
  return (await fromShell(req)) || fetch(req, { cache: "no-cache" });
}

async function fromShell(req) {
  const build = await shellBuild();
  if (!build) return undefined;
  return (await caches.open(SHELL_PREFIX + build)).match(req, { ignoreSearch: true });
}

async function shellBuild() {
  const resp = await (await caches.open(NAV_CACHE)).match(SHELL_BUILD);
  return resp ? resp.text() : "";
}

// buildOf reads the build a response belongs to from its ETag (web.go).
function buildOf(resp) {
  return (resp.headers.get("ETag") || "").replace(/^W\//, "").replace(/"/g, "");
}

// refreshShell makes the cache hold the build the daemon serves now, whole:
// every shell file, each stamped with that one build, or nothing new at all.
// A deploy landing mid-refresh stamps some files with the next build; the set
// is then dropped, and the next page load tries again.
async function refreshShell() {
  try {
    const build = buildOf(await fetch("/build.js", { cache: "no-cache" }));
    if (!build || build === await shellBuild()) return;
    const resps = await Promise.all(SHELL.map((p) => fetch(p, { cache: "no-cache" })));
    if (resps.some((r) => !r.ok || buildOf(r) !== build)) return;
    const name = SHELL_PREFIX + build;
    const cache = await caches.open(name);
    await Promise.all(SHELL.map((p, i) => cache.put(p, resps[i])));
    await (await caches.open(NAV_CACHE)).put(SHELL_BUILD, new Response(build));
    const stale = (await caches.keys()).filter((k) => k.startsWith(SHELL_PREFIX) && k !== name);
    await Promise.all(stale.map((k) => caches.delete(k)));
  } catch (err) {
    console.warn("ccmux sw: the shell cache was not refreshed; it keeps the build it has:", err);
  }
}

// A push arrives even with the PWA closed and the phone locked. The payload is
// the daemon's pushPayload JSON; tag = workspace id so a newer notification
// replaces an older one for the same session rather than stacking.
self.addEventListener("push", (e) => {
  let d = {};
  try {
    d = e.data ? e.data.json() : {};
  } catch (_) {
    d = { title: "ccmux", body: e.data ? e.data.text() : "" };
  }
  const title = d.title || "ccmux";
  e.waitUntil(
    self.registration.showNotification(title, {
      body: d.body || "",
      tag: d.tag || undefined,
      renotify: true,
      icon: "/icons/icon-192.png",
      badge: "/icons/badge-72.png",
      data: { url: d.url || "/" },
    })
  );
});

// Tapping the notification deep-links to the workspace. iOS PWAs are unreliable
// here — a backgrounded page may be frozen and drop a postMessage — so we cover
// every case: (1) stash the target in NAV_CACHE, which the page reads when it
// regains visibility (survives a dropped message or a relaunch); (2) postMessage
// the chosen client as the fast path; (3) focus it, else open a new window at
// the deep-link URL (its ?w=&ws= is consumed on boot).
//
// The chosen client is the browser window showing the session's window, so a
// tap moves only that one. Before per-window pages every client was told, and
// every open window jumped to the session.
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(routeTap(url));
});

async function windowOfTap(url) {
  try {
    const ws = new URL(url, self.location.origin).searchParams.get("ws");
    const resp = await (await caches.open(NAV_CACHE)).match(WS_WINDOWS);
    const map = resp ? await resp.json() : {};
    return (ws && map[ws]) || "";
  } catch (e) {
    console.warn("ccmux sw: window map unreadable; the tap goes to the focused window:", e);
    return "";
  }
}

async function routeTap(url) {
  const w = await windowOfTap(url);
  const target = new URL(url, self.location.origin);
  if (w) target.searchParams.set("w", w);
  const href = target.pathname + target.search;
  try {
    const cache = await caches.open(NAV_CACHE);
    // A plain URL, as before per-window pages, with t for its age: a page
    // still running older code reads only its ws and is not confused by it.
    await cache.put(PENDING_NAV, new Response(`${href}${href.includes("?") ? "&" : "?"}t=${Date.now()}`));
  } catch (e) {
    console.warn("ccmux sw: could not stash the tap; a page that misses the message will not open it:", e);
  }
  const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const shows = (c) => { try { return new URL(c.url).searchParams.get("w") === w; } catch (_) { return false; } };
  const client = (w && wins.find(shows)) || wins.find((c) => c.focused) || wins[0];
  if (client) {
    client.postMessage({ type: "ccmux-navigate", url: href });
    if ("focus" in client) await client.focus();
    return;
  }
  if (self.clients.openWindow) await self.clients.openWindow(href);
}

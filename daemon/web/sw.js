// ccmux service worker: makes the web lens installable (a cached app shell)
// and receives Web Push notifications. It never caches the API or WebSocket —
// live session data must always hit the network.
"use strict";

const CACHE = "ccmux-shell-v33"; // v33: the shell comes from the network first, so a deploy lands on the next load
// A pending deep-link target, stashed on notificationclick so a client that was
// frozen/backgrounded (and may drop the postMessage) can still pick it up when it
// regains visibility. Kept in its own cache so shell-cache cleanup never purges it.
const NAV_CACHE = "ccmux-nav";
const PENDING_NAV = "/__ccmux_pending_nav";
// ws id -> window id, written by every page (pagewin.js) so a tap can find the
// browser window that shows the session's window.
const WS_WINDOWS = "/__ccmux_ws_windows";

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
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(
        // Purge only superseded shell caches; keep NAV_CACHE (pending deep-link).
        keys.filter((k) => k.startsWith("ccmux-shell-") && k !== CACHE).map((k) => caches.delete(k))
      ))
      .then(() => self.clients.claim())
  );
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
// is then ALL cache or ALL network. Cut over per file, a slow link would hand
// a page the new tiny build.js beside the old big app.js, and that page would
// run old code while believing it is current, so update.js would never move
// it on. A page served from the cache takes every file from the cache, and
// learns from /v1/health that it is behind.
const SLOW_MS = 3000;
const cachedPages = new Set(); // client ids of pages that booted from the cache

self.addEventListener("fetch", (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== location.origin || url.pathname.startsWith("/v1/")) {
    return; // default network handling
  }
  if (req.mode === "navigate") e.respondWith(pageFetch(req, e.resultingClientId));
  else if (cachedPages.has(e.clientId)) e.respondWith(cacheFirst(req));
  else e.respondWith(networkFirst(req));
});

async function pageFetch(req, clientId) {
  const cache = await caches.open(CACHE);
  const net = fromNetwork(cache, req);
  const copy = await slowOrFailed(net, cache.match(req, { ignoreSearch: true }));
  if (!copy) return net; // the network answered first, or there is nothing else
  if (clientId) cachedPages.add(clientId);
  return copy;
}

// slowOrFailed settles with the cached copy when the network fails or has not
// answered in SLOW_MS, and with null when the network answers first.
function slowOrFailed(net, cached) {
  return new Promise((resolve) => {
    net.then(() => resolve(null), () => cached.then(resolve));
    setTimeout(() => cached.then((copy) => { if (copy) resolve(copy); }), SLOW_MS);
  });
}

async function networkFirst(req) {
  const cache = await caches.open(CACHE);
  try {
    return await fromNetwork(cache, req);
  } catch (err) {
    const copy = await cache.match(req, { ignoreSearch: true }); // offline
    if (copy) return copy;
    throw err;
  }
}

async function cacheFirst(req) {
  const cache = await caches.open(CACHE);
  return (await cache.match(req, { ignoreSearch: true })) || fromNetwork(cache, req);
}

function fromNetwork(cache, req) {
  return fetch(req, { cache: "no-cache" }).then((resp) => {
    if (resp.ok && resp.type === "basic") cache.put(req, resp.clone());
    return resp;
  });
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

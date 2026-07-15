// ccmux service worker: makes the web lens installable (cache-first app shell)
// and receives Web Push notifications. It never caches the API or WebSocket —
// live session data must always hit the network.
"use strict";

const CACHE = "ccmux-shell-v3";
// A pending deep-link target, stashed on notificationclick so a client that was
// frozen/backgrounded (and may drop the postMessage) can still pick it up when it
// regains visibility. Kept in its own cache so shell-cache cleanup never purges it.
const NAV_CACHE = "ccmux-nav";
const PENDING_NAV = "/__ccmux_pending_nav";

// The app shell: everything needed to boot the lens offline. Session bytes are
// never cached (they come over /v1/attach, which the fetch handler leaves alone).
const SHELL = [
  "/",
  "/index.html",
  "/app.js",
  "/push.js",
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

// Stale-while-revalidate for the shell; network-only for the API (/v1/*) and
// anything that isn't a same-origin GET. Serving cache-first keeps the lens
// instant and offline-capable, but we ALSO refresh the cache from the network in
// the background so a deployed shell change (app.js/style.css/push.js) lands on
// the next load — a fixed cache name alone would otherwise pin the old assets
// forever. WebSocket upgrades never reach fetch, so /v1/attach and /v1/events are
// unaffected either way.
self.addEventListener("fetch", (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== location.origin || url.pathname.startsWith("/v1/")) {
    return; // default network handling
  }
  e.respondWith(
    caches.open(CACHE).then(async (cache) => {
      const cached = await cache.match(req, { ignoreSearch: true });
      const fresh = fetch(req)
        .then((resp) => {
          if (resp.ok && resp.type === "basic") cache.put(req, resp.clone());
          return resp;
        })
        .catch(() => cached); // offline → fall back to whatever we have
      return cached || fresh; // instant from cache, refreshed in the background
    })
  );
});

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
// every client as the fast path; (3) focus an existing window, else open a new
// one at the deep-link URL (its ?ws= is consumed on boot).
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(
    (async () => {
      try {
        const cache = await caches.open(NAV_CACHE);
        await cache.put(PENDING_NAV, new Response(url));
      } catch (_) {}
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const c of wins) c.postMessage({ type: "ccmux-navigate", url });
      for (const c of wins) {
        if ("focus" in c) { await c.focus(); return; }
      }
      if (self.clients.openWindow) await self.clients.openWindow(url);
    })()
  );
});

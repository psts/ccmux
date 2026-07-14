// ccmux service worker: makes the web lens installable (cache-first app shell)
// and receives Web Push notifications. It never caches the API or WebSocket —
// live session data must always hit the network.
"use strict";

const CACHE = "ccmux-shell-v1";

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
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// Cache-first for the shell; network-only for the API (/v1/*) and anything that
// isn't a same-origin GET. WebSocket upgrades never reach fetch, so /v1/attach
// and /v1/events are unaffected either way.
self.addEventListener("fetch", (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== "GET" || url.origin !== location.origin || url.pathname.startsWith("/v1/")) {
    return; // default network handling
  }
  e.respondWith(
    caches.match(req, { ignoreSearch: true }).then((hit) => {
      if (hit) return hit;
      return fetch(req).then((resp) => {
        if (resp.ok && resp.type === "basic") {
          const copy = resp.clone();
          caches.open(CACHE).then((c) => c.put(req, copy));
        }
        return resp;
      });
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

// Tapping the notification deep-links to the workspace: focus an existing lens
// window (asking it to navigate) or open a new one at the deep link.
self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const url = (e.notification.data && e.notification.data.url) || "/";
  e.waitUntil(
    (async () => {
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const c of wins) {
        if ("focus" in c) {
          await c.focus();
          c.postMessage({ type: "ccmux-navigate", url });
          return;
        }
      }
      if (self.clients.openWindow) await self.clients.openWindow(url);
    })()
  );
});

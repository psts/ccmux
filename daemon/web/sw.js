// ccmux service worker: receives Web Push notifications and routes a tap to
// the right browser window. It caches nothing of the lens: every load asks the
// daemon, so a deploy lands on the very next load and a page never mixes two
// builds. The daemon tags each file with its build (web.go), so an unchanged
// file costs a 304, not the bytes. With the daemon out of reach the browser
// says so; without the daemon the lens has nothing to show anyway.
"use strict";

// A pending deep-link target, stashed on notificationclick so a client that was
// frozen/backgrounded (and may drop the postMessage) can still pick it up when it
// regains visibility.
const NAV_CACHE = "ccmux-nav";
const PENDING_NAV = "/__ccmux_pending_nav";
// ws id -> window id, written by every page (pagewin.js) so a tap can find the
// browser window that shows the session's window.
const WS_WINDOWS = "/__ccmux_ws_windows";
// Earlier workers kept the lens in caches named ccmux-shell-*; nothing reads
// them now.
const OLD_SHELL_PREFIX = "ccmux-shell-";

self.addEventListener("install", () => self.skipWaiting());

self.addEventListener("activate", (e) => {
  e.waitUntil(dropOldShells().then(() => self.clients.claim()));
});

async function dropOldShells() {
  const old = (await caches.keys()).filter((k) => k.startsWith(OLD_SHELL_PREFIX));
  await Promise.all(old.map((k) => caches.delete(k)));
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

// ccmux push client: registers the service worker (makes the lens installable),
// drives the notification opt-in/settings UI, and handles the deep-link when a
// notification is tapped. Kept separate from app.js — the terminal works with or
// without notifications.
"use strict";

(function () {
  const swSupported = "serviceWorker" in navigator;
  const pushSupported = swSupported && "PushManager" in window && "Notification" in window;
  let reg = null;

  async function boot() {
    if (swSupported) {
      try {
        reg = await navigator.serviceWorker.register("/sw.js");
      } catch (e) {
        console.warn("[ccmux] service worker registration failed", e);
      }
      navigator.serviceWorker.addEventListener("message", onSWMessage);
    }
    wireSettings();
  }

  // Notification tap → the SW asks an open window to deep-link to the workspace.
  function onSWMessage(ev) {
    const m = ev.data || {};
    if (m.type === "ccmux-navigate" && m.url) {
      const ws = new URL(m.url, location.origin).searchParams.get("ws");
      if (ws && window.ccmux) window.ccmux.attach(ws, null);
    }
  }

  // --- settings sheet ---
  function wireSettings() {
    const open = document.getElementById("open-settings");
    const modal = document.getElementById("settings-modal");
    const close = document.getElementById("settings-close");
    if (open) open.onclick = () => { modal.classList.remove("hidden"); refreshState(); };
    if (close) close.onclick = () => modal.classList.add("hidden");
    if (modal) modal.onclick = (e) => { if (e.target === modal) modal.classList.add("hidden"); };
    const enable = document.getElementById("notif-enable");
    const disable = document.getElementById("notif-disable");
    if (enable) enable.onclick = enableNotifications;
    if (disable) disable.onclick = disableNotifications;
  }

  const isStandalone = () =>
    window.matchMedia("(display-mode: standalone)").matches || window.navigator.standalone === true;
  const isIOS = () => /iphone|ipad|ipod/i.test(navigator.userAgent);

  async function refreshState() {
    const status = document.getElementById("notif-status");
    const iosHint = document.getElementById("ios-hint");
    const enable = document.getElementById("notif-enable");
    const disable = document.getElementById("notif-disable");
    enable.classList.add("hidden");
    disable.classList.add("hidden");
    iosHint.classList.add("hidden");

    if (!pushSupported) {
      status.textContent = "Push notifications aren't supported in this browser.";
      return;
    }
    // iOS only delivers push to a Home-Screen install (a standalone PWA).
    if (isIOS() && !isStandalone()) {
      status.textContent = "Add ccmux to your Home Screen to turn on notifications.";
      iosHint.textContent = "Tap the Share button, choose “Add to Home Screen”, then open ccmux from the new icon and come back here.";
      iosHint.classList.remove("hidden");
      return;
    }
    if (Notification.permission === "denied") {
      status.textContent = "Notifications are blocked. Re-enable them for this site in your browser settings.";
      return;
    }
    const sub = reg && (await reg.pushManager.getSubscription());
    if (sub) {
      status.textContent = "Notifications are on for this device.";
      disable.classList.remove("hidden");
    } else {
      status.textContent = "Get a push when a session needs you — even with ccmux closed.";
      enable.classList.remove("hidden");
    }
  }

  function user() {
    return (window.ccmux && window.ccmux.getUser && window.ccmux.getUser()) || "anon";
  }

  async function enableNotifications() {
    try {
      if (!reg) reg = await navigator.serviceWorker.ready;
      const perm = await Notification.requestPermission();
      if (perm !== "granted") return refreshState();

      const { key } = await (await fetch("/v1/push/vapid")).json();
      const sub = await reg.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: urlB64ToUint8Array(key),
      });
      const r = await fetch("/v1/push/subscriptions?user=" + encodeURIComponent(user()), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(sub.toJSON()),
      });
      if (!r.ok) console.warn("[ccmux] subscription POST failed", r.status);
    } catch (e) {
      console.warn("[ccmux] enabling notifications failed", e);
    }
    refreshState();
  }

  async function disableNotifications() {
    try {
      const sub = reg && (await reg.pushManager.getSubscription());
      if (sub) {
        await fetch("/v1/push/subscriptions", {
          method: "DELETE",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ endpoint: sub.endpoint }),
        });
        await sub.unsubscribe();
      }
    } catch (e) {
      console.warn("[ccmux] disabling notifications failed", e);
    }
    refreshState();
  }

  // VAPID keys travel as base64url; PushManager wants a Uint8Array.
  function urlB64ToUint8Array(base64) {
    const pad = "=".repeat((4 - (base64.length % 4)) % 4);
    const b64 = (base64 + pad).replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(b64);
    const arr = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i++) arr[i] = raw.charCodeAt(i);
    return arr;
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
  else boot();
})();

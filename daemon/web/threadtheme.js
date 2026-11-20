// threadtheme.js: light or dark for the thread page. The person picks
// System, Light or Dark with the lightbulb at the sidebar's top (a click
// moves to the next); the choice is kept in this
// browser only (localStorage), and System follows the device's setting,
// live. The page's colours are tokens in threads.css; this file only sets
// <html data-theme="light|dark">, which picks the set. It runs in <head>,
// before the page draws, so a dark page never flashes light.
//
// read, resolve and save are plain functions, so threadtheme_test.js runs
// them without a browser.
(function (root) {
  "use strict";

  const KEY = "ccmux.threads.theme";
  const CHOICES = ["system", "light", "dark"];

  // read is the saved choice; no storage, storage that throws, or a value
  // that is not a choice all read as System.
  function read(storage) {
    try {
      const v = storage ? storage.getItem(KEY) : null;
      return CHOICES.includes(v) ? v : "system";
    } catch {
      return "system";
    }
  }

  // resolve is the theme a choice shows on a device that prefers dark or not.
  function resolve(choice, prefersDark) {
    return choice === "system" ? (prefersDark ? "dark" : "light") : choice;
  }

  // save keeps a choice in this browser. A browser that refuses storage
  // still switches; the choice is gone at the next load.
  function save(storage, choice) {
    try {
      storage.setItem(KEY, CHOICES.includes(choice) ? choice : "system");
    } catch {
      // private window or blocked site data: nothing to keep it in
    }
  }

  // next is the choice one click on the lightbulb moves to, round the
  // three; LABELS say each one on the button.
  const next = (choice) => CHOICES[(CHOICES.indexOf(choice) + 1) % CHOICES.length];
  const LABELS = { system: "System", light: "Light", dark: "Dark" };
  const label = (choice) => `Theme: ${LABELS[choice] || LABELS.system} (click for ${LABELS[next(choice)] || LABELS.light})`;

  const api = { KEY, read, resolve, save, next, label };
  if (typeof module === "object" && module.exports) {
    module.exports = api;
    return;
  }

  // In the browser: set the theme now, follow the device while the choice is
  // System, and wire the sidebar's picker once the page is there.
  const storage = (() => { try { return root.localStorage; } catch { return null; } })();
  const media = root.matchMedia ? root.matchMedia("(prefers-color-scheme: dark)") : null;
  let choice = read(storage);
  // The fake browser threads_test.js boots the page in has no <html> and
  // no load event; the theme is not what it tests.
  const doc = root.document;
  const apply = () => {
    const theme = resolve(choice, !!(media && media.matches));
    if (doc.documentElement) doc.documentElement.dataset.theme = theme;
    const meta = doc.querySelector ? doc.querySelector('meta[name="theme-color"]') : null;
    if (meta) meta.content = theme === "dark" ? "#1F1F1D" : "#EDEDE8";
  };
  apply();
  if (media && media.addEventListener) media.addEventListener("change", apply);
  if (doc.addEventListener) doc.addEventListener("DOMContentLoaded", () => {
    const bulb = doc.getElementById("theme");
    if (!bulb) return;
    const say = () => { bulb.title = label(choice); bulb.setAttribute("aria-label", label(choice)); bulb.dataset.choice = choice; };
    say();
    bulb.onclick = () => { choice = next(choice); save(storage, choice); apply(); say(); };
  });
  root.ThreadTheme = api;
})(typeof window !== "undefined" ? window : globalThis);

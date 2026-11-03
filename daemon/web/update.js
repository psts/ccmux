// update.js: moves an open page onto a newer web lens after a deploy.
//
// The page knows the build it booted with (window.ccmuxBuild, from /build.js)
// and asks the daemon which build it serves now (/v1/health "web"). When they
// differ, a page nobody is using reloads on its own, and a page in use shows a
// bar with a Reload button, because a reload there could throw away what the
// person is in the middle of.
//
// "Nobody is using it" means the page has just come back into view (the phone
// app reopened, the tab switched back to) with no dialog open and no unsent
// chat text. A page that is hidden waits for that moment. At boot the page
// never reloads itself: it was just fetched, so a mismatch then means a deploy
// landed mid-load, and the bar is enough.
(function (root) {
  "use strict";
  const CHECK_MS = 60000;

  // decide is the whole rule: "none", "reload" or "bar". mine is the build
  // this page runs, served the one the daemon serves; either may be unknown
  // (a page from before builds, a daemon that did not answer).
  function decide(mine, served, { returning, busy, hidden }) {
    if (!mine || !served || mine === served) return "none";
    if (hidden) return "none"; // it is checked again when it comes back
    return returning && !busy ? "reload" : "bar";
  }

  // versionLine is the tooltip on the "ccmux" logo: which version the daemon
  // runs, and whether this page is still on an older lens than it serves.
  // health is /v1/health's answer, or null when the daemon did not answer.
  function versionLine(health, mine) {
    if (!health) return "ccmux: version unknown, the daemon is not answering";
    const v = health.version ? (/^\d/.test(health.version) ? "v" + health.version : health.version) : "unknown";
    const stale = mine && health.web && mine !== health.web;
    return `ccmux ${v}` + (stale ? "\nThis page is older than that: reload to update" : "");
  }

  const api = { decide, versionLine };
  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
    return;
  }
  root.ccmuxUpdate = api;
  const doc = root.document;
  let dismissed = ""; // the build whose bar the person closed

  function busy() {
    if (doc.querySelector(".modal:not(.hidden)")) return true;
    return [...doc.querySelectorAll(".ac-compose textarea")].some((t) => t.value.trim() !== "");
  }

  async function health() {
    try {
      const r = await fetch("/v1/health", { cache: "no-store" });
      return r.ok ? await r.json() : null;
    } catch (_) {
      return null; // the daemon is away; the next check asks again
    }
  }

  async function check(returning) {
    const h = await health();
    const logo = doc.querySelector(".logo");
    if (logo) logo.title = versionLine(h, root.ccmuxBuild);
    const build = (h && h.web) || "";
    const what = decide(root.ccmuxBuild, build, { returning, busy: busy(), hidden: doc.visibilityState === "hidden" });
    if (what === "reload") root.location.reload();
    else if (what === "bar" && build !== dismissed) showBar(build);
  }

  function showBar(build) {
    if (doc.getElementById("update-bar")) return;
    const bar = doc.createElement("div");
    bar.id = "update-bar";
    bar.innerHTML = `<span>ccmux has a new version.</span><button class="update-reload">Reload</button>` +
      `<button class="update-dismiss" title="Not now">&times;</button>`;
    bar.querySelector(".update-reload").onclick = () => root.location.reload();
    bar.querySelector(".update-dismiss").onclick = () => { dismissed = build; bar.remove(); };
    doc.body.appendChild(bar);
  }

  doc.addEventListener("visibilitychange", () => {
    if (doc.visibilityState === "visible") check(true);
  });
  root.setInterval(() => check(false), CHECK_MS);
  check(false);
})(typeof window !== "undefined" ? window : this);

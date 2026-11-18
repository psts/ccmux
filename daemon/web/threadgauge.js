// threadgauge.js: the header's gauge on the thread page. How full the
// session's context window is (the runner asks the SDK after each turn) and
// how long its prompt cache has left: the cache lives an hour (or five
// minutes) from the last reply, and a message after that re-reads the whole
// conversation at full price. Pure, so threadgauge_test.js drives it.
(function (root) {
  "use strict";

  const MIN = 60000;

  // left is a time to come as "46 min" or "<1 min".
  const left = (ms) => (ms < MIN ? "<1 min" : Math.ceil(ms / MIN) + " min");
  // since is a time gone as "12 min" or "2 h 5 min".
  function since(ms) {
    const m = Math.max(0, Math.round(ms / MIN));
    return m < 60 ? m + " min" : Math.floor(m / 60) + " h" + (m % 60 ? " " + (m % 60) + " min" : "");
  }
  const k = (n) => Math.round(n / 1000) + "k";

  // contextPart is the window's share in use, orange from 80%.
  function contextPart(c) {
    if (!c || !c.max) return null;
    return { text: c.percent + "%", cls: c.percent >= 80 ? "warn" : "", title: `Context: ${k(c.used)} of ${k(c.max)} tokens (${c.percent}%)` };
  }

  // cachePart counts the cache down from the last reply: orange in its last
  // five minutes (its last minute for a five-minute cache), cold after.
  function cachePart(r, now) {
    if (!r.cacheTtlMs || !r.endedAt) return null;
    const rest = r.endedAt + r.cacheTtlMs - now, life = r.cacheTtlMs >= 60 * MIN ? "1 hour" : since(r.cacheTtlMs);
    const head = `Prompt cache: ${life}, last reply ${since(now - r.endedAt)} ago`;
    if (rest <= 0) return { text: "cache cold", cls: "cold", title: `${head}, expired ${since(-rest)} ago: the next message re-reads the conversation at full price` };
    const warnAt = Math.min(5 * MIN, r.cacheTtlMs / 5);
    return { text: "cache " + left(rest), cls: rest < warnAt ? "warn" : "", title: `${head}, expires in ${left(rest)}` };
  }

  // gauge is what the header shows for a turn's result at now (ms), or null
  // when nothing is known yet.
  function gauge(r, now) {
    if (!r) return null;
    const c = contextPart(r.context), m = cachePart(r, now);
    if (!c && !m) return null;
    const parts = [c, m].filter(Boolean);
    return {
      text: parts.map((p) => p.text).join(" · "),
      title: parts.map((p) => p.title).join("\n"),
      ctx: c ? c.cls : "", cache: m ? m.cls : "",
      parts: parts.map((p) => ({ text: p.text, cls: p.cls })),
    };
  }

  const api = { gauge };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadGauge = api;
})(typeof window !== "undefined" ? window : this);

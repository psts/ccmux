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
  // five minutes (its last minute for a five-minute cache), cold after, and
  // live while a turn runs.
  function cachePart(r, now, busy) {
    if (!r.cacheTtlMs || !r.endedAt) return null;
    if (busy) return { text: "cache live", cls: "", title: "Prompt cache: in use by the running turn, so it stays warm" };
    const rest = r.endedAt + r.cacheTtlMs - now, life = r.cacheTtlMs >= 60 * MIN ? "1 hour" : since(r.cacheTtlMs);
    const head = `Prompt cache: ${life}, last reply ${since(now - r.endedAt)} ago`;
    if (rest <= 0) return { text: "cache cold", cls: "cold", title: `${head}, expired ${since(-rest)} ago: the next message re-reads the conversation at full price` };
    const warnAt = Math.min(5 * MIN, r.cacheTtlMs / 5);
    return { text: "cache " + left(rest), cls: rest < warnAt ? "warn" : "", title: `${head}, expires in ${left(rest)}` };
  }

  // gauge is what the header shows for a turn's result at now (ms), or null
  // when nothing is known yet. busy: a turn is running, which keeps the
  // cache warm with every call it makes.
  function gauge(r, now, busy) {
    if (!r) return null;
    const c = contextPart(r.context), m = cachePart(r, now, busy);
    if (!c && !m) return null;
    const parts = [c, m].filter(Boolean);
    return {
      text: parts.map((p) => p.text).join(" · "),
      title: parts.map((p) => p.title).join("\n"),
      ctx: c ? c.cls : "", cache: m ? m.cls : "",
      parts: parts.map((p) => ({ text: p.text, cls: p.cls })),
    };
  }

  // ASK_FROM is the smallest conversation worth asking about: below it a
  // cold cache costs little, and the question would only be in the way.
  const ASK_FROM = 20000;

  // coldAsk is what the composer asks before a message on a cold cache:
  // null when the cache is warm, unknown or in use, or the conversation is
  // small; else its size ({used, max, percent} when known) and how long ago
  // the cache went cold. Cold is cachePart's own rule, so the gauge and the
  // question never disagree.
  function coldAsk(r, now, busy) {
    const m = r ? cachePart(r, now, busy) : null;
    if (!m || m.cls !== "cold") return null;
    const c = r.context && r.context.max ? r.context : null;
    if (c && c.used < ASK_FROM) return null;
    return { used: c ? c.used : 0, max: c ? c.max : 0, percent: c ? c.percent : 0, coldFor: since(now - r.endedAt - r.cacheTtlMs) };
  }

  // coldText is the question's words for what coldAsk found.
  function coldText(a) {
    const size = a.max ? `${k(a.used)} tokens (${a.percent}% of the context window)` : "the whole conversation";
    return `The prompt cache went cold ${a.coldFor} ago. Continuing re-reads ${size} at full price. A clean start opens a new conversation; the old one stays on screen above it.`;
  }

  const api = { gauge, coldAsk, coldText };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadGauge = api;
})(typeof window !== "undefined" ? window : this);

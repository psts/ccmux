// threadearlier.js: the conversations a session's clean starts left, shown
// above the one it runs now on the thread page. A clean start (the composer
// asks when the prompt cache has gone cold) moves the session onto a new
// conversation; the row lists the ones it left (leftConversations), and
// each is read once (GET /v1/sessions/{id}/conversations/{conv}) and kept
// for the page's life. Each ends in a line that says a clean one began.
// Read only: an earlier message cannot be edited, since Claude is no
// longer in that conversation.
(function (root) {
  "use strict";

  // SHOWN is how many of the newest left conversations are drawn.
  const SHOWN = 3;

  const pad = (n) => String(n).padStart(2, "0");

  // when is a time as the divider says it: "14:05", or "2026-11-20 14:05"
  // on another day than today.
  function when(ms, now) {
    const d = new Date(ms), t = new Date(now);
    const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
    return d.toDateString() === t.toDateString() ? hm : `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hm}`;
  }

  // shown is the part of a session's list that is drawn, oldest first.
  const shown = (left) => (left || []).slice(-SHOWN);

  // dividerText is the line after a left conversation.
  const dividerText = (c, now) => `New clean conversation, started ${when(c.at, now)}. Claude does not see what is above.`;

  // blocks is what goes above the conversation on screen: each shown left
  // conversation's blocks (from toBlocks), marked old and named apart, then
  // its divider. One not read yet is its divider alone, and one that could
  // not be read says why; a note says how many older ones are not drawn.
  function blocks(left, turnsOf, toBlocks, now) {
    const list = left || [], out = [];
    if (list.length > SHOWN) out.push({ kind: "note", id: "earlier:more", text: `${list.length - SHOWN} older conversation${list.length - SHOWN === 1 ? "" : "s"} not shown.`, level: "info" });
    for (const c of shown(list)) {
      const got = turnsOf(c.id);
      if (got && got.error) out.push({ kind: "note", id: "unread:" + c.id, text: `This earlier conversation could not be read (${got.error}). Reload the page to try again.`, level: "warning" });
      else for (const b of toBlocks(got || [])) out.push({ ...b, id: `old:${c.id}:${b.id}`, old: true });
      out.push({ kind: "divider", id: "div:" + c.id, text: dividerText(c, now) });
    }
    return out;
  }

  // create wires it to the page: d.call is its fetch, d.snapshot(frame)
  // turns a snapshot frame into a stream (threadstream.js), d.changed()
  // redraws.
  function create(d) {
    const turns = new Map(); // "session|conv" → turns, or null while being read
    const key = (sid, conv) => sid + "|" + conv;

    // seed keeps turns already on screen for a conversation being left,
    // so it does not blink out while the session starts again.
    function seed(sid, conv, list) {
      if (sid && conv && !turns.get(key(sid, conv))) turns.set(key(sid, conv), list || []);
    }

    // load reads the shown left conversations of sess not read yet. One
    // that cannot be read keeps the reason, which blocks shows; a reload
    // of the page asks again.
    function load(sess) {
      if (!sess) return;
      for (const c of shown(sess.leftConversations)) {
        const k = key(sess.id, c.id);
        if (turns.has(k)) continue;
        turns.set(k, null);
        d.call("GET", `/v1/sessions/${encodeURIComponent(sess.id)}/conversations/${encodeURIComponent(c.id)}`)
          .then((frame) => { turns.set(k, d.snapshot(frame).turns || []); d.changed(); })
          .catch((e) => { turns.set(k, { error: (e && e.message) || "no answer" }); d.changed(); });
      }
    }

    const turnsFor = (sid) => (conv) => turns.get(key(sid, conv));
    return { seed, load, turnsFor };
  }

  const api = { create, blocks, dividerText, when, SHOWN };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadEarlier = api;
})(typeof window !== "undefined" ? window : this);

// threadpast.js: an agent's earlier conversations on the thread page. A
// base that starts fresh opens a new conversation at every wake, so the
// session on screen holds only the latest; the ones before are still in
// Claude Code's store. "Earlier" in the header lists them
// (GET /v1/sessions/{id}/conversations) and one picked is shown read only
// in place of the current conversation (…/conversations/{conv}), with a bar
// that leads back. Nothing here changes what the agent runs or remembers.
(function (root) {
  "use strict";

  const pad = (n) => String(n).padStart(2, "0");

  // when is a conversation's time as the list writes it: "2026-11-17 09:38".
  function when(ms) {
    const d = new Date(ms);
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  // label is one entry of the list: its time, and its title cut short.
  function label(c) {
    const title = String(c.title || "").replace(/\s+/g, " ").trim();
    const cut = title.length > 48 ? title.slice(0, 47) + "…" : title;
    return when(c.updated) + (cut ? " · " + cut : "") + (c.current ? " (now)" : "");
  }

  // barText is what the bar over a past conversation says.
  const barText = (c) => `An earlier conversation, from ${when(c.updated)}. Read only: the agent is not in it now.`;

  // create wires it to the page: d.call is its fetch, d.sessionId() the
  // session on screen, d.changed() redraws, d.menu(items, ev) opens the
  // page's menu, d.fail(text) shows what went wrong, d.snapshot(frame) turns
  // a snapshot frame into a stream (threadstream.js).
  function create(d) {
    const st = { conv: null, stream: null, gen: 0 };

    const on = () => !!st.conv;
    const turns = () => (st.stream ? st.stream.turns : null);
    const key = () => (st.conv ? st.conv.id : "");
    const bar = () => (st.conv ? barText(st.conv) : "");

    // reset leaves the past: a new pick, or Back to now.
    function reset() {
      st.gen++;
      st.conv = st.stream = null;
    }

    function back() {
      reset();
      d.changed();
    }

    // open lists the session's conversations in the page's menu.
    async function open(ev) {
      const id = d.sessionId();
      if (!id) return;
      let got;
      try {
        got = await d.call("GET", `/v1/sessions/${encodeURIComponent(id)}/conversations`);
      } catch (e) {
        d.fail("Earlier conversations could not be read: " + e.message);
        return;
      }
      const list = got.conversations || [];
      if (!list.length) { d.fail("This agent has no earlier conversations."); return; }
      d.menu(list.map((c) => [label(c), () => (c.current ? back() : show(id, c))]), ev);
    }

    // show reads one conversation and puts it on screen; an answer for a
    // pick left since is dropped.
    async function show(id, c) {
      const gen = ++st.gen;
      let frame;
      try {
        frame = await d.call("GET", `/v1/sessions/${encodeURIComponent(id)}/conversations/${encodeURIComponent(c.id)}`);
      } catch (e) {
        if (gen === st.gen) d.fail("That conversation could not be read: " + e.message);
        return;
      }
      if (gen !== st.gen || d.sessionId() !== id) return;
      st.conv = c;
      st.stream = d.snapshot(frame);
      d.changed();
    }

    return { on, turns, key, bar, reset, back, open };
  }

  const api = { create, label, when, barText };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadPast = api;
})(typeof window !== "undefined" ? window : this);

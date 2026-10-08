// ccmux web lens — the attention board: every pane of one window that is
// waiting on a human, as a grid of tiles, answered in place. The Mac app's
// AttentionBoardView is the same feature; the rules below (which claims get
// a tile, in what order, how many, when the set freezes, when a tile leaves)
// are the same rules, kept in the same words in both lenses.
//
// A passive tile is a picture: the pane's screen as plain text, or an agent's
// last word, fetched over REST. A screen tile sizes its pane to the tile, so
// the text reads at a terminal's size however many tiles share the board; the
// workspace takes its own size back when it shows the pane again. Clicking a
// tile makes it ACTIVE: the lens's one live view (terminal or chat) moves
// into it and attaches, which takes over the pane's size like any lens that
// starts typing. Moving on from an active tile (another tile, Next, or Done)
// tells the daemon "acted", which retires that pane's claim everywhere.
(function () {
  "use strict";

  const SNOOZE_MS = 30 * 60 * 1000;
  const PREVIEW_EVERY_MS = 3000;
  const LIVE_IDS = ["harness-bar", "agent-chat-bar", "pane-notice", "terminal", "agent-chat"];

  // --- the rules (pure; smoke_test.js drives them) ---

  // A permission or a question, as against "your turn". A needs_input with no
  // reason comes from an agent too old to say, and every such signal was one
  // of the two.
  function isBlocked(c) { return c.state === "needs_input" && c.reason !== "finished"; }

  // Blocked first, then the longest wait (no recorded start counts as the
  // oldest), then by name so equal claims hold still.
  function claimOrder(a, b) {
    return (Number(isBlocked(b)) - Number(isBlocked(a))) ||
      ((a.since || 0) - (b.since || 0)) || a.name.localeCompare(b.name);
  }

  // How many tiles a board this wide shows: one on a phone, four on a
  // laptop, six on a big screen. The rest wait in the strip.
  function capFor(width) { return width < 700 ? 1 : width < 1300 ? 4 : 6; }

  function columnsFor(n) { return n <= 1 ? 1 : n <= 4 ? 2 : 3; }

  // A screen tile draws its text at the size the Mac's terminal does (12pt
  // Monaco). cell is one character's box at that size, measured in the
  // tile's font.
  const TILE_FONT = 12;
  // .board-preview's padding, both sides together (style.css).
  const PREVIEW_PAD_W = 16, PREVIEW_PAD_H = 12;

  // tileGrid: the pane size that fills a tile's text area at TILE_FONT, or
  // null for a tile too small to drive a pane to: a crushed pane is worse
  // than small text (app.js's MIN_COLS).
  function tileGrid(width, height, cell) {
    if (!(cell.w > 0 && cell.h > 0)) return null;
    const cols = Math.floor(width / cell.w), rows = Math.floor(height / cell.h);
    return cols >= 20 && rows >= 5 ? { cols, rows } : null;
  }

  // fontFor: the text size that shows a cols x rows screen whole in a tile:
  // TILE_FONT when the pane is the tile's size, smaller when another lens has
  // it bigger, never below 4.
  function fontFor(width, height, cols, rows, cell) {
    if (!(cols > 0 && cell.w > 0 && cell.h > 0)) return TILE_FONT;
    const across = width / (cols * cell.w);
    const down = rows > 0 ? height / (rows * cell.h) : 1;
    return Math.max(4, TILE_FONT * Math.min(1, across, down));
  }

  // sizeOutcome: a resize's HTTP status (0: no answer) as "sized", "refused"
  // or "failed". A 4xx is the daemon saying no for good (an unknown pane, an
  // older host without the route, a degraded one): asking again every tick
  // would only fill the log. A 5xx or no answer may pass.
  function sizeOutcome(status) {
    return status === 200 ? "sized" : status >= 400 && status < 500 ? "refused" : "failed";
  }

  // "Not now" hides a claim for 30 minutes, or until it changes: a new start
  // time is a new claim and comes straight back.
  function snoozedNow(snoozed, c, now) {
    const s = snoozed.get(c.pane);
    return !!s && s.since === c.since && s.until > now;
  }

  // layout picks the tiles. While a tile is active the set is frozen, so
  // nothing moves under the person typing: arrivals wait in the strip, and a
  // tile whose claim was dealt with elsewhere stays, marked handled, until
  // they move on.
  function layout(claims, opts) {
    const live = claims.filter((c) => !snoozedNow(opts.snoozed, c, opts.now)).sort(claimOrder);
    if (opts.active && opts.prevVisible.some((c) => c.pane === opts.active)) {
      const byPane = new Map(live.map((c) => [c.pane, c]));
      const shown = new Set(opts.prevVisible.map((c) => c.pane));
      return {
        visible: opts.prevVisible.map((c) => byPane.get(c.pane) || Object.assign({}, c, { handled: true })),
        waiting: live.filter((c) => !shown.has(c.pane)),
      };
    }
    return { visible: live.slice(0, opts.cap), waiting: live.slice(opts.cap) };
  }

  function reasonLabel(c) {
    if (c.handled) return "handled";
    if (c.reason === "permission") return "wants permission";
    if (c.reason === "question") return "asked a question";
    if (isBlocked(c)) return "needs you";
    return "your turn";
  }

  function waitedFor(since, now) {
    if (!since) return "";
    const min = Math.floor((now - since) / 60000);
    return min < 1 ? "just now" : min < 60 ? `${min} min` : `${Math.floor(min / 60)} h`;
  }

  // --- state ---

  const board = {
    win: "",            // the window (group name) on show, "" when closed
    active: null,       // pane id of the active tile
    visible: [],        // the claims on show, in order
    waiting: [],
    snoozed: new Map(), // pane -> {since, until}
    previews: new Map(), // pane -> {key, text, cols, rows}
    fetching: new Set(), // panes with a preview read in flight
    sized: new Map(),   // pane -> "COLSxROWS" its tile last sent it
    timer: null,
    gridKey: "",        // the tiles last drawn: their panes and the active one
  };

  // claimsIn lists the window's claiming panes from the firehose state, with
  // what a tile needs to draw them. Only panes the workspace still lists count.
  function claimsIn(win) {
    const out = [];
    for (const ws of state.workspaces) {
      if ((ws.group || "") !== win || ws.status !== "live") continue;
      const per = state.claims[ws.id] || {};
      for (const p of ws.panes || []) {
        const c = per[p.id];
        if (!c || (c.state !== "needs_input" && c.state !== "done")) continue;
        out.push({
          ws: ws.id, pane: p.id, state: c.state, reason: c.reason || "", since: c.since || 0,
          name: ws.name || ws.repoPath, chat: !!p.agent && hasChat(p.harness),
        });
      }
    }
    return out;
  }

  function count(win) {
    const now = Date.now();
    return claimsIn(win).filter((c) => !snoozedNow(board.snoozed, c, now)).length;
  }

  // --- open / close ---

  function open(win) {
    if (board.win === win) return;
    if (board.win) close();
    leaveWorkspace();
    board.win = win;
    state.board = win;
    document.getElementById("app").classList.add("board-mode");
    $("empty").style.display = "none";
    $("board").classList.remove("hidden");
    closeDrawer();
    reportBoardPresence();
    board.timer = setInterval(tick, PREVIEW_EVERY_MS);
    refresh();
    renderList();
  }

  function close() {
    if (!board.win) return;
    if (board.active) deactivate(true);
    clearInterval(board.timer);
    board.timer = null;
    board.win = "";
    state.board = "";
    board.visible = [];
    board.gridKey = "";
    board.sized.clear();
    document.getElementById("app").classList.remove("board-mode");
    $("board").classList.add("hidden");
    $("board").innerHTML = "";
    reportBoardPresence();
  }

  // The board holds no workspace: drop whatever was attached, without the
  // drawer-opening detachIfCurrent does for a closed session.
  function leaveWorkspace() {
    if (state.conn) { const c = state.conn; state.conn = null; c.close(); }
    state.wsId = null;
    state.paneId = null;
    state.panes = [];
    window.ccmuxAgentChat.hide();
    renderTabs();
  }

  // With no tile active the board holds no attach socket, and presence rides
  // the attach sockets on this lens, so a person at the board would read as
  // away and their phone would buzz at the desk. The firehose takes the same
  // presence frame: present while the board is open and visible.
  function reportBoardPresence() {
    const fh = state.firehose;
    if (!fh || fh.readyState !== 1) return;
    fh.send(JSON.stringify({ t: "focus", present: !!board.win && document.visibilityState === "visible" }));
  }

  // --- active tile ---

  function activate(pane) {
    if (board.active === pane) return;
    if (board.active) deactivate(true);
    const c = board.visible.find((x) => x.pane === pane) || board.waiting.find((x) => x.pane === pane);
    if (!c) return;
    if (!board.visible.some((x) => x.pane === pane)) board.visible = [c].concat(board.visible);
    board.active = pane;
    render();
    attach(c.ws, pane);
    // Ready to type. A chat focuses its own box when it shows (agentchat.js).
    if (!c.chat) setTimeout(() => { if (board.active === pane && state.term) state.term.focus(); }, 300);
  }

  // deactivate ends the active tile. acted says the person dealt with it:
  // the daemon retires that pane's claim, on every lens.
  function deactivate(acted) {
    const pane = board.active;
    if (!pane) return;
    const c = board.visible.find((x) => x.pane === pane);
    if (acted && c) sendActed(c);
    board.active = null;
    parkLiveView();
    leaveWorkspace();
  }

  // sendActed rides the active tile's attach socket. Once it has gone out
  // the claim is dropped here at once, rather than when the daemon's idle
  // comes back on the firehose, so moving on never leaves the old tile behind.
  // A socket that is not open (still dialing, mid-reconnect) sends nothing,
  // and the claim stays: the tile is still there to be dealt with, on this
  // lens as on every other. Same rule as the Mac's boardActed.
  function sendActed(c) {
    if (!state.conn || state.conn.readyState !== 1 || state.paneId !== c.pane) {
      console.warn("board: not connected to", c.name, "- its tile stays until it can be retired");
      return;
    }
    send({ t: "acted", pane: c.pane });
    noteAttention(c.ws, c.pane, "idle", "", 0);
  }

  // next moves on: the active tile is done with, and the next waiting one
  // (if any) becomes active.
  function next() {
    const done = board.active;
    deactivate(true);
    refresh();
    const c = board.visible.find((x) => x.pane !== done && !x.handled);
    if (c) activate(c.pane);
  }

  function snooze(c) {
    board.snoozed.set(c.pane, { since: c.since, until: Date.now() + SNOOZE_MS });
    if (board.active === c.pane) deactivate(false);
    refresh();
    renderList();
  }

  // The lens has ONE live view (terminal, chat, their bars). The active tile
  // borrows it; parking puts it back in #main, where the board hides it.
  function hostLiveView(body) {
    for (const id of LIVE_IDS) body.appendChild($(id));
    scheduleFit();
  }

  function parkLiveView() {
    const main = $("main");
    for (const id of LIVE_IDS) main.insertBefore($(id), $("empty"));
  }

  // --- rendering ---

  // refresh recomputes the tiles. The grid is redrawn only when its tiles
  // changed: a redraw re-parents the live view, and moving a focused element
  // drops its focus, so an arrival while someone types touches only the strip
  // and the headers. Called on every attention change.
  function refresh() {
    if (!board.win) return;
    const out = layout(claimsIn(board.win), {
      cap: capFor($("main").clientWidth || window.innerWidth),
      active: board.active, prevVisible: board.visible, snoozed: board.snoozed, now: Date.now(),
    });
    board.visible = out.visible;
    board.waiting = out.waiting;
    forgetSizes();
    if (gridKey() !== board.gridKey) { render(); return; }
    const strip = $("board").querySelector(".board-strip");
    if (strip) strip.replaceWith(stripEl());
    updateHeaders();
  }

  function gridKey() { return board.visible.map((c) => c.pane).join(",") + "|" + board.active; }

  function render() {
    board.gridKey = gridKey();
    if (board.active) parkLiveView();
    const root = $("board");
    root.innerHTML = "";
    root.appendChild(stripEl());
    if (!board.visible.length) {
      root.appendChild(el("div", "board-empty", "Nothing needs you in " + board.win + "."));
      return;
    }
    const grid = el("div", "board-grid");
    grid.style.gridTemplateColumns = `repeat(${columnsFor(board.visible.length)}, minmax(0, 1fr))`;
    for (const c of board.visible) grid.appendChild(tileEl(c));
    root.appendChild(grid);
    for (const c of board.visible) if (c.pane !== board.active) showPreview(c);
  }

  function stripEl() {
    const strip = el("div", "board-strip");
    strip.appendChild(el("span", "board-title", "Attention · " + board.win));
    if (board.waiting.length) {
      strip.appendChild(el("span", "board-more", `+${board.waiting.length} waiting:`));
      for (const c of board.waiting) {
        const b = el("button", "board-chip" + (isBlocked(c) ? " blocked" : ""), c.name);
        b.onclick = () => activate(c.pane);
        strip.appendChild(b);
      }
    }
    const nx = el("button", "board-next", "Next ⌘↵");
    nx.title = "Done with this one; open the next (Ctrl/Cmd+Enter)";
    nx.onclick = next;
    strip.appendChild(nx);
    return strip;
  }

  function tileEl(c) {
    const active = c.pane === board.active;
    const tile = el("div", "board-tile" + (active ? " active" : "") + (c.handled ? " handled" : "") +
      (isBlocked(c) ? " blocked" : " turn"));
    tile.dataset.pane = c.pane;
    const head = el("div", "board-head");
    head.appendChild(el("span", "board-name", c.name));
    head.appendChild(el("span", "board-why", reasonLabel(c)));
    head.appendChild(el("span", "board-wait", waitedFor(c.since, Date.now())));
    head.appendChild(tileButtons(c, active));
    tile.appendChild(head);
    const body = el("div", "board-body");
    tile.appendChild(body);
    if (active) hostLiveView(body);
    else {
      body.appendChild(el("pre", "board-preview", "…"));
      body.onclick = () => activate(c.pane);
    }
    return tile;
  }

  // A tile answers nothing by itself: to answer, open it. Its buttons act on
  // the claim as it is when clicked, not as it was drawn, since the header is
  // updated in place and the claim can change under it.
  function tileButtons(c, active) {
    const box = el("span", "board-acts");
    const later = el("button", "board-later", "Not now");
    later.onclick = (e) => {
      e.stopPropagation();
      const now = board.visible.find((x) => x.pane === c.pane);
      if (now) snooze(now);
    };
    box.appendChild(later);
    if (active) {
      const done = el("button", "board-done", "Done");
      done.onclick = (e) => { e.stopPropagation(); next(); };
      box.appendChild(done);
    }
    return box;
  }

  function updateHeaders() {
    const now = Date.now();
    for (const c of board.visible) {
      const tile = $("board").querySelector(`.board-tile[data-pane="${c.pane}"]`);
      if (!tile) continue;
      tile.classList.toggle("handled", !!c.handled);
      tile.classList.toggle("blocked", isBlocked(c));
      tile.classList.toggle("turn", !isBlocked(c));
      tile.querySelector(".board-why").textContent = reasonLabel(c);
      tile.querySelector(".board-wait").textContent = waitedFor(c.since, now);
    }
  }

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  // --- previews ---

  function tick() {
    if (!board.win) return;
    refresh();
    for (const c of board.visible) if (c.pane !== board.active && !c.handled) fetchPreview(c);
  }

  function showPreview(c) {
    const p = board.previews.get(c.pane);
    if (p) paintPreview(c.pane, p);
    fetchPreview(c);
  }

  // A terminal tile re-reads its screen every few seconds (a capture is
  // cheap). An agent tile reads its last turns once per claim: that read can
  // wake a node process on the daemon for an asleep agent.
  async function fetchPreview(c) {
    const key = c.state + c.since;
    const had = board.previews.get(c.pane);
    if ((c.chat && had && had.key === key) || board.fetching.has(c.pane)) return;
    board.fetching.add(c.pane);
    try {
      const url = c.chat ? `/v1/panes/${c.pane}/agent?tail=4` : `/v1/panes/${c.pane}/snapshot?plain=1`;
      const r = await fetch(url);
      if (!r.ok) return;
      const body = await r.json();
      const p = c.chat ? agentPreview(body) : terminalPreview(body);
      p.key = key;
      board.previews.set(c.pane, p);
      paintPreview(c.pane, p);
    } catch (e) {
      console.debug("board preview failed", c.pane, e);
    } finally {
      board.fetching.delete(c.pane);
    }
  }

  function terminalPreview(body) {
    const text = new TextDecoder().decode(b64ToBytes(body.data || "")).replace(/\s+$/, "");
    return { text, cols: body.cols || 80, rows: body.rows || 0 };
  }

  // The agent's last word (its tail) and the card it is waiting on, as
  // text. Same words as the Mac app's BoardPreviewReader.agentPreview.
  function agentPreview(hello) {
    const turns = (hello.turns || []).filter((t) => t.role === "assistant");
    const last = turns[turns.length - 1];
    const said = last ? (last.parts || []).filter((p) => p.type === "text").map((p) => p.text).join("\n") : "";
    const perm = (hello.permissions || [])[0];
    const ask = (hello.questions || [])[0];
    let text = said ? said.slice(-800) : "(no reply yet)";
    if (perm) text += `\n\n? ${perm.permission}: ${(perm.patterns || []).join(" ")}`;
    else if (ask) text += "\n\n? " + (ask.questions || []).map((q) => q.question).join(" / ");
    return { text: text.trim(), cols: 0 };
  }

  // A terminal picture is drawn at TILE_FONT once its pane fits the tile,
  // smaller while another lens has the pane bigger (fontFor). Painting is
  // also when a tile learns its size, so it sizes its pane here.
  function paintPreview(pane, p) {
    const tile = $("board").querySelector(`.board-tile[data-pane="${pane}"]`);
    const pre = tile && tile.querySelector(".board-preview");
    if (!pre) return;
    pre.textContent = p.text || " ";
    pre.classList.toggle("screen", p.cols > 0);
    if (p.cols > 0) {
      const w = pre.clientWidth - PREVIEW_PAD_W, h = pre.clientHeight - PREVIEW_PAD_H;
      pre.style.fontSize = fontFor(w, h, p.cols, p.rows, cell()) + "px";
      sizePane(pane, w, h);
    } else {
      pre.style.fontSize = "";
      pre.scrollTop = pre.scrollHeight;
    }
  }

  // One character's box at TILE_FONT in .board-preview.screen's font and
  // line height (style.css). Measured once; 0.6em wide if it cannot be.
  let cellAt = null;
  function cell() {
    if (cellAt) return cellAt;
    let w = 0;
    try {
      const ctx = document.createElement("canvas").getContext("2d");
      ctx.font = `${TILE_FONT}px Menlo, Monaco, "SF Mono", monospace`;
      w = ctx.measureText("M".repeat(10)).width / 10;
    } catch (e) {
      console.debug("board: could not measure the screen font", e);
    }
    cellAt = { w: w > 0 ? w : TILE_FONT * 0.6, h: TILE_FONT * 1.2 };
    return cellAt;
  }

  // sizePane sizes a screen tile's pane to its text area at TILE_FONT, as a
  // lens sizes a pane to its screen. Once per tile size, not every tick, so
  // if another lens takes the pane over the tile scales its text down rather
  // than fight back. Leaving the board gives the pane back: a workspace
  // re-asserts its own size when it shows the pane. A send that failed is
  // forgotten, so the next paint tries again; one the daemon refused stands
  // (sizeOutcome). Same rule as the Mac's tileArea.
  function sizePane(pane, w, h) {
    const c = board.visible.find((x) => x.pane === pane);
    const grid = tileGrid(w, h, cell());
    if (!c || c.chat || pane === board.active || !grid) return;
    const key = grid.cols + "x" + grid.rows;
    if (board.sized.get(pane) === key) return;
    board.sized.set(pane, key);
    fetch(`/v1/panes/${pane}/resize`, {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(grid),
    }).then(async (r) => {
      if (r.ok) return reread(pane);
      // The daemon's {"error": …} says which: an unknown pane, a host to upgrade, tmux.
      const why = await r.text().catch(() => "");
      notSized(pane, key, sizeOutcome(r.status), `HTTP ${r.status} ${why.trim()}`);
    }).catch((e) => notSized(pane, key, "failed", String(e)));
  }

  // The pane redraws at its new size; read it again once it has, rather than
  // show the old picture until the next tick.
  function reread(pane) {
    setTimeout(() => {
      const now = board.visible.find((x) => x.pane === pane);
      if (now && pane !== board.active) fetchPreview(now);
    }, 400);
  }

  function notSized(pane, key, outcome, why) {
    console.warn("board: could not size pane", pane, "to", key, "-", why, "- its tile scales instead");
    if (outcome === "failed" && board.sized.get(pane) === key) board.sized.delete(pane);
  }

  // A tile that left the board forgets its size, so a new claim on its pane
  // sizes it again, whatever another lens did with it meanwhile. Same rule as
  // the Mac's forgetSizes.
  function forgetSizes() {
    const shown = new Set(board.visible.map((c) => c.pane));
    for (const pane of board.sized.keys()) if (!shown.has(pane)) board.sized.delete(pane);
  }

  // --- keyboard: Ctrl/Cmd+Enter is Next, caught before the terminal sees
  // it. Not while a chat tile is active: Enter sends there, and Next would
  // drop the draft (Done does the same, by hand). Same rule as the Mac app.
  document.addEventListener("keydown", (e) => {
    if (!board.win || e.key !== "Enter" || !(e.metaKey || e.ctrlKey)) return;
    const a = board.visible.find((x) => x.pane === board.active);
    if (a && a.chat) return;
    e.preventDefault();
    e.stopPropagation();
    next();
  }, true);
  document.addEventListener("visibilitychange", reportBoardPresence);
  window.addEventListener("resize", () => { board.gridKey = ""; refresh(); });

  window.ccmuxBoard = {
    open, close, refresh, count, isOpen: () => !!board.win, window: () => board.win,
    reportPresence: () => { if (board.win) reportBoardPresence(); },
    // exposed for smoke_test.js
    activate, snooze, next, sizePane,
    peek: () => ({ active: board.active, waiting: board.waiting.map((c) => c.pane),
      visible: board.visible.map((c) => c.pane + (c.handled ? "!" : "")) }),
    rules: { isBlocked, claimOrder, capFor, columnsFor, snoozedNow, layout, tileGrid, fontFor, sizeOutcome },
  };
})();

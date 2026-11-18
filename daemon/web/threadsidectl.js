// threadsidectl.js: each person's own thread-page sidebar, wired. It reads
// the person's folds and order from the daemon (GET /v1/me/sidebar), hands
// the sidebar (threadview.js) the groups in that order and what each
// header needs (fold, alert, drag), and sends what the person changes: a
// fold (PUT /v1/me/sidebar/{id}) and a new order (PUT /v1/me/sidebar/order).
// A change shows at once and goes back, with a line saying so, if the save
// fails. What these mean is threadsideprefs.js's; this file is the glue.
//
// A page opened at ?g=<windowId> is for that one group ("Open alone in a
// new window"): it shows only that group, unfolded, and moves nothing.
(() => {
  "use strict";
  const P = window.ccmuxThreadSidePrefs;
  const enc = encodeURIComponent;

  // create wires it to the page: d.call is its fetch, d.user() the name the
  // daemon knows the page by, d.changed() redraws, d.tree() is the
  // sidebar's element, d.taskAlerts(g) how many of a group's tasks need a
  // person, d.threadsPath the thread page's address.
  function create(d) {
    const alone = new URLSearchParams(location.search).get("g") || "";
    const st = { prefs: [], gen: 0, saving: 0, note: "", order: [], drag: "", before: null };
    const { load, save } = daemon(st, d);

    const folded = (g) => P.folded(st.prefs, g.id, alone);
    const canFold = (g) => !!g.id && !alone;
    const toggle = (g) => save(P.withFolded(st.prefs, g.id, !folded(g)), "/v1/me/sidebar/" + enc(g.id), { collapsed: !folded(g) }, "the fold");
    const setOrder = (ids) => save(P.withOrder(st.prefs, ids), "/v1/me/sidebar/order", { windowIds: ids }, "the order");

    // arrange is the groups as this person sees them; it remembers the
    // order on screen for Move up/down and the drag.
    function arrange(groups) {
      const out = P.arrange(groups, st.prefs, alone);
      st.order = alone ? [] : P.ids(out);
      return out;
    }

    // menuItems are a group's own entries in its right-click menu.
    function menuItems(g) {
      if (!g || !g.id || alone) return [];
      const items = [];
      const up = P.moveBy(st.order, g.id, -1), down = P.moveBy(st.order, g.id, 1);
      if (up) items.push(["Move up", () => setOrder(up)]);
      if (down) items.push(["Move down", () => setOrder(down)]);
      items.push(["Open alone in a new window", () => window.open(`${d.threadsPath}?g=${enc(g.id)}`, "_blank")]);
      return items;
    }

    // keepAlone is a thread page address that stays on this one group.
    const keepAlone = (href) => (alone ? href + (href.includes("?") ? "&" : "?") + "g=" + enc(alone) : href);

    const drag = dragging(st, d, setOrder);
    const side = {
      folded, canFold, toggle, alert: (g) => P.alert(g, d.taskAlerts(g)), note: () => st.note,
      allHref: alone ? d.threadsPath : "",
      canDrag: (g) => !!g.id && !alone && st.order.length > 1 && fine(), ...drag.handlers,
    };
    return { load, arrange, side, menuItems, keepAlone, dragging: drag.on };
  }

  // daemon reads and writes the person's choices. ?user= is the name the
  // page goes by, like the events feed, so the alias tier and a scratch
  // daemon tell people apart.
  function daemon(st, d) {
    const who = () => "?user=" + enc(d.user());

    // load reads the person's choices. A read that started before a save,
    // or lands while one is out, is dropped: it would undo the click. A
    // read that fails keeps what the page has.
    async function load() {
      const gen = st.gen;
      const got = await d.call("GET", "/v1/me/sidebar" + who()).then((x) => x.groups || [], () => null);
      if (!got || gen !== st.gen || st.saving) return;
      st.prefs = got;
      d.changed();
    }

    async function save(next, path, body, what) {
      const before = st.prefs;
      Object.assign(st, { prefs: next, note: "" });
      st.gen++;
      st.saving++;
      d.changed();
      try {
        await d.call("PUT", path + who(), body);
      } catch (e) {
        Object.assign(st, { prefs: before, note: `Could not save ${what} (${e.message}). It is back as it was.` });
      } finally {
        st.gen++; // a read that started while this was out is older than it
        st.saving--;
        d.changed();
      }
    }

    return { load, save };
  }

  // fine is a mouse or a trackpad: on a touch screen a long press must stay
  // the menu's, so groups move with Move up/down there.
  const fine = () => !!(window.matchMedia && window.matchMedia("(pointer: fine)").matches);

  // dragging moves a group by its header: the header starts it, any part of
  // a group is a target, a line shows where it lands. The page does not
  // redraw the sidebar while a drag is on (on()), so the elements it
  // started on stay; dragend always ends it, dropped or not.
  function dragging(st, d, setOrder) {
    const marks = () => d.tree().querySelectorAll(".group.drop-before, .group.drop-after");
    const unmark = () => { for (const m of marks()) m.classList.remove("drop-before", "drop-after"); };
    const groupEl = (id) => d.tree().querySelector(`.group[data-gid="${CSS.escape(id)}"]`);

    function mark(before) {
      unmark();
      const at = before ? groupEl(before) : groupEl(st.order[st.order.length - 1]);
      if (at) at.classList.add(before ? "drop-before" : "drop-after");
    }

    const handlers = {
      dragStart(g, e) {
        st.drag = g.id;
        st.before = null;
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("text/plain", g.name);
      },
      dragOver(g, e, box) {
        if (!st.drag) return;
        const r = box.getBoundingClientRect();
        const before = P.dropBefore(st.order, g.id, e.clientY > r.top + r.height / 2);
        if (before === null || !P.moveTo(st.order, st.drag, before)) {
          // Where it already is: no line, and no drop.
          if (st.before !== null) { st.before = null; unmark(); }
          return;
        }
        e.preventDefault();
        e.dataTransfer.dropEffect = "move";
        if (before !== st.before) { st.before = before; mark(before); }
      },
      drop(e) {
        if (!st.drag || st.before === null) return;
        e.preventDefault();
        const next = P.moveTo(st.order, st.drag, st.before);
        end();
        if (next) setOrder(next);
      },
      dragEnd: () => end(),
    };

    function end() {
      const was = st.drag;
      Object.assign(st, { drag: "", before: null });
      unmark();
      if (was) d.changed();
    }

    return { handlers, on: () => !!st.drag };
  }

  window.ccmuxThreadSideCtl = { create };
})();

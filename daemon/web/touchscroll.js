// touchscroll.js: a finger scrolls an app that has captured the mouse.
//
// xterm.js ignores touch while the app in the pane has asked for mouse events:
// its touchstart/touchmove handlers return early when
// coreMouseService.areMouseEventsActive. Claude Code in wheel-only mouse mode
// (CLAUDE_CODE_DISABLE_MOUSE_CLICKS) asks, and so does opencode, so on a phone
// a drag over them moved nothing. This turns a one-finger drag into the wheel
// events xterm.js already turns into wheel reports for such an app, with a
// short glide after a flick. A pane whose app has not captured the mouse keeps
// xterm.js's own touch scrolling: nothing here runs for it.
(function (root) {
  "use strict";
  const FRICTION = 0.92; // glide speed kept per frame
  const MIN_SPEED = 0.5; // px per frame below which the glide stops
  const FRAME_MS = 16;
  // A finger that rested this long before lifting was placing the screen, not
  // flicking it. Phones send no touchmove while a finger holds still, so the
  // last move's speed would otherwise glide on past the line it stopped at.
  const REST_MS = 80;

  // wheelFor is the wheel event a finger movement of dy px (down positive)
  // stands for: dragging the content down shows older lines, as a wheel
  // turned up does. Pixel deltas: xterm.js turns them into rows itself.
  function wheelFor(dy, x, y) {
    return { deltaY: -dy, deltaMode: 0, clientX: x, clientY: y, bubbles: true, cancelable: true };
  }

  // glideSpeed is the finger's speed in px per frame from its last movement.
  function glideSpeed(dy, dtMs) {
    return (dy / Math.max(1, dtMs)) * FRAME_MS;
  }

  // liftSpeed is the speed a glide starts at when the finger lifts restMs after
  // its last movement: the flick's own, or none after a rest.
  function liftSpeed(speed, restMs) {
    return restMs > REST_MS ? 0 : speed;
  }

  // rowsFor splits pending finger travel (px) into whole rows to send now and
  // the px left over for the next move. xterm.js sends ONE wheel report per
  // wheel event however far the event goes, so a fast swipe sent as one event
  // per move lost every row but the first of each move.
  function rowsFor(px, rowPx) {
    const rows = Math.trunc(px / rowPx);
    return { rows, rest: px - rows * rowPx };
  }

  function attach(term) {
    const el = term.element;
    const captured = () => term.modes.mouseTrackingMode !== "none";
    const rowPx = () => (el.querySelector(".xterm-screen") || el).clientHeight / term.rows || 14;
    let pending = 0; // finger travel not yet sent, under one row
    // send turns finger travel into one wheel event per whole row crossed.
    const send = (dy, x, y) => {
      const row = rowPx();
      const { rows, rest } = rowsFor(pending + dy, row);
      pending = rest;
      const step = Math.sign(rows) * row;
      for (let i = 0; i < Math.abs(rows); i++) el.dispatchEvent(new WheelEvent("wheel", wheelFor(step, x, y)));
    };
    let last = null;
    let speed = 0;
    let glide = 0;
    el.addEventListener("touchstart", (e) => {
      cancelAnimationFrame(glide);
      last = null;
      pending = 0;
      if (e.touches.length !== 1 || !captured()) return;
      const t = e.touches[0];
      last = { x: t.clientX, y: t.clientY, at: e.timeStamp };
      speed = 0;
    }, { passive: true });
    el.addEventListener("touchmove", (e) => {
      if (!last || e.touches.length !== 1) return;
      const t = e.touches[0];
      const dy = t.clientY - last.y;
      speed = glideSpeed(dy, e.timeStamp - last.at);
      send(dy, t.clientX, t.clientY);
      last = { x: t.clientX, y: t.clientY, at: e.timeStamp };
      e.preventDefault(); // the page must not scroll or bounce under the finger
    }, { passive: false });
    // A lift glides on; so does a cancel, which is what a phone sends when it
    // takes the gesture for itself after all.
    const release = (e) => {
      if (!last) return;
      const { x, y } = last;
      speed = liftSpeed(speed, e.timeStamp - last.at);
      last = null;
      const step = () => {
        speed *= FRICTION;
        if (Math.abs(speed) < MIN_SPEED || !captured()) return;
        send(speed, x, y);
        glide = requestAnimationFrame(step);
      };
      glide = requestAnimationFrame(step);
    };
    el.addEventListener("touchend", release);
    el.addEventListener("touchcancel", release);
  }

  const api = { attach, wheelFor, glideSpeed, liftSpeed, rowsFor, FRICTION, MIN_SPEED, REST_MS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxTouchScroll = api;
})(typeof window !== "undefined" ? window : this);

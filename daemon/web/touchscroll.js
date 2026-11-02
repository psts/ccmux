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

  function attach(term) {
    const el = term.element;
    const captured = () => term.modes.mouseTrackingMode !== "none";
    const send = (dy, x, y) => el.dispatchEvent(new WheelEvent("wheel", wheelFor(dy, x, y)));
    let last = null;
    let speed = 0;
    let glide = 0;
    el.addEventListener("touchstart", (e) => {
      cancelAnimationFrame(glide);
      last = null;
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
    el.addEventListener("touchend", (e) => {
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
    });
  }

  const api = { attach, wheelFor, glideSpeed, liftSpeed, FRICTION, MIN_SPEED, REST_MS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxTouchScroll = api;
})(typeof window !== "undefined" ? window : this);

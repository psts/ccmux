// retry.js: how long a dropped connection waits before it dials again.
//
// The sidebar feed, an agent's chat and the peer messages used to redial
// every 2s for as long as they kept failing.
// Chrome counts failed connections per browser process and, past a few, holds
// back every new one from the windows that share it: one window stuck on a
// refused socket made session switches in the others wait 1.5 to 10s. So a
// connection that keeps failing waits longer each time, 2s doubling to 30s,
// and starts over from 2s once it connects. The terminal socket keeps a flat
// 2s (app.js attach): while it is down, typing goes nowhere.
(function (root) {
  "use strict";

  const FIRST_MS = 2000;
  const MAX_MS = 30000;

  // backoff counts one connection's failures in a row: next() is the wait
  // before its next dial, ok() starts the count over once it has connected.
  function backoff() {
    let failures = 0;
    return {
      next() {
        failures++;
        return Math.min(MAX_MS, FIRST_MS * 2 ** (failures - 1));
      },
      ok() { failures = 0; },
    };
  }

  const api = { backoff, FIRST_MS, MAX_MS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  root.ccmuxRetry = api;
})(typeof window !== "undefined" ? window : this);

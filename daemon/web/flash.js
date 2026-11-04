// flash.js: how long a session row's attention flash lasts in the sidebar.
//
// Orange (needs input) flashes until it is answered, for every session.
// Green (done) flashes until someone looks, for a repo session. An agent
// finishes often and on its own, so its green lasts one minute from when it
// finished and then goes off; the attention board still lists it. The start
// is the daemon's claim time (pane attentionSince, unix ms), so a reload does
// not restart the minute. A claim with no time (an older daemon) flashes as
// before.
(function (root) {
  "use strict";

  const AGENT_DONE_MS = 60000;

  // doneShows says whether a pane's "done" still flashes at now (unix ms).
  function doneShows(agent, since, now) {
    if (!agent || !since) return true;
    return now - since < AGENT_DONE_MS;
  }

  const api = { doneShows, AGENT_DONE_MS };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  root.ccmuxFlash = api;
})(typeof window !== "undefined" ? window : this);

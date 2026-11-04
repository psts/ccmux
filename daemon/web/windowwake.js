// windowwake.js: a closed window comes back as it was left.
//
// A window's close sends which of its sessions are awake (awakeAt); the
// daemon marks them. Opening the window answers the marked ones as `wake`,
// and only those that are asleep are woken (toWake). Waking a session clears
// its mark on the daemon, so an open while someone else has the window wakes
// only what is still marked and asleep. No list in the answer wakes nothing.
(function (root) {
  "use strict";

  const isCold = (ws) => !!ws && ws.status === "cold";

  // awakeAt is the ids of a window's members that are awake now.
  function awakeAt(memberIds, byId) {
    return (memberIds || []).filter((id) => byId(id) && !isCold(byId(id)));
  }

  // toWake is the ids of the members an open should wake, given its answer.
  function toWake(answer, memberIds, byId) {
    const wake = answer && Array.isArray(answer.wake) ? answer.wake : [];
    return wake.filter((id) => (memberIds || []).includes(id) && isCold(byId(id)));
  }

  const api = { awakeAt, toWake };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  root.ccmuxWindowWake = api;
})(typeof window !== "undefined" ? window : this);

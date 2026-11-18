// dom.js: the three helpers every piece of shared page code leans on. Both
// pages load it first: the lens (index.html) and the thread page
// (threads.html). Function declarations at the top level on purpose, so a
// later script sees them as plain globals; a script that wants its own $
// (the thread page's are all wrapped) simply shadows this one.
"use strict";

function $(id) { return document.getElementById(id); }

// Quotes included: group names are user-typed (moveToWindow's prompt) and get
// interpolated into title="..." attributes — a bare " would break out.
function esc(s) {
  return String(s).replace(/[<>&"']/g,
    (c) => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

// Copies text and says how it went in `state`. A silent Copy is worse than
// none: the user just revealed a secret, and a click that copies nothing
// leaves them pasting whatever was there before. The clipboard API is absent
// on a plain-http lens and a write can be refused even on https, so both
// outcomes land in the state line; the text is on screen and selectable
// either way. Top level so the smoke test can drive all three outcomes.
async function copyToClipboard(text, state) {
  if (!text) return;
  if (!navigator.clipboard) {
    state.textContent = "Copy needs https here; select the key and copy it by hand.";
    return;
  }
  try {
    await navigator.clipboard.writeText(text);
    state.textContent = "Copied.";
  } catch (e) {
    state.textContent = "Couldn't copy (" + (e.message || e) + "); select the key and copy it by hand.";
  }
}

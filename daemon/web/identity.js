// identity.js: who the daemon takes this page for. One copy for both pages:
// the lens (app.js gates its boot on it) and the thread page, whose
// Notifications tab shows the same line and subscribes under the same name.
// Top-level declarations, like dom.js, so later scripts read them as globals.
"use strict";

// Who the daemon takes this lens for, from GET /v1/whoami at boot. On the
// tailnet that is the Tailscale login of the device (an iPhone, a laptop) and
// it is VOUCHED: routing, suppression and window ownership already keyed on it
// and ignored whatever name was typed here, so the prompt below was ceremony.
// Null until the answer arrives, and stays null when the daemon cannot vouch
// (loopback with no owner setting, a scratch daemon) — then the prompt is
// still the only name there is.
let whoami = null;
// Set when the daemon could not be ASKED (unreachable, timed out, or an error
// status). Distinct from a null whoami: "the daemon did not say" must not
// render as "the daemon says it does not know you", which sends a reader to
// reconfigure a working identity. A 404 is not an error here: the daemon is
// too old for the route and genuinely has nothing to say.
let whoamiError = null;
// Set when the daemon answered 404: too old for the route. Not an error, but
// not "unknown to the daemon" either — an old daemon still vouches every
// tailnet request the old way, so the line must not send a reader to
// reconfigure a working identity.
let whoamiUnsupported = false;

// The name this lens goes by, without ever prompting: the vouched identity,
// else the stored name, else anon. deviceLabel and whoAmILine read it as is;
// getUser is the prompting superset, asking only when nothing is stored.
const currentName = () => (whoami ? (whoami.display || whoami.login) : (localStorage.getItem("ccmux-user") || "anon"));

// Bounds loadIdentity so a half-open connection cannot hold the whole boot.
const WHOAMI_TIMEOUT_MS = 3000;

// Asks the daemon who it takes this lens for. The stored name rides along
// (read directly, never via getUser, which would prompt): the daemon's alias
// tier maps a typed name onto a login, and asking as "anon" would miss it.
// On a timeout the boot proceeds with the prompt.
async function loadIdentity() {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), WHOAMI_TIMEOUT_MS);
  try {
    const declared = localStorage.getItem("ccmux-user") || "";
    const r = await fetch("/v1/whoami?user=" + encodeURIComponent(declared), { signal: ctl.signal });
    if (r.status === 404) {
      whoamiUnsupported = true;
      console.warn("[ccmux] this daemon has no /v1/whoami; falling back to the name prompt");
      return;
    }
    if (!r.ok) {
      whoamiError = `Couldn't reach ccmuxd at ${location.host} to check who you are (HTTP ${r.status}).`;
      console.warn(`[ccmux] whoami answered ${r.status}; falling back to the name prompt`);
      return;
    }
    const id = await r.json();
    if (id && id.vouched && id.login) whoami = id;
  } catch (e) {
    whoamiError = `Couldn't reach ccmuxd at ${location.host} to check who you are (${e && e.name === "AbortError" ? "timed out" : e}).`;
    console.warn("[ccmux] whoami failed; falling back to the name prompt", e);
  } finally {
    clearTimeout(timer);
    renderWhoAmI();
  }
}

// One ask per page, whoever asks first. The lens asks at boot (app.js); the
// thread page has no boot that needs the answer, so its Notifications tab
// asks when Settings first opens. Both get the same promise back.
let identityAsked = null;
function askIdentityOnce() {
  if (!identityAsked) identityAsked = loadIdentity();
  return identityAsked;
}

// A display name for presence: the vouched identity when the daemon has one,
// else asked once and remembered. Callers run after boot awaited loadIdentity,
// so a vouched lens never sees the prompt.
function getUser() {
  if (whoami || localStorage.getItem("ccmux-user")) return currentName();
  const u = (prompt("Your name (for presence):", "") || "anon").trim() || "anon";
  localStorage.setItem("ccmux-user", u);
  renderWhoAmI(); // the line rendered "anon" before the prompt had an answer
  return u;
}

// The settings line under Notifications: the same identity pushes are keyed
// on, so a phone that never buzzes can be checked against it. Names the TIER
// that keyed the login (whoami.source), because the alias and owner tiers
// look alike from here and send a reader to different settings.
function whoAmILine() {
  if (whoamiError) return whoamiError;
  if (whoamiUnsupported) {
    return `This daemon does not say who you are (no /v1/whoami; update ccmuxd). Going by the name "${currentName()}".`;
  }
  if (!whoami) {
    return `Not identified by Tailscale. Going by the name "${currentName()}".`;
  }
  const who = whoami.display && whoami.display !== whoami.login
    ? `${whoami.display} (${whoami.login})` : whoami.login;
  switch (whoami.source) {
    case "tailscale": return `Signed in as ${who} via Tailscale.`;
    case "alias": return `Signed in as ${who}, by the daemon's identity alias for "${whoami.display}".`;
    case "owner": return `Signed in as ${who}, this machine's owner.`;
    default: return `Signed in as ${who}.`;
  }
}

// Writes the line where the Notifications tab keeps it. The tab is injected
// by a later script, so an answer that lands before then finds no element;
// the tab draws the line itself each time Settings opens (push.js).
function renderWhoAmI() {
  const el = $("whoami");
  if (el) el.textContent = whoAmILine();
}

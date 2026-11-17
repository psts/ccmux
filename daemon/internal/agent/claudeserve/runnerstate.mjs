// What a session runner keeps so a crash or a daemon restart loses nothing:
// a small state file (is a turn open, which prompts reached Claude) and a
// numbered log of the events it sent, so a client that reconnects asks for
// "everything after N". Pure: the file edges come in as functions, so
// runnerstate.test.mjs runs it without a disk or the SDK.

// RUNNER_PROTOCOL is the version of the runner's HTTP surface the daemon
// checks at connect (GET hello). Raise it on any change the daemon must
// know about; an old runner then gets restarted on the new one.
export const RUNNER_PROTOCOL = 2;

// deliveredCap bounds the ids kept as delivered. The daemon asks about the
// prompts it still holds as undelivered, which are the newest few, so the
// oldest can go.
const deliveredCap = 500;

// RunnerState is the state file's content: the prompts sent and not yet
// answered (open, oldest first, each with its text for the notice), the
// ids Claude has in its transcript (delivered), and what to tell Claude at
// the next start when this process is being restarted on purpose (note: a
// thread member moved into its worktree, readonly.mjs).
export class RunnerState {
  constructor(data = {}) {
    this.note = typeof data.note === "string" ? data.note : "";
    this.open = Array.isArray(data.open) ? data.open.filter((p) => p && p.id).map((p) => ({ id: String(p.id), text: String(p.text || "") })) : [];
    this.delivered = Array.isArray(data.delivered) ? data.delivered.slice(-deliveredCap) : [];
  }

  // markDelivered records ids Claude has (seen in the transcript or stamped
  // on a reply). It reports whether any id was new.
  markDelivered(ids) {
    let changed = false;
    for (const id of ids) {
      if (!id || this.delivered.includes(id)) continue;
      this.delivered.push(id);
      changed = true;
    }
    if (this.delivered.length > deliveredCap) this.delivered = this.delivered.slice(-deliveredCap);
    return changed;
  }

  isDelivered(id) {
    return this.delivered.includes(id);
  }

  toJSON() {
    return { open: this.open, delivered: this.delivered, note: this.note };
  }
}

// loadState reads the state file; a missing file is a fresh session, and an
// unreadable one is said and treated as fresh rather than ending the start.
export function loadState(path, readFile, warn) {
  if (!path) return new RunnerState();
  let text;
  try {
    text = readFile(path, "utf8");
  } catch (err) {
    if (!err || err.code !== "ENOENT") warn(`state file ${path}: ${err && err.message ? err.message : err}`);
    return new RunnerState();
  }
  try {
    return new RunnerState(JSON.parse(text));
  } catch (err) {
    warn(`state file ${path} is not JSON, starting fresh: ${err.message}`);
    return new RunnerState();
  }
}

// saveState writes the file whole through a temporary name, so a crash
// mid-write leaves the old file, never half a new one.
export function saveState(path, state, fs) {
  if (!path) return;
  const tmp = path + ".tmp";
  fs.writeFile(tmp, JSON.stringify(state));
  fs.rename(tmp, path);
}

// EventLog numbers events and keeps the newest ones for replay.
export class EventLog {
  constructor(cap = 2000) {
    this.cap = cap;
    this.seq = 0;
    this.items = []; // {seq, ev}, oldest first
  }

  push(ev) {
    this.seq += 1;
    this.items.push({ seq: this.seq, ev });
    if (this.items.length > this.cap) this.items.shift();
    return this.seq;
  }

  // after is every kept event numbered above n. gap is true when events
  // between n and the oldest kept one were dropped, so the reader must
  // rebuild from the transcript instead.
  after(n) {
    const from = Number(n) || 0;
    const oldest = this.items.length ? this.items[0].seq : this.seq + 1;
    return { gap: from > 0 && from < oldest - 1, events: this.items.filter((x) => x.seq > from) };
  }
}

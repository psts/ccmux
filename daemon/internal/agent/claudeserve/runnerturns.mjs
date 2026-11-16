// A session runner's turns: which prompts are open (sent, not answered),
// whether the session is busy, and whether a restart cut a turn off. Agent
// panes keep their own simpler rule in agent.mjs; this is the runner's,
// kept pure (runnerturns.test.mjs) and saved through RunnerState.
//
// A prompt is open from the moment it is sent until a result ends the turn
// that answered it. Claude names the prompts a turn answers on its replies
// (user_message_uuid, and user_message_uuids for prompts folded into a
// running turn), so a result closes exactly those. Prompts queued behind a
// running turn stay open, so the session stays busy and a crash during
// them is still caught.

const noticeHead = "[ccmux] This session was restarted while a turn was running, so that turn was cut off.";

export class RunnerTurns {
  // state is the runner's RunnerState (runnerstate.mjs).
  constructor(state) {
    this.state = state;
    this.answering = new Set(); // ids the running turn has replied to
  }

  sent(id, text) {
    if (!this.state.open.some((p) => p.id === id)) this.state.open.push({ id, text: String(text).slice(0, 200) });
  }

  // stamped records the prompts a reply names.
  stamped(ids) {
    for (const id of ids) if (id) this.answering.add(id);
  }

  // result ends a turn: the prompts it answered close. A turn that named
  // none (an error before any reply) closes the oldest prompt Claude has,
  // the one it was working on.
  result() {
    const before = this.state.open.length;
    if (this.answering.size) {
      this.state.open = this.state.open.filter((p) => !this.answering.has(p.id));
    } else {
      const i = this.state.open.findIndex((p) => this.state.isDelivered(p.id));
      if (i >= 0) this.state.open.splice(i, 1);
    }
    this.answering.clear();
    return this.state.open.length !== before;
  }

  busy() {
    return this.state.open.length > 0;
  }

  // notice is the prompt a restarted runner sends when a turn was cut off:
  // an open prompt Claude has in its transcript was being worked on. Open
  // prompts Claude never got are left alone; the daemon sends them again.
  // The cut-off prompts close, since the notice takes over from them.
  notice() {
    const cut = this.state.open.filter((p) => this.state.isDelivered(p.id));
    if (!cut.length) return "";
    this.state.open = this.state.open.filter((p) => !this.state.isDelivered(p.id));
    const last = cut[cut.length - 1].text;
    const what = last ? ` The turn was answering: "${last}".` : "";
    return noticeHead + what + " Check what state your last steps left (files, git, running commands, messages sent) before redoing any of them, then carry on.";
  }
}

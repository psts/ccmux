// threadedit.js: "Edit" on a message the person sent, on the thread page.
// Like claude.ai: the conversation goes back to just before that message,
// the files Claude changed since are put back, and the message's words wait
// in the box to be changed and sent again. The runner does the going back
// and the daemon starts the session again on it (POST
// /v1/sessions/{id}/rewind; claudeserve/rewind.mjs). A message sent while
// Claude works is stopped first, after the person says so.
(() => {
  "use strict";

  const WAIT_MS = 500; // between looks while a stopped turn winds down
  const WAIT_TRIES = 20;

  // question is what the person is asked before going back: what leaves
  // the conversation, what happens to the files, and what cannot be undone.
  function question(look, b) {
    const n = look.dropped || 1;
    const lines = ["Go back to this message and edit it?", ""];
    lines.push(n > 1 ? `This message and the ${n - 1} after it, with Claude's answers, leave the conversation.` : "This message and Claude's answer to it leave the conversation.");
    lines.push(filesLine(look.files || {}));
    lines.push("Commands Claude ran (installs, git and the like) are not undone.");
    if (b.files && b.files.length) lines.push("Its pictures are not put back in the box; paste them again.");
    lines.push("", "The session starts again, which takes a few seconds.");
    return lines.join("\n");
  }

  function filesLine(f) {
    if (!f.canRewind) return `Files cannot be put back${f.error ? ` (${f.error.replace(/\.$/, "")})` : ""}: only the conversation goes back.`;
    const list = f.files || [];
    if (!list.length) return "No files were changed since.";
    const names = list.slice(0, 5).map((p) => p.split("/").pop());
    const more = list.length > 5 ? ` and ${list.length - 5} more` : "";
    return `Files Claude changed since are put back (${list.length}): ${names.join(", ")}${more}.`;
  }

  // working is a refusal that only means the stopped turn has not ended.
  const working = (e) => /working|stop the turn/i.test(e.message || "");

  // create wires it to the page: d.call is its fetch, d.busy() whether
  // Claude is working, d.confirm asks, d.wait(ms) sleeps, d.fill(text) puts
  // the words back in the box, d.done(text) says what happened, d.fail(text)
  // what went wrong.
  function create(d) {
    let going = false;

    // look asks what going back would do, waiting out a turn that was
    // just stopped.
    async function look(sid, to) {
      for (let i = 0; ; i++) {
        try {
          return await d.call("POST", `/v1/sessions/${sid}/rewind`, { to, dry: true });
        } catch (e) {
          if (!working(e) || i >= WAIT_TRIES) throw e;
          await d.wait(WAIT_MS);
        }
      }
    }

    async function edit(sid, b) {
      if (going || !sid) return;
      going = true;
      try {
        if (d.busy()) {
          if (!d.confirm("Claude is working. Stop it and go back to this message?")) return;
          await d.call("POST", `/v1/sessions/${sid}/abort`);
        }
        const seen = await look(sid, b.id);
        if (!d.confirm(question(seen, b))) return;
        const out = await d.call("POST", `/v1/sessions/${sid}/rewind`, { to: b.id });
        d.fill(b.text || "");
        d.done(out.files && out.files.canRewind === false && (seen.files || {}).canRewind ? `Went back, but the files could not be put back: ${out.files.error}` : "Went back. Edit the message and send it.");
      } catch (e) {
        d.fail("Going back failed: " + e.message);
      } finally {
        going = false;
      }
    }

    return { edit };
  }

  const api = { create, question };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  if (typeof window !== "undefined") window.ccmuxThreadEdit = api;
})();

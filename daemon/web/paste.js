// paste.js: a pasted image reaches the session's Claude.
//
// Claude Code reads the clipboard of the machine it RUNS on, so a screenshot
// copied on this computer or phone is invisible to a hosted session. The
// image goes to the daemon instead (POST /v1/workspaces/{id}/paste, which the
// hub forwards to the session's host), the daemon answers with a file path on
// that host, and the path is typed in with a trailing space, as a drag and
// drop would. The Mac app does the same (RemoteTermController.swift).
//
// Into a terminal the path goes as keystrokes; into the agent chat box it is
// inserted at the cursor. Everything else pastes as the browser would.
(function (root) {
  "use strict";
  const MAX_BYTES = 10 << 20; // the daemon's cap (paste.go maxPasteBytes)

  // imageFrom says which image a paste carries, or null for a text paste.
  // Same order as the Mac app: a copy that carries real text (a spreadsheet,
  // a web selection with a picture in it) pastes as text, so a paste never
  // turns text into an upload by surprise. A bare image (a screenshot), or a
  // copied image file whose only text is its own name, is the image. Any
  // picture counts; one of the four kinds the daemon takes is preferred when
  // a copy offers several (Preview offers TIFF beside PNG).
  const KINDS = /^image\/(png|jpeg|gif|webp)$/; // paste.go sniffImageExt
  function imageFrom(dt) {
    if (!dt) return null;
    const files = [...(dt.files || [])];
    const items = [...(dt.items || [])].filter((i) => i.kind === "file").map((i) => i.getAsFile());
    const all = [...files, ...items].filter((f) => f && /^image\//.test(f.type));
    const img = all.find((f) => KINDS.test(f.type)) || all[0];
    if (!img) return null;
    const text = ((dt.getData && dt.getData("text/plain")) || "").trim();
    return text && text !== img.name ? null : img;
  }

  // uploadable gives an image the daemon takes: the four kinds as they are,
  // and a HEIC photo as it is too, since the daemon converts those (heic.go).
  // Never redrawn here: a phone photo of 2 MB as HEIC is 15 MB or more as a
  // PNG, over the paste cap. Any other picture is redrawn as PNG by toPng; one
  // this browser cannot open is refused with its kind named, and one it opened
  // but could not redraw (too big for a canvas) says that instead.
  const HEIF = /^image\/hei[cf]/;
  async function uploadable(img, toPng) {
    if (KINDS.test(img.type) || HEIF.test(img.type)) return img;
    const kind = img.type.replace(/^image\//, "").toUpperCase();
    try {
      return await toPng(img);
    } catch (err) {
      if (err.unreadable) throw new Error(`this browser cannot read ${kind} images; copy it as PNG or JPEG instead`);
      throw new Error(`the ${kind} image could not be turned into a PNG (${err.message})`);
    }
  }

  // toPng draws a picture onto a canvas and reads it back as PNG.
  async function toPng(img) {
    const url = URL.createObjectURL(img);
    try {
      const pic = await opened(url);
      const c = document.createElement("canvas");
      c.width = pic.naturalWidth;
      c.height = pic.naturalHeight;
      const ctx = c.getContext("2d");
      if (!ctx) throw new Error("no room to draw it");
      ctx.drawImage(pic, 0, 0);
      const png = await new Promise((resolve) => c.toBlob(resolve, "image/png"));
      if (!png) throw new Error(`${c.width}x${c.height} is too large to redraw`);
      return png;
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  // opened decodes a picture, failing with err.unreadable when this browser
  // cannot open its kind or it has no size to draw.
  async function opened(url) {
    const pic = new Image();
    pic.src = url;
    const decoded = await pic.decode().then(() => true, () => false);
    if (!decoded || !pic.naturalWidth || !pic.naturalHeight) throw Object.assign(new Error("unreadable"), { unreadable: true });
    return pic;
  }

  // upload lands the image on the session's host and returns its path there.
  async function upload(wsId, img, fetchFn) {
    if (img.size > MAX_BYTES) throw new Error("the image is over 10 MB");
    const r = await fetchFn(`/v1/workspaces/${encodeURIComponent(wsId)}/paste`, {
      method: "POST", headers: { "Content-Type": "application/octet-stream" }, body: img,
    });
    const body = await r.json().catch(() => ({}));
    if (r.status !== 201 || !body.path) throw new Error(body.error || `the daemon answered ${r.status}`);
    return body.path;
  }

  // target says where a paste event lands: the chat box it was made in, the
  // terminal, or nowhere this file handles.
  function target(el) {
    const box = el && el.closest && el.closest(".ac-compose textarea");
    if (box) return { chat: box };
    if (el && el.closest && el.closest("#terminal")) return { terminal: true };
    return null;
  }

  // onPaste handles one paste event. env: currentPane() -> {ws, pane} or
  // null, typeIntoPane({ws, pane}, text) -> whether it was typed,
  // fail(message, keep), fetch. The pane is read when the paste happens: the
  // upload can take seconds, and the path belongs where the paste was made.
  // keep marks a note that carries the uploaded file's path: it is the only
  // copy, so that note stays until it is closed.
  async function onPaste(e, env) {
    const where = target(e.target);
    const img = where && imageFrom(e.clipboardData);
    if (!img) return; // text, or not ours: the browser pastes it
    e.preventDefault();
    e.stopPropagation();
    const to = env.currentPane();
    if (!to) return env.fail("Open a session before pasting an image.");
    let path;
    try {
      path = await upload(to.ws, await uploadable(img, env.toPng), env.fetch);
    } catch (err) {
      return env.fail("The image was not pasted: " + err.message);
    }
    deliver(where, to, path, env);
  }

  // deliver puts the path where the paste was made, if that pane is still the
  // one on screen. The chat box is one textarea shared by every agent pane,
  // and the terminal follows the open pane, so after a move either would take
  // the path to a session whose host may not have the file. Then the path
  // goes in a note instead, which stays until closed.
  function deliver(where, to, path, env) {
    const now = env.currentPane();
    if (!now || now.ws !== to.ws || now.pane !== to.pane) {
      return env.fail(`You left that pane before the image finished uploading, so its path was not added: ${path}`, true);
    }
    if (where.terminal) {
      if (!env.typeIntoPane(to, path + " ")) env.fail(`The session was not connected, so the image path was not typed: ${path}`, true);
      return;
    }
    where.chat.setRangeText(path + " ", where.chat.selectionStart, where.chat.selectionEnd, "end");
    where.chat.dispatchEvent(new Event("input", { bubbles: true }));
  }

  const NOTE_MS = 10000;

  // showNote puts a paste note in its own bar at the bottom of the screen, so
  // it shows whatever pane, session or board is open. A newer note replaces
  // an older one. It clears itself after NOTE_MS unless keep is set.
  function showNote(doc, text, keep, later) {
    const old = doc.getElementById("paste-bar");
    if (old) old.remove();
    const bar = doc.createElement("div");
    bar.id = "paste-bar";
    const span = doc.createElement("span");
    span.textContent = text; // the daemon's error text is never markup
    const close = doc.createElement("button");
    close.className = "paste-dismiss";
    close.title = "Close";
    close.textContent = "\u00d7";
    close.onclick = () => bar.remove();
    bar.append(span, close);
    doc.body.appendChild(bar);
    if (!keep) later(() => bar.remove(), NOTE_MS);
    return bar;
  }

  const api = { imageFrom, uploadable, upload, target, onPaste, showNote };
  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
    return;
  }
  root.ccmuxPaste = api;
  // Capture phase on the page, so the image is taken before xterm's own
  // paste handler turns the clipboard's text (if any) into keystrokes.
  root.document.addEventListener("paste", (e) => {
    const app = root.ccmux || {};
    onPaste(e, {
      currentPane: () => (app.currentPane ? app.currentPane() : null),
      typeIntoPane: (to, t) => Boolean(app.typeIntoPane && app.typeIntoPane(to, t)),
      fail: (msg, keep) => showNote(root.document, msg, keep, root.setTimeout),
      fetch: (...a) => root.fetch(...a),
      toPng,
    });
  }, true);
})(typeof window !== "undefined" ? window : this);

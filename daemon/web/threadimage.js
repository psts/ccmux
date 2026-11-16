// threadimage.js: getting a pasted or picked picture ready to send to a
// session. The daemon takes PNG, JPEG or GIF up to 3000 px a side and 5 MB
// (api/sessionmedia.go says why), and nothing after the upload resizes it, so
// the page does it here: a screenshot from a large screen is drawn smaller,
// and a kind the daemon does not take (WebP, a browser's own formats) is
// drawn again as a PNG.
(function (root) {
  "use strict";

  // MAX_SIDE is the longest side sent. Claude looks at an image at up to
  // 2576 px on its long edge and shrinks anything larger itself, so drawing
  // it at that size here loses nothing it would have seen.
  const MAX_SIDE = 2576;
  const MAX_BYTES = 5 << 20;
  const asIs = new Set(["image/png", "image/jpeg", "image/gif"]);

  // fit is the size to draw a w x h picture at: the same when it fits, else
  // scaled so its longer side is max, never below one pixel.
  function fit(w, h, max = MAX_SIDE) {
    if (w <= max && h <= max) return { w, h, scaled: false };
    const k = max / Math.max(w, h);
    return { w: Math.max(1, Math.round(w * k)), h: Math.max(1, Math.round(h * k)), scaled: true };
  }

  // plan says what to do with a picture of this type, size and byte count:
  // "send" as it is, or "draw" it again (as "image/png", or "image/jpeg"
  // when it is too heavy to stay a PNG).
  function plan(type, w, h, bytes) {
    const size = fit(w, h);
    const heavy = bytes > MAX_BYTES;
    if (asIs.has(type) && !size.scaled && !heavy) return { act: "send", ...size };
    return { act: "draw", as: heavy || type === "image/jpeg" ? "image/jpeg" : "image/png", ...size };
  }

  // prepare turns a File or Blob into the Blob to upload. Browser only.
  async function prepare(file) {
    const bitmap = await root.createImageBitmap(file);
    const p = plan(file.type, bitmap.width, bitmap.height, file.size);
    if (p.act === "send") return file;
    const canvas = root.document.createElement("canvas");
    canvas.width = p.w;
    canvas.height = p.h;
    canvas.getContext("2d").drawImage(bitmap, 0, 0, p.w, p.h);
    const blob = await new Promise((resolve) => canvas.toBlob(resolve, p.as, 0.9));
    if (!blob) throw new Error("this browser could not redraw the image");
    return blob;
  }

  // pasted is the image files in a paste or a drop, in order.
  function pasted(dt) {
    return [...((dt && dt.files) || [])].filter((f) => /^image\//.test(f.type));
  }

  const api = { fit, plan, prepare, pasted, MAX_SIDE, MAX_BYTES };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadImage = api;
})(typeof window !== "undefined" ? window : this);

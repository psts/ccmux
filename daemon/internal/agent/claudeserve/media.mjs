// A session's images, kept as files in its media folder and named after
// their bytes, so the events and the transcript carry a short name instead
// of megabytes of base64, and the same picture read back from disk after a
// restart lands on the same file. The daemon serves the folder to the page
// and writes a pasted image into it under the same rule
// (api/sessionmedia.go). Pure but for the fs edges handed in
// (media.test.mjs).
import { createHash } from "node:crypto";

// mediaName is the only shape of name this folder holds. The daemon checks
// the same pattern before it serves or attaches one.
export const mediaName = /^[0-9a-f]{32}\.(png|jpg|gif|webp)$/;

const extOf = { "image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp" };
const mimeOf = { png: "image/png", jpg: "image/jpeg", gif: "image/gif", webp: "image/webp" };

// mimeFor is the media type a stored name says it is.
export function mimeFor(name) {
  return mimeOf[String(name).split(".").pop()] || "";
}

// MediaStore reads and writes one session's media folder. fs is
// {mkdir(dir), exists(path), write(path, bytes), read(path) → Buffer}; warn
// hears a picture that could not be kept.
export class MediaStore {
  constructor(dir, fs, warn = () => {}) {
    this.dir = dir;
    this.fs = fs;
    this.warn = warn;
  }

  // put stores one image given as base64 and returns its name, or "" when
  // it is not kept: a kind this folder does not hold, or a folder that
  // cannot be written (full disk, no permission). The page then shows no
  // picture for it, and the warning says why. A picture is only for show
  // here, so failing to keep one never fails the conversation it is in: this
  // runs for every message, also when a session's history is read back at
  // its start, where a throw would end the runner on every restart. A
  // picture already there is not written again.
  put(base64, mime) {
    const ext = extOf[mime];
    if (!ext || !base64) return "";
    const bytes = Buffer.from(base64, "base64");
    const name = createHash("sha256").update(bytes).digest("hex").slice(0, 32) + "." + ext;
    const path = this.dir + "/" + name;
    try {
      if (!this.fs.exists(path)) {
        this.fs.mkdir(this.dir);
        this.fs.write(path, bytes);
      }
    } catch (err) {
      this.warn(`image not kept at ${path}: ${err && err.message ? err.message : String(err)} (the conversation goes on without the picture)`);
      return "";
    }
    return name;
  }

  // block is one stored image as the content block Claude reads. A name of
  // any other shape, or one with no file, throws: a prompt that says it has
  // a picture must not go out without it.
  block(name) {
    if (!mediaName.test(name)) throw new Error("not an image of this session: " + name);
    const data = this.fs.read(this.dir + "/" + name).toString("base64");
    return { type: "image", source: { type: "base64", media_type: mimeFor(name), data } };
  }
}

// imageRefs stores every image block of a content list and returns one
// reference per stored image, as {mime, name}. With no store (an agent
// pane) there are none, as before images were kept.
export function imageRefs(store, content) {
  if (!store || !Array.isArray(content)) return [];
  const refs = [];
  for (const b of content) {
    const src = b && b.type === "image" && b.source && b.source.type === "base64" ? b.source : null;
    const name = src ? store.put(src.data, src.media_type) : "";
    if (name) refs.push({ mime: src.media_type, name });
  }
  return refs;
}

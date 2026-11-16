// node --test media.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { MediaStore, imageRefs, mediaName, mimeFor } from "./media.mjs";

// memFS is a folder in memory with the store's four edges.
function memFS() {
  const files = new Map();
  const made = [];
  return {
    files, made,
    mkdir: (d) => made.push(d),
    exists: (p) => files.has(p),
    write: (p, b) => files.set(p, Buffer.from(b)),
    read: (p) => { if (!files.has(p)) throw Object.assign(new Error("ENOENT " + p), { code: "ENOENT" }); return files.get(p); },
  };
}

const png = Buffer.from("\x89PNG fake bytes", "binary").toString("base64");

test("an image is stored once under a name made from its bytes", () => {
  const fs = memFS();
  const store = new MediaStore("/m", fs);
  const name = store.put(png, "image/png");
  assert.match(name, mediaName);
  assert.equal(store.put(png, "image/png"), name, "the same bytes land on the same file");
  assert.equal(fs.files.size, 1);
  assert.equal(fs.made.length, 1, "nothing is written the second time");
  assert.notEqual(store.put(Buffer.from("other").toString("base64"), "image/png"), name);
  assert.equal(mimeFor(name), "image/png");
  assert.equal(mimeFor(store.put(png, "image/jpeg")), "image/jpeg");
});

test("a kind the folder does not hold is not stored", () => {
  const fs = memFS();
  const store = new MediaStore("/m", fs);
  assert.equal(store.put(png, "image/svg+xml"), "");
  assert.equal(store.put("", "image/png"), "");
  assert.equal(fs.files.size, 0);
});

test("a stored image goes back to Claude as the bytes that came in", () => {
  const store = new MediaStore("/m", memFS());
  const name = store.put(png, "image/png");
  assert.deepEqual(store.block(name), { type: "image", source: { type: "base64", media_type: "image/png", data: png } });
});

test("a name of another shape, or with no file, is refused", () => {
  const store = new MediaStore("/m", memFS());
  assert.throws(() => store.block("../../etc/passwd"), /not an image of this session/);
  assert.throws(() => store.block("a.png"), /not an image of this session/);
  assert.throws(() => store.block("0123456789abcdef0123456789abcdef.png"), /ENOENT/);
});

test("the image blocks of a content list become references, the rest is left", () => {
  const store = new MediaStore("/m", memFS());
  const refs = imageRefs(store, [
    { type: "text", text: "look" },
    { type: "image", source: { type: "base64", media_type: "image/png", data: png } },
    { type: "image", source: { type: "url", url: "https://example.invalid/a.png" } },
    { type: "image", source: { type: "base64", media_type: "image/tiff", data: png } },
  ]);
  assert.equal(refs.length, 1);
  assert.deepEqual(Object.keys(refs[0]), ["mime", "name"]);
  assert.match(refs[0].name, mediaName);
});

test("without a store there are no references", () => {
  assert.deepEqual(imageRefs(null, [{ type: "image", source: { type: "base64", media_type: "image/png", data: png } }]), []);
  assert.deepEqual(imageRefs(new MediaStore("/m", memFS()), "plain text"), []);
});

test("a folder that cannot be written costs the picture, never the conversation", () => {
  const fs = memFS();
  const warns = [];
  const store = new MediaStore("/m", fs, (w) => warns.push(w));
  const kept = store.put(png, "image/png"); // on disk before the folder filled up
  fs.write = () => { throw Object.assign(new Error("ENOSPC: no space left on device"), { code: "ENOSPC" }); };
  const other = Buffer.from("another picture").toString("base64");
  assert.equal(store.put(other, "image/png"), "", "not kept, and nothing thrown");
  assert.equal(warns.length, 1);
  assert.match(warns[0], /^image not kept at \/m\/[0-9a-f]{32}\.png: ENOSPC/);
  assert.deepEqual(imageRefs(store, [{ type: "image", source: { type: "base64", media_type: "image/png", data: other } }]), []);
  // A picture that is already there needs no write, so it is still named.
  assert.equal(store.put(png, "image/png"), kept);
  assert.equal(warns.length, 2, "only the two that could not be written were said");
  // And a folder that cannot even be looked into is the same story.
  fs.exists = () => { throw new Error("EACCES: permission denied"); };
  assert.equal(store.put(png, "image/png"), "");
  assert.match(warns[2], /EACCES/);
});

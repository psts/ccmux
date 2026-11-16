// node --test threadimage_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const img = require("./threadimage.js");

test("a picture that fits is left alone, a larger one keeps its shape", () => {
  assert.deepEqual(img.fit(1920, 1080), { w: 1920, h: 1080, scaled: false });
  assert.deepEqual(img.fit(2576, 2576), { w: 2576, h: 2576, scaled: false });
  assert.deepEqual(img.fit(5152, 2576), { w: 2576, h: 1288, scaled: true });
  assert.deepEqual(img.fit(3456, 2234), { w: 2576, h: 1665, scaled: true }, "a 16-inch laptop's screenshot");
  assert.deepEqual(img.fit(10, 9000), { w: 3, h: 2576, scaled: true });
  assert.deepEqual(img.fit(1, 100000), { w: 1, h: 2576, scaled: true }, "never thinner than a pixel");
  assert.ok(img.MAX_SIDE <= 3000, "what the page sends must pass the daemon's own limit");
});

test("what is done with a picture: sent as it is, or drawn again", () => {
  const rows = [
    ["image/png", 800, 600, 1000, { act: "send", w: 800, h: 600, scaled: false }],
    ["image/gif", 100, 100, 1000, { act: "send", w: 100, h: 100, scaled: false }],
    ["image/png", 3456, 2234, 1000, { act: "draw", as: "image/png", w: 2576, h: 1665, scaled: true }],
    ["image/jpeg", 6000, 4000, 1000, { act: "draw", as: "image/jpeg", w: 2576, h: 1717, scaled: true }],
    ["image/webp", 800, 600, 1000, { act: "draw", as: "image/png", w: 800, h: 600, scaled: false }],
    ["image/png", 800, 600, img.MAX_BYTES + 1, { act: "draw", as: "image/jpeg", w: 800, h: 600, scaled: false }],
  ];
  for (const [type, w, h, bytes, want] of rows) assert.deepEqual(img.plan(type, w, h, bytes), want, `${type} ${w}x${h} ${bytes}`);
});

test("only the pictures of a paste are taken", () => {
  const dt = { files: [{ type: "image/png", name: "a.png" }, { type: "text/plain", name: "notes.txt" }, { type: "image/webp", name: "b.webp" }] };
  assert.deepEqual(img.pasted(dt).map((f) => f.name), ["a.png", "b.webp"]);
  assert.deepEqual(img.pasted({}), []);
  assert.deepEqual(img.pasted(null), []);
});

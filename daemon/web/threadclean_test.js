// node --test threadclean_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const m = require("./threadclean.js");

// fakeDialog closes with value when shown, as a click on that button does.
function fakeDialog(value) {
  const handlers = [];
  const box = {
    returnValue: "x", shown: 0,
    addEventListener: (ev, fn) => handlers.push(fn),
    showModal() { box.shown++; setTimeout(() => { box.returnValue = value; handlers.splice(0).forEach((fn) => fn()); }, 0); },
  };
  return box;
}

test("the dialog's button decides; Escape (no value) sends nothing", async () => {
  for (const [value, want] of [["clean", "clean"], ["continue", "continue"], ["", null]]) {
    const box = fakeDialog(value);
    let said = "";
    const c = m.create({ dialog: () => box, say: (t) => { said = t; } });
    assert.equal(await c.ask("cold"), want);
    assert.equal(said, "cold");
  }
});

test("a second question while one is open sends nothing, and a page with no dialog just sends", async () => {
  const box = fakeDialog("continue");
  const c = m.create({ dialog: () => box, say: () => {} });
  const first = c.ask("one");
  assert.equal(await c.ask("two"), null);
  assert.equal(await first, "continue");
  assert.equal(box.shown, 1);
  assert.equal(await m.create({ dialog: () => null, say: () => {} }).ask("x"), "continue");
});

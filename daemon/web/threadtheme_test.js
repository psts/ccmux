// node --test threadtheme_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const T = require("./threadtheme.js");

const store = (init = {}) => {
  const m = { ...init };
  return { getItem: (k) => (k in m ? m[k] : null), setItem: (k, v) => { m[k] = String(v); }, m };
};
const broken = { getItem: () => { throw new Error("blocked"); }, setItem: () => { throw new Error("blocked"); } };

test("the choice is read from this browser, and anything else is System", () => {
  assert.equal(T.read(store()), "system");
  assert.equal(T.read(store({ [T.KEY]: "dark" })), "dark");
  assert.equal(T.read(store({ [T.KEY]: "light" })), "light");
  assert.equal(T.read(store({ [T.KEY]: "purple" })), "system", "an unknown value is System");
  assert.equal(T.read(broken), "system", "storage that throws is System, never an error");
  assert.equal(T.read(null), "system");
});

test("System follows the device; Light and Dark override it", () => {
  assert.equal(T.resolve("system", true), "dark");
  assert.equal(T.resolve("system", false), "light");
  assert.equal(T.resolve("light", true), "light");
  assert.equal(T.resolve("dark", false), "dark");
});

test("a choice is saved, and saving where storage throws does not throw", () => {
  const s = store();
  T.save(s, "dark");
  assert.equal(s.m[T.KEY], "dark");
  T.save(s, "nonsense");
  assert.equal(s.m[T.KEY], "system", "only the three choices are saved");
  assert.doesNotThrow(() => T.save(broken, "light"));
});

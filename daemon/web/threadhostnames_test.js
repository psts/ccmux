// node --test threadhostnames_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const H = require("./threadhostnames.js");

test("the dialog opens with the saved names, else the suggested ones, else one empty row", () => {
  const ws = { hostnames: [{ name: "app", port: 3000 }] };
  assert.deepEqual(H.startRows(ws, { suggestions: [{ name: "x", port: 1 }] }), [{ name: "app", port: "3000" }]);
  assert.deepEqual(H.startRows({}, { suggestions: [{ name: "web", port: 5173 }] }), [{ name: "web", port: "5173" }]);
  assert.deepEqual(H.startRows({}, null), [{ name: "", port: "" }]);
});

test("saving skips empty rows and refuses a bad port or a missing name", () => {
  assert.deepEqual(H.parseRows([{ name: " app ", port: " 80 " }, { name: "", port: "" }]), { hostnames: [{ name: "app", port: 80 }] });
  assert.match(H.parseRows([{ name: "app", port: "99999" }]).error, /1 to 65535/);
  assert.match(H.parseRows([{ name: "app", port: "abc" }]).error, /1 to 65535/);
  assert.match(H.parseRows([{ name: "", port: "80" }]).error, /name is missing/);
  assert.deepEqual(H.parseRows([]), { hostnames: [] }, "no rows clears the names");
});

test("a listening port is mapped when a row names it, and a failed read is not 'nothing'", () => {
  const rows = [{ name: "app", port: "3000" }];
  assert.deepEqual(H.liveRows([{ port: 3000, process: "node" }, { port: 5173 }], rows), [
    { port: 3000, process: "node", mapped: true }, { port: 5173, process: "", mapped: false },
  ]);
  assert.match(H.liveNote(null), /Could not read/);
  assert.match(H.liveNote([]), /Nothing yet/);
  assert.equal(H.liveNote([{ port: 1 }]), "");
});

test("map replaces a lone empty row and names the next ones after the repo", () => {
  assert.deepEqual(H.mapRow([{ name: "", port: "" }], "My App", 3000), [{ name: "my-app", port: "3000" }]);
  assert.deepEqual(H.mapRow([{ name: "my-app", port: "3000" }], "My App", 5173), [{ name: "my-app", port: "3000" }, { name: "my-app-2", port: "5173" }]);
  assert.equal(H.suggestLabel("", 0), "app");
  assert.equal(H.suggestLabel("--", 0), "app");
});

test("the menu links only names that have an address, saying where they route", () => {
  const ws = { hostnames: [
    { name: "a", port: 80, url: "https://a.x", listening: true },
    { name: "b", port: 81, url: "https://b.x", livePort: 82 },
    { name: "c", port: 83, url: "https://c.x", heldBy: "blog" },
    { name: "d", port: 84 },
  ] };
  assert.deepEqual(H.links(ws), [
    { label: "● a : 80", url: "https://a.x" }, { label: "○ b : 81 → 82", url: "https://b.x" }, { label: "○ c : 83 (held by blog)", url: "https://c.x" },
  ]);
  assert.deepEqual(H.links(null), []);
});

// The Sandbox tab's own reading of what the daemon answers
// (settingssandbox.js): the host's lines, the form as it is sent, and the
// one line that says what happens to task and thread work.
const test = require("node:test");
const assert = require("node:assert/strict");
const { hostLines, extraPaths, formSettings, verdict } = require("./settingssandbox.js");

const goodHost = { caps: true, walls: true, bwrap: true, socat: true };

test("a host that allows everything reads as four good lines", () => {
  const lines = hostLines(goodHost);
  assert.equal(lines.length, 4);
  assert.ok(lines.every((l) => l.ok));
});

test("what a host lacks is said with the daemon's own reason", () => {
  const lines = hostLines({ caps: true, walls: false, wallsWhy: "it runs in the daemon's own mount namespace", bwrap: false, bwrapWhy: "setting up uid map: Permission denied", socat: false });
  assert.deepEqual(lines.map((l) => l.ok), [true, false, false, false]);
  assert.match(lines[1].text, /Walls do not work here: it runs in the daemon's own mount namespace/);
  assert.match(lines[2].text, /uid map/);
  assert.equal(lines[3].text, "socat is not installed.");
});

test("no host (a failed read) is no lines, never four guessed ones", () => {
  assert.deepEqual(hostLines(undefined), []);
  assert.deepEqual(hostLines(null), []);
});

test("the folders box is one folder per line, blanks dropped", () => {
  assert.deepEqual(extraPaths(" /home/u/.cargo \n\n/opt/cache\n"), ["/home/u/.cargo", "/opt/cache"]);
  assert.deepEqual(extraPaths(""), []);
  assert.deepEqual(extraPaths(undefined), []);
});

test("the form is sent whole, with numbers as numbers", () => {
  const got = formSettings({ on: true, shellBox: false, memoryPercent: "60", cpuCores: "2", tasksMax: "4096", jobHours: "3", extraWrite: "/a\n/b" });
  assert.deepEqual(got, { on: true, shellBox: false, memoryPercent: 60, cpuCores: 2, tasksMax: 4096, jobHours: 3, extraWrite: ["/a", "/b"] });
});

test("an empty number box is sent as 0, which the daemon refuses with its bounds", () => {
  const got = formSettings({ memoryPercent: "", cpuCores: "x", tasksMax: " ", jobHours: undefined });
  assert.deepEqual([got.memoryPercent, got.cpuCores, got.tasksMax, got.jobHours], [0, 0, 0, 0]);
  assert.equal(got.on, false);
});

test("the verdict says what happens to task and thread work as things stand", () => {
  assert.match(verdict({ on: false }, goodHost), /^Off:/);
  assert.match(verdict({ on: true }, goodHost), /^On: each builder/);
  assert.match(verdict({ on: true }, { ...goodHost, walls: false }), /will not start until the host is fixed/);
  assert.match(verdict({ on: true, shellBox: true }, { ...goodHost, socat: false }), /sandbox for shell commands is on too, which this host cannot run/);
  assert.equal(verdict(undefined, goodHost), "", "nothing read, nothing claimed");
  assert.equal(verdict({ on: true }, undefined), "");
});

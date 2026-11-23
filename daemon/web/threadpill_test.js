// node --test threadpill_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const P = require("./threadpill.js");

test("the closed picker shows a choice's short name, the open list keeps the full one", () => {
  assert.equal(P.shortLabel("Opus 5.5 (default)"), "Opus 5.5");
  assert.equal(P.shortLabel("Haiku"), "Haiku");
  assert.equal(P.shortLabel("Settings (Fable)"), "Fable", "the settings' advisor reads as its name");
  assert.equal(P.shortLabel("Settings (none)"), "None");
  assert.equal(P.shortLabel("Settings"), "Settings", "settings not read yet");
  assert.equal(P.shortLabel("Ask first"), "Ask first");
  assert.equal(P.shortLabel(""), "");
  assert.equal(P.shortLabel(undefined), "");
});

test("sync shows each picker's pick and follows the person's change", () => {
  const opt = (value, textContent) => ({ value, textContent });
  const els = {
    model: { value: "opus", children: [opt("haiku", "Haiku"), opt("opus", "Opus 5.5 (default)")] },
    "model-shown": { textContent: "" },
    advisor: { value: "", children: [opt("", "Settings (Fable)"), opt("opus", "Opus 5.5")] },
    "advisor-shown": { textContent: "" },
  };
  P.sync((id) => els[id] || null);
  assert.equal(els["model-shown"].textContent, "Opus 5.5");
  assert.equal(els["advisor-shown"].textContent, "Fable");
  els.model.value = "haiku";
  els.model.oninput();
  assert.equal(els["model-shown"].textContent, "Haiku");
});

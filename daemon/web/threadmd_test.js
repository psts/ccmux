// node --test threadmd_test.js
const { test } = require("node:test");
const assert = require("node:assert/strict");
const md = require("./threadmd.js");

test("a line is text, code, bold and links, in the order written", () => {
  assert.deepEqual(md.spans("Run `go test` **now**, see [docs](https://x.dev/a?b=1)."), [
    { t: "text", text: "Run " }, { t: "code", text: "go test" }, { t: "text", text: " " }, { t: "b", text: "now" },
    { t: "text", text: ", see " }, { t: "a", text: "docs", href: "https://x.dev/a?b=1" }, { t: "text", text: "." },
  ]);
  assert.deepEqual(md.spans("plain"), [{ t: "text", text: "plain" }]);
  assert.deepEqual(md.spans(""), []);
});

test("only a web address becomes a link", () => {
  for (const bad of ["[x](javascript:alert(1))", "[x](data:text/html,hi)", "[x](/v1/settings)", "[x](file:///etc/passwd)"]) {
    assert.deepEqual(md.spans(bad), [{ t: "text", text: bad }], bad);
  }
});

test("a bare web address is a link, without the punctuation after it", () => {
  assert.deepEqual(md.spans("Open https://claude.ai/artifact/AGU9 now."), [
    { t: "text", text: "Open " }, { t: "a", text: "https://claude.ai/artifact/AGU9", href: "https://claude.ai/artifact/AGU9" }, { t: "text", text: " now." },
  ]);
  assert.deepEqual(md.spans("(see http://x.dev/a?b=1), then"), [
    { t: "text", text: "(see " }, { t: "a", text: "http://x.dev/a?b=1", href: "http://x.dev/a?b=1" }, { t: "text", text: "), then" },
  ]);
  assert.deepEqual(md.spans("`https://in.code` and [t](https://x.dev)"), [
    { t: "code", text: "https://in.code" }, { t: "text", text: " and " }, { t: "a", text: "t", href: "https://x.dev" },
  ]);
  for (const bad of ["javascript:alert(1)", "ftp://x.dev", "https://", "file:///etc/passwd"]) {
    assert.ok(md.spans(bad).every((s) => s.t === "text"), bad);
  }
});

test("markup inside a reply stays text", () => {
  const b = md.parse("<img src=x onerror=alert(1)> and **<b>bold</b>**");
  assert.deepEqual(b, [{ type: "p", lines: [[{ t: "text", text: "<img src=x onerror=alert(1)> and " }, { t: "b", text: "<b>bold</b>" }]] }]);
});

test("a reply is paragraphs, headings, list items, code and tables", () => {
  const b = md.parse(["## Plan", "", "Two steps", "on two lines.", "", "- first", "  - nested `x`", "1. numbered", "", "```go", "func main() {}", "", "// **not bold**", "```", "| a | b |", "|---|---|", "after"].join("\n"));
  assert.deepEqual(b.map((x) => x.type), ["h", "p", "li", "li", "li", "code", "table", "p"]);
  assert.deepEqual([b[0].level, b[0].spans[0].text], [2, "Plan"]);
  assert.equal(b[1].lines.length, 2);
  assert.deepEqual([b[2].marker, b[3].depth, b[3].spans[1].t, b[4].marker], ["•", 1, "code", "1."]);
  assert.deepEqual([b[5].lang, b[5].text], ["go", "func main() {}\n\n// **not bold**"]);
  assert.deepEqual([b[6].head.map((c) => c[0].text), b[6].rows], [["a", "b"], []]);
  assert.ok(b.every((x) => !("closed" in x)));
});

test("a blank line ends a paragraph; a code fence still open is the code so far", () => {
  assert.equal(md.parse("one\n\ntwo").length, 2);
  assert.deepEqual(md.parse("look:\n```js\nconst a = 1;"), [{ type: "p", lines: [[{ t: "text", text: "look:" }]] }, { type: "code", lang: "js", text: "const a = 1;" }]);
  assert.deepEqual(md.parse(""), []);
  assert.deepEqual(md.parse(undefined), []);
});

// A document that only records what was made of it.
function fakeDoc() {
  const node = (tag) => ({ tag, children: [], style: {}, dataset: {}, appendChild(c) { this.children.push(c); return c; } });
  return { createElement: node, createTextNode: (text) => ({ tag: "#text", textContent: text, children: [] }) };
}

test("drawing sets text as text and gives a link its guards", () => {
  const box = md.render(fakeDoc(), { children: [], appendChild(c) { this.children.push(c); } }, "See [the docs](https://x.dev) for `<script>`.\n\n```\n<b>raw</b>\n```");
  const [p, pre] = box.children;
  assert.deepEqual(p.children.map((c) => c.tag), ["#text", "a", "#text", "code", "#text"]);
  const a = p.children[1];
  assert.deepEqual([a.href, a.target, a.rel, a.textContent], ["https://x.dev", "_blank", "noopener noreferrer", "the docs"]);
  assert.equal(p.children[3].textContent, "<script>");
  assert.equal(pre.children[0].textContent, "<b>raw</b>");
});

test("a table has its header, its columns' alignment and its cells' own markup", () => {
  const [t] = md.parse(["| Who pays | Affiliate gives 10% | none |", "|---|:---:|--:|", "| ChartLabs (`yours`) | they get **18** | 20 |", "| A \\| B | 10 |"].join("\n"));
  assert.equal(t.type, "table");
  assert.deepEqual(t.align, ["", "center", "right"]);
  assert.deepEqual(t.head.map((c) => c.map((s) => s.text).join("")), ["Who pays", "Affiliate gives 10%", "none"]);
  assert.deepEqual(t.rows[0].map((c) => c.map((s) => s.t)), [["text", "code", "text"], ["text", "b"], ["text"]]);
  assert.deepEqual(t.rows[1].map((c) => c.map((s) => s.text).join("")), ["A | B", "10", ""], "an escaped pipe is text, a short row is filled");
});

test("a table with no |---| row has no header; a line with one pipe is text", () => {
  const [t] = md.parse("| a | b |\n| c | d |");
  assert.equal(t.head, null);
  assert.equal(t.rows.length, 2);
  assert.deepEqual(md.parse("cost | value").map((b) => b.type), ["p"]);
  const streaming = md.parse("| a | b |\n|---|");
  assert.deepEqual([streaming[0].head, streaming[0].rows.length], [null, 2], "half a separator row is a row until it is whole");
});

test("a table is drawn as a table, its cells as text", () => {
  const box = md.render(fakeDoc(), { children: [], appendChild(c) { this.children.push(c); } }, "| x | y |\n|:--|--:|\n| <b>1</b> | `2` |");
  const wrap = box.children[0];
  const table = wrap.children[0];
  assert.deepEqual([wrap.className, table.tag, table.className], ["md-table-wrap", "table", "md-table"]);
  const [thead, tbody] = table.children;
  const th = thead.children[0].children;
  assert.deepEqual(th.map((c) => [c.tag, c.style.textAlign]), [["th", "left"], ["th", "right"]]);
  const td = tbody.children[0].children;
  assert.equal(td[0].children[0].textContent, "<b>1</b>");
  assert.equal(td[1].children[0].tag, "code");
});

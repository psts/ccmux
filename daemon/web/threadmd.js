// threadmd.js: the little of Markdown Claude's replies are written in, read
// into blocks the thread page draws with createElement and textContent.
// Nothing here ever becomes markup: a reply is text from a model that reads
// files and web pages, so the page builds its elements itself and the text
// goes in as text.
//
// Read: fenced code, headings, list items, tables (a header row when the
// second line is |---|, a column's alignment from its colons), paragraphs;
// and inside a line or a table cell `code`, **bold** and [links](https://…).
// Anything else stays as written.
(function (root) {
  "use strict";

  // One pattern for the three inline forms; the first that matches wins.
  const inlineRE = /`([^`\n]+)`|\*\*([^*\n]+)\*\*|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g;

  // spans splits one line of text into {t: "text"|"code"|"b"|"a", text, href}.
  function spans(text) {
    const out = [];
    let last = 0;
    for (const m of String(text).matchAll(inlineRE)) {
      if (m.index > last) out.push({ t: "text", text: text.slice(last, m.index) });
      if (m[1] != null) out.push({ t: "code", text: m[1] });
      else if (m[2] != null) out.push({ t: "b", text: m[2] });
      else out.push({ t: "a", text: m[3], href: m[4] });
      last = m.index + m[0].length;
    }
    if (last < text.length) out.push({ t: "text", text: text.slice(last) });
    return out;
  }

  const fenceRE = /^\s*```\s*([\w+-]*)\s*$/;
  const headRE = /^(#{1,6})\s+(.*)$/;
  const itemRE = /^(\s*)([-*]|\d+[.)])\s+(.*)$/;
  const tableRE = /^\s*\|.*\|\s*$/;

  // lineKind says what one line is outside a code fence.
  function lineKind(line) {
    if (fenceRE.test(line)) return "fence";
    if (!line.trim()) return "blank";
    if (headRE.test(line)) return "h";
    if (itemRE.test(line)) return "li";
    return tableRE.test(line) ? "table" : "p";
  }

  // Each kind of line starts a block or goes on with the one before it.
  const starters = {
    h: (line) => { const m = headRE.exec(line); return { type: "h", level: m[1].length, spans: spans(m[2]) }; },
    li: (line) => { const m = itemRE.exec(line); return { type: "li", depth: Math.floor(m[1].length / 2), marker: /\d/.test(m[2]) ? m[2] : "•", spans: spans(m[3]) }; },
    table: (line, prev) => (prev && prev.type === "table" ? (prev.lines.push(line), null) : { type: "table", lines: [line] }),
    p: (line, prev) => (prev && prev.type === "p" && !prev.closed ? (prev.lines.push(spans(line)), null) : { type: "p", lines: [spans(line)] }),
  };

  // cells splits one table row on its pipes (a \\| is a pipe in the text),
  // without the ones at its two ends.
  function cells(line) {
    const inner = line.trim().replace(/^\|/, "").replace(/(?<!\\)\|$/, "");
    return inner.split(/(?<!\\)\|/).map((c) => c.replace(/\\\|/g, "|").trim());
  }

  const sepRE = /^:?-+:?$/;

  // alignOf is a column's alignment from its separator cell.
  function alignOf(sep) {
    const l = sep.startsWith(":"), r = sep.endsWith(":");
    return l && r ? "center" : r ? "right" : l ? "left" : "";
  }

  // tableOf turns a table's lines into its rows of spans: a header when the
  // second line is the |---| row as wide as the first (a narrower one is
  // still being typed), every row as wide as the widest.
  function tableOf(lines) {
    let rows = lines.map(cells);
    const sep = rows.length > 1 && rows[1].length === rows[0].length && rows[1].every((c) => sepRE.test(c)) ? rows[1] : null;
    const head = sep ? rows[0] : null;
    rows = sep ? rows.slice(2) : rows;
    const width = Math.max(head ? head.length : 0, ...rows.map((r) => r.length));
    const fill = (r) => Array.from({ length: width }, (_, i) => spans(r[i] || ""));
    const align = Array.from({ length: width }, (_, i) => (sep && sep[i] ? alignOf(sep[i]) : ""));
    return { type: "table", align, head: head ? fill(head) : null, rows: rows.map(fill) };
  }

  // parse reads a reply into blocks:
  //   {type: "p", lines: [spans…]}   {type: "h", level, spans}
  //   {type: "li", depth, marker, spans}   {type: "code", lang, text}
  //   {type: "table", align: [""|"left"|"center"|"right"…], head: [spans…] or null, rows: [[spans…]…]}
  // A fence still open at the end (the reply is streaming) is a code block
  // so far.
  function parse(text) {
    const out = [];
    let code = null;
    for (const line of String(text || "").split("\n")) {
      if (code) {
        if (fenceRE.test(line)) { out.push(code); code = null; } else code.text += (code.text ? "\n" : "") + line;
        continue;
      }
      const kind = lineKind(line);
      const prev = out[out.length - 1];
      if (kind === "fence") code = { type: "code", lang: fenceRE.exec(line)[1], text: "" };
      else if (kind === "blank") { if (prev) prev.closed = true; } else {
        const made = starters[kind](line, prev && !prev.closed ? prev : null);
        if (made) out.push(made);
      }
    }
    if (code) out.push(code);
    return out.map(({ closed, ...b }) => (b.type === "table" ? tableOf(b.lines) : b));
  }

  // render draws the blocks into parent with doc's createElement.
  function render(doc, parent, text) {
    const el = (tag, cls, t) => { const e = doc.createElement(tag); if (cls) e.className = cls; if (t != null) e.textContent = t; return e; };
    const inline = (box, list) => {
      for (const s of list) {
        if (s.t === "text") box.appendChild(doc.createTextNode(s.text));
        else if (s.t === "code") box.appendChild(el("code", "", s.text));
        else if (s.t === "b") box.appendChild(el("strong", "", s.text));
        else { const a = el("a", "", s.text); a.href = s.href; a.target = "_blank"; a.rel = "noopener noreferrer"; box.appendChild(a); }
      }
      return box;
    };
    const draw = {
      p: (b) => { const p = el("p", "md-p"); b.lines.forEach((l, i) => { if (i) p.appendChild(el("br")); inline(p, l); }); return p; },
      h: (b) => inline(el("div", "md-h md-h" + Math.min(b.level, 3)), b.spans),
      li: (b) => { const li = el("div", "md-li"); li.style.marginLeft = b.depth * 16 + "px"; li.appendChild(el("span", "md-mark", b.marker)); li.appendChild(inline(el("span", "md-item"), b.spans)); return li; },
      code: (b) => { const pre = el("pre", "md-code"); if (b.lang) pre.dataset.lang = b.lang; pre.appendChild(el("code", "", b.text)); return pre; },
      table: (b) => {
        const row = (tag, list) => { const tr = el("tr"); list.forEach((c, i) => { const td = inline(el(tag), c); if (b.align[i]) td.style.textAlign = b.align[i]; tr.appendChild(td); }); return tr; };
        const table = el("table", "md-table");
        if (b.head) table.appendChild(el("thead")).appendChild(row("th", b.head));
        const body = table.appendChild(el("tbody"));
        b.rows.forEach((r) => body.appendChild(row("td", r)));
        const wrap = el("div", "md-table-wrap"); // a wide table scrolls, the page does not
        wrap.appendChild(table);
        return wrap;
      },
    };
    for (const b of parse(text)) parent.appendChild(draw[b.type](b));
    return parent;
  }

  const api = { spans, parse, render };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.ccmuxThreadMD = api;
})(typeof window !== "undefined" ? window : this);

// node --test mcp.test.mjs — the base's mcp.json mapped onto the SDK's
// servers and the allow rules for their tools.
import { test } from "node:test";
import assert from "node:assert/strict";
import { expand, mcpFromNeutral, PEERS_RULE } from "./mcp.mjs";

test("${VAR} expands from the environment, an unset one stays as written, nothing else is a reference", () => {
  const env = { X_MCP_URL: "http://100.100.67.87:8931/mcp", EMPTY: "" };
  assert.equal(expand("${X_MCP_URL}", env), "http://100.100.67.87:8931/mcp");
  assert.equal(expand("${EMPTY}", env), "", "set-but-empty is set, not missing");
  assert.equal(expand("${MISSING}/x", env), "${MISSING}/x");
  assert.equal(expand("plain $HOME {x} ${A:-d}", env), "plain $HOME {x} ${A:-d}", "only the ${NAME} form is a reference; no default form");
});

test("a local entry becomes a stdio server, a remote one an http (or sse) server, and every server gets an allow rule", () => {
  const env = { X_MCP_URL: "http://door/mcp", KB_TOKEN: "k1" };
  const { servers, allow } = mcpFromNeutral(JSON.stringify({
    kb: { command: "kb-mcp", args: ["--token", "${KB_TOKEN}"], env: { KB_TOKEN: "${KB_TOKEN}" } },
    door: { type: "http", url: "${X_MCP_URL}", headers: { authorization: "Bearer ${KB_TOKEN}" } },
    old: { type: "sse", url: "http://h/sse" },
    bare: { url: "http://h/mcp" },
  }), env);
  assert.deepEqual(servers.kb, { type: "stdio", command: "kb-mcp", args: ["--token", "k1"], env: { KB_TOKEN: "k1" } });
  assert.deepEqual(servers.door, { type: "http", url: "http://door/mcp", headers: { authorization: "Bearer k1" } });
  assert.deepEqual(servers.old, { type: "sse", url: "http://h/sse" });
  assert.deepEqual(servers.bare, { type: "http", url: "http://h/mcp" }, "a url with no type is http");
  assert.equal(servers.bare.headers, undefined, "no empty headers object");
  assert.deepEqual(allow, [PEERS_RULE, "mcp__kb", "mcp__door", "mcp__old", "mcp__bare"]);
});

test("a reference the environment does not set is named per server, so the pane can say why a server failed", () => {
  const { servers, unresolved } = mcpFromNeutral(JSON.stringify({
    door: { url: "${X_MCP_URL}", headers: { authorization: "Bearer ${TOKEN}", other: "${TOKEN}" } },
    kb: { command: "kb-mcp", env: { KB_TOKEN: "${KB_TOKEN}" } },
    fine: { url: "${X_MCP_URL}/fine" },
  }), { X_MCP_URL: "http://door" });
  assert.deepEqual(unresolved, { door: ["TOKEN"], kb: ["KB_TOKEN"] }, "each missing name once, resolved ones absent");
  assert.equal(servers.door.headers.authorization, "Bearer ${TOKEN}", "the literal stays; the report is beside it, not instead of it");
});

test("no file, an empty file and {} all mean no servers but still the peers rule", () => {
  for (const text of ["", "  \n", "{}"]) {
    assert.deepEqual(mcpFromNeutral(text, {}), { servers: {}, allow: [PEERS_RULE], unresolved: {} }, JSON.stringify(text));
  }
});

test("a file that is not an object of servers fails loudly", () => {
  assert.throws(() => mcpFromNeutral("[]", {}), /JSON object of servers/);
  assert.throws(() => mcpFromNeutral('{"x": {"args": ["a"]}}', {}), /server "x" needs a command or a url/);
  assert.throws(() => mcpFromNeutral("{bad", {}), SyntaxError);
});

// A session runner's loadout: what its group and repo chose from the
// capability catalog (the daemon's internal/loadout and agent/loadout.go),
// handed over as one JSON file (--loadout) because it does not fit a
// command line. The session reads the user's own settings, where CLAUDE.md
// and memory come from, so "load only this" is said as switches on top of
// them, each seen to hold on SDK 0.3.311 with Claude Code 2.1.326:
//
//   pluginDirs      plugin folders loaded by path (options.plugins)
//   enabledPlugins  every plugin id the host knows, on or off, in the
//                   session's own settings (options.settings), which win
//                   over the user's and the repo's settings files
//   skills          the only skills offered to the model (options.skills);
//                   one typed as /name still runs
//   deniedServers   MCP servers and connectors refused by name
//                   (settings.deniedMcpServers)
//   connectors      false switches every claude.ai connector off
//                   (ENABLE_CLAUDEAI_MCP_SERVERS=false)
//   mcp             chosen catalog servers, in the neutral mcp.json shape
//
// strictMcpConfig is never used: it drops every server, the bus too.
// Pure: no I/O, no SDK import.
import { PEERS_SERVER } from "./mcp.mjs";

function strings(raw, name) {
  const v = raw[name];
  if (!Array.isArray(v) || v.some((x) => typeof x !== "string")) throw new Error(`the loadout file's ${name} must be a list of strings`);
  return v;
}

function object(raw, name) {
  const v = raw[name];
  if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error(`the loadout file's ${name} must be an object`);
  return v;
}

// parseLoadout reads the file's text. Every field must be there: a missing
// skills list would mean "every skill", the opposite of an empty one, so a
// file the daemon and the sidecar disagree on fails the start instead of
// loading everything. The bus is the session's fixed base, so its server is
// dropped from the denied names and from the chosen servers whatever the
// file says.
export function parseLoadout(text) {
  const raw = JSON.parse(text);
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("the loadout file must be a JSON object");
  const enabledPlugins = object(raw, "enabledPlugins");
  if (Object.values(enabledPlugins).some((v) => typeof v !== "boolean")) throw new Error("the loadout file's enabledPlugins must map plugin ids onto true or false");
  if (typeof raw.connectors !== "boolean") throw new Error("the loadout file's connectors must be true or false");
  const mcp = { ...object(raw, "mcp") };
  delete mcp[PEERS_SERVER];
  return {
    pluginDirs: strings(raw, "pluginDirs"),
    enabledPlugins,
    skills: strings(raw, "skills"),
    deniedServers: strings(raw, "deniedServers").filter((name) => name !== PEERS_SERVER),
    connectors: raw.connectors,
    mcp,
  };
}

// applyLoadout puts a loadout onto the SDK options of a query. env is the
// process environment, which options.env replaces whole when set.
export function applyLoadout(opts, lo, env) {
  if (lo.pluginDirs.length) opts.plugins = (opts.plugins || []).concat(lo.pluginDirs.map((path) => ({ type: "local", path })));
  const settings = { ...(opts.settings || {}) };
  if (Object.keys(lo.enabledPlugins).length) settings.enabledPlugins = lo.enabledPlugins;
  if (lo.deniedServers.length) settings.deniedMcpServers = lo.deniedServers.map((serverName) => ({ serverName }));
  if (Object.keys(settings).length) opts.settings = settings;
  opts.skills = lo.skills;
  if (!lo.connectors) opts.env = { ...(opts.env || env), ENABLE_CLAUDEAI_MCP_SERVERS: "false" };
  return opts;
}

// describeLoadout is the one line a runner's log says about it at start.
export function describeLoadout(lo) {
  const on = Object.values(lo.enabledPlugins).filter(Boolean).length;
  const off = Object.keys(lo.enabledPlugins).length - on;
  return `loadout: ${on + lo.pluginDirs.length} plugin(s) on, ${off} off, ${lo.skills.length} skill(s) offered, ${lo.deniedServers.length} server(s) denied, ` +
    `${Object.keys(lo.mcp).length} catalog server(s), claude.ai connectors ${lo.connectors ? "as chosen" : "off"}`;
}

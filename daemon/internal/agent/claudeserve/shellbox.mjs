// Claude Code's own sandbox for shell commands (bubblewrap on Linux), for a
// session the daemon has walled: --shell-box-write DIR, once per folder the
// session's walls open. The daemon passes the flag only when the Sandbox
// setting is on and the host has what the sandbox needs (bwrap that works,
// socat); the session's unit is walled by systemd either way, so this is a
// second wall inside the first, around Bash alone.
//
// What it adds: the systemd walls cannot tell Claude Code from the commands
// it runs, so both may write Claude Code's own folder. Here a command may
// not write its settings (Claude Code keeps those out of every sandbox).
//
// What it leaves alone, on purpose:
//   - permissions: a sandboxed command is asked about exactly as an open
//     one is (autoAllowBashIfSandboxed off), so the session's own mode and
//     its fences decide, as before.
//   - the network: no list of allowed hosts is given. A builder fetches
//     modules from wherever its repo's checks say and nobody is there to
//     answer a prompt for a new host; and it talks to its own dev servers
//     on loopback, which a network sandbox would cut off.
// No SDK import: shellbox.test.mjs runs it as it is.

// shellBox is the SDK's sandbox option for a session whose walls open
// roots, or undefined for a session with none.
export function shellBox(roots) {
  if (!Array.isArray(roots) || roots.length === 0) return undefined;
  return {
    enabled: true,
    // A session that was to be sandboxed and cannot be says so and stops:
    // it never falls back to running its commands open.
    failIfUnavailable: true,
    // No way out through the tool's own dangerouslyDisableSandbox switch.
    allowUnsandboxedCommands: false,
    autoAllowBashIfSandboxed: false,
    filesystem: { allowWrite: roots.slice() },
  };
}

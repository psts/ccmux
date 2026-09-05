// ccmux opencode plugin: tells the daemon when this agent pane is busy or
// idle, which is what the lifecycle loop needs to put an idle agent to sleep
// (docs/agent-spec.md §8). Claude Code panes report the same through the
// hooks socket; opencode has no hooks, so this plugin is the equivalent.
//
// Written by ccmuxd into the agents root and listed in every instance's
// opencode.jsonc. It only speaks to the daemon on loopback, using the pane
// identity the pane's own environment carries.
import type { Plugin } from "@opencode-ai/plugin"

export const CcmuxPlugin: Plugin = async () => {
  const daemon = process.env.CCMUX_DAEMON_URL
  const pane = process.env.CCMUX_PANE_ID
  if (!daemon || !pane) return {}
  const signal = async (state: "busy" | "idle") => {
    try {
      await fetch(`${daemon}/v1/panes/${pane}/agent-signal`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ state }),
      })
    } catch {
      // The daemon being away is not the agent's problem.
    }
  }
  return {
    event: async ({ event }: { event: { type: string; properties?: any } }) => {
      switch (event.type) {
        case "session.idle":
        case "session.error":
          await signal("idle")
          break
        case "message.updated":
          if (event.properties?.info?.role === "user") await signal("busy")
          break
      }
    },
  }
}

export default CcmuxPlugin

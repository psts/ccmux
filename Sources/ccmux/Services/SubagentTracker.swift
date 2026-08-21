import Foundation

/// Which subagents each Claude session still has running.
///
/// It exists because Claude Code's idle reminder fires 60 seconds after the main
/// loop stops talking, whether or not that loop is waiting on background agents.
/// Nothing in the notification payload distinguishes the two, so the only way to
/// tell "the turn ended" from "the turn is waiting" is to count the agents.
///
/// Pairing is by agent id and nothing else. A running subagent also emits a
/// SubagentStop at every turn of its own inner loop, each with an id that never
/// appeared in a start (1793 of them against 548 real starts in one trace file),
/// so a stop for an id we are not tracking is discarded. `prompt_id` looks like a
/// tempting alternative and is not one: it is restamped whenever a new prompt is
/// submitted, and background agents routinely outlive the turn that spawned them.
///
/// The Go daemon keeps this state for hosted panes
/// (`daemon/internal/hooks/agents.go`) and is the LEAD implementation; this
/// copy serves the app's own local panes, which never reach the daemon's
/// socket, and deliberately lags it (no prompt tracking, single-session sweep,
/// no expiry trace) pending the planned parity refactor. Treat the Go file as
/// the reference when they disagree.
///
/// All mutation happens on the main thread — `ClaudeHookListener` hops to main
/// before handling a message — following `ClaudeAttentionMonitor`'s convention.
final class SubagentTracker {

    /// How long one unfinished subagent can hold a session's alerts back. A
    /// subagent that never reports its stop would otherwise mute the workspace
    /// for as long as the app runs.
    ///
    /// Measured over 548 subagent runs in one trace file: 3 never sent a stop
    /// (one left an agent "running" for 15 hours), and the longest agent that DID
    /// finish took 18 minutes. 30 minutes clears every real run seen while
    /// capping a lost stop at half an hour.
    ///
    /// What the timeout restores is the session's NEXT turn-ending event, not the
    /// one that was already held.
    ///
    /// A HELD EVENT IS FINAL — the one thing the design gives up, written down
    /// because the obvious "fix" is worse and someone will otherwise implement it.
    /// Nothing re-applies a held event when the last subagent drains, so the alert
    /// waits for the session's next genuine idle reminder. Replaying 302 held
    /// events from one trace file, 2 never got one: both were sessions whose
    /// subagent leaked and never reported a stop, so the loss is the leak, not the
    /// drop. Re-delivering on drain is the tempting alternative and costs more
    /// than it saves: when the main loop does resume after its agents report, its
    /// next Stop is a median 252 seconds away — only 32% arrive within a minute,
    /// 57% within five. A release timer short enough to be a useful alert would
    /// fire while the model was demonstrably still working, in roughly two thirds
    /// of the cases it fired at all, reinstating the false "needs your input" this
    /// whole type exists to remove.
    static let agentTimeout: TimeInterval = 30 * 60

    private var open: [String: [String: Date]] = [:]  // session id → agent id → started

    /// Record a subagent as running; returns how many the session now has. A
    /// session id is required: without one there is nothing to attribute the
    /// agent to, and a shared empty key would pool every anonymous session.
    @discardableResult
    func start(session: String, agent: String, now: Date = Date()) -> Int {
        guard !session.isEmpty, !agent.isEmpty else { return 0 }
        open[session, default: [:]][agent] = now
        return open[session]?.count ?? 0
    }

    /// Clear a subagent. `known` is false for the inner-loop stops described on
    /// the type: routine, not an error.
    @discardableResult
    func stop(session: String, agent: String) -> (known: Bool, left: Int) {
        guard open[session]?[agent] != nil else { return (false, open[session]?.count ?? 0) }
        open[session]?[agent] = nil
        let left = open[session]?.count ?? 0
        if left == 0 { open[session] = nil }
        return (true, left)
    }

    /// Forget a session's agents wholesale, for a session that just started or
    /// ended. Deliberately NOT called on a new user prompt: a background agent
    /// commonly reports its stop after the next prompt has been submitted, so
    /// clearing there would drop agents that are genuinely still running.
    func clear(session: String) {
        open[session] = nil
    }

    /// How many subagents a session still has running, dropping any that have
    /// outlived `agentTimeout` first.
    func busy(session: String, now: Date = Date()) -> Int {
        guard var running = open[session] else { return 0 }
        let cutoff = now.addingTimeInterval(-Self.agentTimeout)
        running = running.filter { $0.value >= cutoff }
        open[session] = running.isEmpty ? nil : running
        return running.count
    }

    /// A count for the trace, where these lines are read next to the
    /// notification they explain.
    static func describe(_ count: Int) -> String {
        count == 1 ? "1 subagent running" : "\(count) subagents running"
    }
}

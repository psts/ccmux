import Foundation

/// Tracks a Claude session's live background agents, so a `Stop` can be told apart
/// from a finish.
///
/// Claude Code fires `Stop` when the main agent stops emitting text. With
/// background agents outstanding that is not the end of the work: each agent that
/// finishes wakes the main agent, which talks and stops again. One observed run
/// produced seven `Stop`s in three minutes for a single piece of work, every one
/// of them announcing "finished a task" while up to seven agents still ran.
///
/// The Go twin is `daemon/internal/hooks/agents.go`. Both exist because hosted
/// panes reach the daemon and local panes reach this app; a rule in only one of
/// them would make the two kinds of pane behave differently.
///
/// The count is deliberately one-sided: an agent is live only if its START was
/// seen. `SubagentStop` fires for agents `SubagentStart` never announced — 229
/// stops against 87 starts in a day's log — so a stop for an unknown id is
/// ignored. The count can then never go negative, and never suppresses a real
/// finish because of an agent that was never seen starting.
final class BackgroundAgentTracker {
    /// What `observe` concluded, so the caller knows whether to stop there.
    enum Outcome: Equatable {
        /// Pure agent bookkeeping; the event carries no other meaning.
        case tracked(detail: String)
        /// Not an agent event (or one that still means something else too).
        case notAnAgentEvent
    }

    /// Bounds a leak. An agent that starts and never stops would otherwise silence
    /// its session's done alerts forever, and a real leaked id was present in the
    /// log this was built from. The worst case at ten minutes is an alert you'd
    /// have preferred suppressed, not silence.
    static let agentTTL: TimeInterval = 600

    private var live: [String: [String: Date]] = [:] // session -> agent -> started
    private let now: () -> Date

    init(now: @escaping () -> Date = Date.init) {
        self.now = now
    }

    /// Fold a hook event into the tracker.
    func observe(event: String, sessionId: String, agentId: String) -> Outcome {
        switch event {
        case "subagent_start":
            return .tracked(detail: start(sessionId: sessionId, agentId: agentId))
        case "subagent_stop":
            return .tracked(detail: stop(sessionId: sessionId, agentId: agentId))
        case "session_start":
            // A restarted session cannot still be waiting on the previous one's
            // agents, and their stops will never arrive. Not consumed — the caller
            // still has its own use for a session start.
            live[sessionId] = nil
            return .notAnAgentEvent
        default:
            return .notAnAgentEvent
        }
    }

    /// The session's live agent count, dropping any that outlived `agentTTL`.
    /// Expiry happens on read because that is the only moment the count matters.
    func outstanding(sessionId: String) -> Int {
        guard !sessionId.isEmpty, var agents = live[sessionId] else { return 0 }
        let cutoff = now().addingTimeInterval(-Self.agentTTL)
        agents = agents.filter { $0.value >= cutoff }
        live[sessionId] = agents.isEmpty ? nil : agents
        return agents.count
    }

    private func start(sessionId: String, agentId: String) -> String {
        guard !sessionId.isEmpty, !agentId.isEmpty else {
            return "unattributable agent (no session or agent id)"
        }
        live[sessionId, default: [:]][agentId] = now()
        return "\(live[sessionId]?.count ?? 0) background agent(s) now live"
    }

    private func stop(sessionId: String, agentId: String) -> String {
        guard !sessionId.isEmpty, !agentId.isEmpty else {
            return "unattributable agent (no session or agent id)"
        }
        guard live[sessionId]?[agentId] != nil else {
            return "stop for an agent we never saw start; ignored"
        }
        live[sessionId]?[agentId] = nil
        let remaining = live[sessionId]?.count ?? 0
        if remaining == 0 { live[sessionId] = nil }
        return "\(remaining) background agent(s) still live"
    }
}

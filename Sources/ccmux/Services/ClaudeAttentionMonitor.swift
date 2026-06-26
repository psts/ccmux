import Foundation
import Combine

/// Visual attention signal for a workspace, driven by Claude Code hook events.
enum AttentionState: Equatable {
    /// No signal — row renders normally.
    case none
    /// Claude is blocked waiting for the user (permission / question / idle prompt).
    case needsInput
    /// Claude finished a turn and is waiting for the next message.
    case done
}

/// Per-workspace attention state with an auto-clearing timer, so the sidebar
/// highlight doesn't linger forever once you stop looking. Mirrors the
/// per-workspace ObservableObject pattern of `ClaudeProcessMonitor`/`GitStatusMonitor`.
///
/// Fed by `ClaudeHookListener`; observed by `AttentionRowBackground` in the sidebar.
/// All mutation happens on the main thread (callers guarantee this — the listener
/// hops to main before touching state, and the focus-clear paths are UI code).
final class ClaudeAttentionMonitor: ObservableObject {
    @Published private(set) var state: AttentionState = .none

    /// Shared placeholder for sidebar rows whose workspace has no monitor yet.
    static let empty = ClaudeAttentionMonitor()

    /// `needsInput` is a genuine block Claude won't re-announce, so it lingers
    /// longer than the informational `done` tint. Both also clear on focus.
    static let needsInputTimeout: TimeInterval = 60
    static let doneTimeout: TimeInterval = 12

    private var timer: DispatchSourceTimer?

    deinit { timer?.cancel() }

    /// Set a new state and (re)arm the auto-clear timer. `.none` clears immediately.
    func set(_ newState: AttentionState) {
        timer?.cancel()
        timer = nil

        guard newState != .none else {
            if state != .none { state = .none }
            return
        }

        state = newState
        let timeout = newState == .needsInput ? Self.needsInputTimeout : Self.doneTimeout
        let t = DispatchSource.makeTimerSource(queue: .main)
        t.schedule(deadline: .now() + timeout)
        t.setEventHandler { [weak self] in
            self?.state = .none
            self?.timer = nil
        }
        t.resume()
        timer = t
    }

    /// The "I've seen it" clear — used on focus/switch-to.
    func clear() {
        set(.none)
    }

    /// Cancel the pending timer for lifecycle teardown (workspace closed/removed).
    func stop() {
        timer?.cancel()
        timer = nil
    }
}

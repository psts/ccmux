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

/// Per-workspace attention state. A flash holds until the workspace is looked
/// at — here, or on any other lens, which the daemon reports as an attention
/// change to idle. It used to fade on a timer (60s / 12s), which is exactly
/// what "blink until it gets attention" rules out. Mirrors the per-workspace
/// ObservableObject pattern of `ClaudeProcessMonitor`/`GitStatusMonitor`.
///
/// Fed by `ClaudeHookListener` and `RemoteSessionService`; observed by
/// `AttentionRowBackground` in the sidebar. All mutation happens on the main
/// thread (callers guarantee this — the listener hops to main before touching
/// state, and the focus-clear paths are UI code).
final class ClaudeAttentionMonitor: ObservableObject {
    @Published private(set) var state: AttentionState = .none

    /// Shared placeholder for sidebar rows whose workspace has no monitor yet.
    static let empty = ClaudeAttentionMonitor()

    /// Set a new state; `.none` clears. Publishes only on a real change so the
    /// pulse animation is not restarted by a repeated hook.
    func set(_ newState: AttentionState) {
        if state != newState { state = newState }
    }

    /// The "I've seen it" clear — used on focus/switch-to.
    func clear() {
        set(.none)
    }

    /// Lifecycle teardown (workspace closed/removed). Nothing pending to cancel
    /// any more; kept so the call sites read as intent.
    func stop() {
        set(.none)
    }
}

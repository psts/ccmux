import Foundation
import UserNotifications

/// Thin wrapper over `UNUserNotificationCenter` for workspace attention alerts.
/// Each notification embeds the workspace UUID in `userInfo` so the click handler
/// (in `AppDelegate`) can navigate to it. Uses a stable per-workspace identifier so
/// a newer alert replaces the older one instead of stacking.
///
/// `UNUserNotificationCenter` requires a bundled app; the plain `swift build`
/// executable has no bundle identifier, so every call no-ops there.
final class AttentionNotifier {
    static let workspaceIdKey = "ccmux.workspaceId"

    /// Whether a state is worth an alert, as opposed to just a sidebar flash.
    ///
    /// `needsInput` is, and so is any flash still showing when Claude Code's idle
    /// reminder re-sends it (`remind`). A `done` on its own is not: it comes from
    /// the Stop hook alone, and Stop fires
    /// when Claude finishes *responding*, not when the work is finished: a turn
    /// routinely carries on afterwards, and a session whose background agents keep
    /// reporting back produces a Stop for each one. The signal that really means
    /// "finished, nothing more coming" is Claude Code's own `idle_prompt`, which
    /// arrives 60s after the last Stop and resets on every new one. It leaves a
    /// done row green and alerts for it here, but only while the row's flash is
    /// still showing: a look within the minute retires both.
    ///
    /// This lives on the notifier rather than at the call sites because there are
    /// two of them — the local hook listener and the hosted firehose — and they
    /// have already drifted apart once. `post` enforces it, so neither can alert
    /// on a state this returns false for. The daemon's `notifyState` is the same
    /// rule for web push.
    static func alerts(_ state: AttentionState, remind: Bool = false) -> Bool {
        state == .needsInput || (remind && state != .none)
    }

    /// Notifications only work from the `.app` bundle (needs a CFBundleIdentifier).
    private var isAvailable: Bool { Bundle.main.bundleIdentifier != nil }

    func requestAuthorization() {
        guard isAvailable else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, error in
            if let error { NSLog("[ccmux notify] authorization failed: \(error.localizedDescription)") }
        }
    }

    func post(for workspace: Workspace, state: AttentionState, remind: Bool = false) {
        guard isAvailable, Self.alerts(state, remind: remind) else { return }

        let content = UNMutableNotificationContent()
        switch state {
        case .needsInput:
            content.title = "\(workspace.name) needs input"
            content.body = "Claude is waiting for you"
            content.sound = .default
        case .done:
            content.title = "\(workspace.name) finished"
            content.body = "Claude completed a turn"
        case .none:
            return
        }
        content.userInfo = [Self.workspaceIdKey: workspace.id.uuidString]

        let request = UNNotificationRequest(
            identifier: "ccmux.attention.\(workspace.id.uuidString)",
            content: content,
            trigger: nil
        )
        UNUserNotificationCenter.current().add(request)
    }
}

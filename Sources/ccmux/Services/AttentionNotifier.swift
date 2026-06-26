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

    /// Notifications only work from the `.app` bundle (needs a CFBundleIdentifier).
    private var isAvailable: Bool { Bundle.main.bundleIdentifier != nil }

    func requestAuthorization() {
        guard isAvailable else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { _, error in
            if let error { NSLog("[ccmux notify] authorization failed: \(error.localizedDescription)") }
        }
    }

    func post(for workspace: Workspace, state: AttentionState) {
        guard isAvailable else { return }

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

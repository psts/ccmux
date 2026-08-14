import Foundation

/// Hands keyboard focus to the terminal of a just-clicked pane tab, so one click on
/// the tab bar is enough to start typing.
///
/// A tab click only moves app state (`SplitTreeController.focusedPaneId` and
/// `PaneTabs.activeTabId`); AppKit leaves the first responder on the SwiftUI tab bar.
/// The click can't reach the terminal view directly either: `PaneContentView` keys its
/// content on the active tab id, so switching tabs tears the terminal's container down
/// and rebuilds it — at click time the view that should take focus may not be in a
/// window yet.
///
/// So a request is both broadcast (claimed immediately by a terminal already on screen)
/// and parked (claimed by one still being embedded). It expires so a request no
/// terminal ever claimed — a tab whose content is a browser, say — can't surface later
/// and pull focus out of whatever the user is actually typing in.
///
/// Main thread only; every caller is a SwiftUI closure or an NSView.
final class PaneFocusCoordinator {
    static let shared = PaneFocusCoordinator()

    /// Posted with the terminal tab id (a `UUID`) as `object`.
    static let didRequestFocus = Notification.Name("ccmux.PaneFocusCoordinator.didRequestFocus")

    /// How long a parked request stays claimable. Long enough to cover a tab's
    /// teardown-and-rebuild, short enough that it can't outlive the click that made it.
    static let requestLifetime: TimeInterval = 2

    private var pending: (tabId: UUID, madeAt: Date)?
    private let now: () -> Date

    init(now: @escaping () -> Date = Date.init) {
        self.now = now
    }

    /// Ask the terminal owning `tabId` to take keyboard focus.
    func requestFocus(tabId: UUID) {
        pending = (tabId, now())
        NotificationCenter.default.post(name: Self.didRequestFocus, object: tabId)
    }

    /// True at most once per request, and only for the terminal the request named.
    /// An expired request is dropped rather than granted.
    func claim(tabId: UUID) -> Bool {
        guard let request = pending, request.tabId == tabId else { return false }
        pending = nil
        return now().timeIntervalSince(request.madeAt) <= Self.requestLifetime
    }
}

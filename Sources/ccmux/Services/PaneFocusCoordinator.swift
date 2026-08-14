import AppKit
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
/// and parked (claimed by one still being embedded). It expires because a terminal tab
/// can be rebuilt long after the click that named it — a pane scrolled out of view, a
/// delayed re-embed — and claiming then would yank focus out of whatever the user has
/// moved on to. A request no terminal ever holds, a browser tab's say, is simply never
/// claimed.
///
/// `@MainActor` rather than a comment saying "main thread only": every caller is
/// already a SwiftUI closure, an NSView method, or a `queue: .main` notification block,
/// so the annotation costs nothing and makes the rule a compile error.
@MainActor
final class PaneFocusCoordinator {
    static let shared = PaneFocusCoordinator()

    /// Posted with the terminal tab id (a `UUID`) as `object`.
    static let didRequestFocus = Notification.Name("ccmux.PaneFocusCoordinator.didRequestFocus")

    /// How long a parked request stays claimable. Long enough to cover a tab's
    /// teardown-and-rebuild, short enough that it can't outlive the click that made it.
    static let requestLifetime: TimeInterval = 2

    private var pending: (tabId: UUID, madeAt: Date)?
    private let now: () -> Date

    /// Private so `shared` is the only instance production can reach. A second
    /// coordinator would compile and then focus nothing at all, because every claim
    /// goes through `shared` — an invariant worth having the compiler hold rather
    /// than a comment.
    private init(now: @escaping () -> Date = Date.init) {
        self.now = now
    }

    #if DEBUG
    /// Test seam: the same type with a clock you control. Not reachable from a
    /// release build, so it cannot become a second production instance.
    static func makeForTesting(now: @escaping () -> Date) -> PaneFocusCoordinator {
        PaneFocusCoordinator(now: now)
    }
    #endif

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

/// One terminal container's end of the focus handshake: it listens for requests
/// naming `tabId` and hands first responder to the terminal when one arrives.
///
/// Both container views (local and daemon-hosted) need the identical three-part
/// protocol — observe, tear the observer down, claim-then-focus — and differ only in
/// how they reach their terminal view. Keeping the policy here means the claim
/// ordering, which is subtle, is written once.
@MainActor
final class PaneFocusClaim {
    private let tabId: UUID
    private weak var host: NSView?
    private let terminal: () -> NSView?
    private var observer: NSObjectProtocol?

    /// - Parameters:
    ///   - host: the container the terminal must currently be embedded in.
    ///   - terminal: looks up the terminal view; may be nil before it exists.
    init(tabId: UUID, host: NSView, terminal: @escaping () -> NSView?) {
        self.tabId = tabId
        self.host = host
        self.terminal = terminal
        // A tab click on an already-embedded terminal never re-enters embed or
        // layout, so the broadcast is the only way that case learns it should focus.
        observer = NotificationCenter.default.addObserver(
            forName: PaneFocusCoordinator.didRequestFocus, object: nil, queue: .main
        ) { [weak self] note in
            guard note.object as? UUID == tabId else { return }
            MainActor.assumeIsolated { self?.claimIfRequested() }
        }
    }

    deinit {
        if let observer { NotificationCenter.default.removeObserver(observer) }
    }

    /// Make the terminal first responder if a tab click asked for it.
    ///
    /// Claiming comes LAST, after the window and embedding checks, so a request is
    /// only consumed by a container that can actually act on it. That ordering also
    /// settles the case where a reused terminal view is briefly observed by two
    /// containers with the same tab id: only the one that currently holds it claims.
    func claimIfRequested() {
        guard let host, let window = host.window else { return }
        guard let terminal = terminal(), terminal.superview === host else { return }
        guard PaneFocusCoordinator.shared.claim(tabId: tabId) else { return }
        if !window.makeFirstResponder(terminal) {
            // The claim is already spent, so this is unrecoverable and otherwise
            // looks exactly like an expired or unclaimed request: three different
            // bugs, one symptom of clicking a tab and typing into nothing.
            NSLog("[ccmux focus] tab \(tabId): AppKit refused first responder; terminal stays unfocused")
        }
    }
}

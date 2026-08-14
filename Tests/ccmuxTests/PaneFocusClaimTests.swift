import AppKit
import XCTest
@testable import ccmux

/// The container half of the tab-click focus handshake. `PaneFocusCoordinatorTests`
/// covers the request/expiry bookkeeping; this covers the guard ordering, which is
/// where the subtlety lives.
///
/// The claim must come LAST, after the window and embedding checks. Hoisting it
/// above them would let a container that cannot act consume the one-shot request
/// and focus nothing — the feature silently dead, and with no log, since the only
/// NSLog sits past the claim.
@MainActor
final class PaneFocusClaimTests: XCTestCase {
    private final class FocusableView: NSView {
        override var acceptsFirstResponder: Bool { true }
    }

    private func makeWindow() -> NSWindow {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 400, height: 300),
            styleMask: [.titled], backing: .buffered, defer: false)
        window.makeKeyAndOrderFront(nil)
        return window
    }

    /// A host that is on screen with its terminal embedded takes the focus.
    func testAnEmbeddedTerminalTakesFocus() {
        let window = makeWindow()
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 200, height: 200))
        window.contentView?.addSubview(host)
        let terminal = FocusableView(frame: host.bounds)
        host.addSubview(terminal)

        let tab = UUID()
        let claim = PaneFocusClaim(tabId: tab, host: host) { terminal }
        PaneFocusCoordinator.shared.requestFocus(tabId: tab)
        claim.claimIfRequested()

        XCTAssertTrue(window.firstResponder === terminal)
    }

    /// A host not yet in a window must leave the request intact for the container
    /// that can actually serve it.
    func testAHostWithNoWindowLeavesTheRequestUnclaimed() {
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 200, height: 200))
        let terminal = FocusableView(frame: host.bounds)
        host.addSubview(terminal)

        let tab = UUID()
        let claim = PaneFocusClaim(tabId: tab, host: host) { terminal }
        PaneFocusCoordinator.shared.requestFocus(tabId: tab)
        claim.claimIfRequested()

        XCTAssertTrue(PaneFocusCoordinator.shared.claim(tabId: tab),
                      "the request was consumed by a container that could not act on it")
    }

    /// The case the ordering exists for: a terminal view is reused across containers,
    /// so two of them can briefly share a tab id. Only the one currently holding the
    /// view may claim.
    func testAHostThatNoLongerHoldsTheTerminalLeavesTheRequestUnclaimed() {
        let window = makeWindow()
        let oldHost = NSView(frame: NSRect(x: 0, y: 0, width: 200, height: 200))
        let newHost = NSView(frame: NSRect(x: 0, y: 0, width: 200, height: 200))
        window.contentView?.addSubview(oldHost)
        window.contentView?.addSubview(newHost)
        let terminal = FocusableView(frame: oldHost.bounds)
        newHost.addSubview(terminal) // reparented already

        let tab = UUID()
        let stale = PaneFocusClaim(tabId: tab, host: oldHost) { terminal }
        PaneFocusCoordinator.shared.requestFocus(tabId: tab)
        stale.claimIfRequested()

        XCTAssertFalse(window.firstResponder === terminal)
        XCTAssertTrue(PaneFocusCoordinator.shared.claim(tabId: tab),
                      "the container that no longer holds the terminal consumed the request")
    }

    /// A request naming a different tab is not this container's to take.
    func testARequestForAnotherTabIsNotClaimed() {
        let window = makeWindow()
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 200, height: 200))
        window.contentView?.addSubview(host)
        let terminal = FocusableView(frame: host.bounds)
        host.addSubview(terminal)

        let mine = UUID()
        let other = UUID()
        let claim = PaneFocusClaim(tabId: mine, host: host) { terminal }
        PaneFocusCoordinator.shared.requestFocus(tabId: other)
        claim.claimIfRequested()

        XCTAssertFalse(window.firstResponder === terminal)
        XCTAssertTrue(PaneFocusCoordinator.shared.claim(tabId: other))
    }

    /// With no terminal to focus yet, the request must survive for the rebuild that
    /// will have one.
    func testAMissingTerminalLeavesTheRequestUnclaimed() {
        let window = makeWindow()
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 200, height: 200))
        window.contentView?.addSubview(host)

        let tab = UUID()
        let claim = PaneFocusClaim(tabId: tab, host: host) { nil }
        PaneFocusCoordinator.shared.requestFocus(tabId: tab)
        claim.claimIfRequested()

        XCTAssertTrue(PaneFocusCoordinator.shared.claim(tabId: tab))
    }
}

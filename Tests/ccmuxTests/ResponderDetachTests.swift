import AppKit
import XCTest
@testable import ccmux

/// Closing a pane drops the last reference to its terminal view. A window does not
/// retain its first responder, so a view released while holding that role leaves the
/// window pointing at freed memory — and the next action sent to the first responder
/// reads it. `sendAction(_:to:from:)` with a nil target (every Edit-menu item works
/// that way) starts its walk at `window.firstResponder`, which is why pressing Cmd+Z
/// after closing a pane is what surfaces it.
@MainActor
final class ResponderDetachTests: XCTestCase {
    /// Stands in for a terminal: a view that takes first responder.
    private final class FocusableView: NSView {
        override var acceptsFirstResponder: Bool { true }
    }

    private func makeWindow() -> NSWindow {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 400, height: 300),
            styleMask: [.titled], backing: .buffered, defer: false)
        // A window only maintains a first responder once it can key.
        window.makeKeyAndOrderFront(nil)
        return window
    }

    func testDetachingClearsTheWindowsFirstResponder() {
        let window = makeWindow()
        let view = FocusableView(frame: NSRect(x: 0, y: 0, width: 100, height: 100))
        window.contentView?.addSubview(view)

        XCTAssertTrue(window.makeFirstResponder(view))
        XCTAssertTrue(window.firstResponder === view, "precondition: the view holds first responder")

        view.detachFromResponderChain()

        XCTAssertFalse(window.firstResponder === view,
                       "the window still points at a view that is about to be released")
        XCTAssertNil(view.superview, "the view is still in the hierarchy")
    }

    /// Bounds the risk this helper covers, and is why the fix cannot stop here.
    ///
    /// A view outside the window's hierarchy cannot hold first responder: AppKit
    /// refuses to hand it the role, and removing a focused view from its superview
    /// takes the role away. So a terminal sitting in a background tab — no superview,
    /// alive only because a dictionary holds it — is NOT a stale-responder risk, and
    /// the dangerous window is only while a focused terminal is still on screen.
    func testAViewOutsideTheHierarchyCannotHoldFirstResponder() {
        let window = makeWindow()
        let view = FocusableView(frame: NSRect(x: 0, y: 0, width: 100, height: 100))
        window.contentView?.addSubview(view)
        XCTAssertTrue(window.makeFirstResponder(view))

        view.removeFromSuperview()
        XCTAssertFalse(window.firstResponder === view,
                       "removing a focused view should have released the role")

        // makeFirstResponder reports success here — it answers "did the exchange
        // happen", not "is this view now the responder" — so only the resulting
        // state is worth asserting.
        _ = window.makeFirstResponder(view)
        XCTAssertFalse(window.firstResponder === view,
                       "a view outside the window ended up holding first responder")
    }

    /// Detaching a view that never had focus must not steal the role from whoever does.
    func testDetachingABackgroundViewLeavesTheFocusedOneAlone() {
        let window = makeWindow()
        let focused = FocusableView(frame: NSRect(x: 0, y: 0, width: 100, height: 100))
        let other = FocusableView(frame: NSRect(x: 0, y: 100, width: 100, height: 100))
        window.contentView?.addSubview(focused)
        window.contentView?.addSubview(other)
        XCTAssertTrue(window.makeFirstResponder(focused))

        other.detachFromResponderChain()

        XCTAssertTrue(window.firstResponder === focused, "an unrelated close stole the keyboard")
        XCTAssertNil(other.superview)
    }

    /// A view with no window at all is the common case at app teardown; it must not trap.
    func testDetachingAWindowlessViewIsSafe() {
        let view = FocusableView(frame: NSRect(x: 0, y: 0, width: 100, height: 100))
        view.detachFromResponderChain()
        XCTAssertNil(view.superview)
    }
}

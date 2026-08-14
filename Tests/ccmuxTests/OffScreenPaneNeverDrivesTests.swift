import AppKit
import XCTest
@testable import ccmux

/// A reconnect re-drives every pane's size so the window being looked at owns the
/// pane again. That is only safe because an off-screen pane sends nothing: without
/// the guard, a background window coming back would crush the width a phone is
/// actively driving, and the only way back would be the "Take over" pill.
///
/// This pins the guard itself, which is the single assumption the reconnect policy
/// in `WorkspaceAttachment`'s `.hello` handler rests on.
///
/// The send is observed through `isStale` rather than a spy socket: driving the
/// pane clears staleness optimistically (`paneCols = cols`) from inside the same
/// branch that sends the resize, and it does so after the superview guard. So
/// "still stale" means "nothing was driven".
final class OffScreenPaneNeverDrivesTests: XCTestCase {
    func testAPaneWithNoSuperviewDrivesNothing() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        XCTAssertNil(c.terminalView.superview, "precondition: the pane is not on screen")

        // Another lens owns the pane at a width this view does not show.
        c.setAuthoritativeSize(cols: c.terminalView.getTerminal().cols + 37, rows: 24)
        XCTAssertTrue(c.isStale)

        c.sendCurrentSize()

        XCTAssertTrue(c.isStale, "an off-screen pane must not drive the shared size")
    }

    func testAnEmbeddedPaneDoesDriveItsSize() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 800, height: 480))
        host.addSubview(c.terminalView)

        c.setAuthoritativeSize(cols: c.terminalView.getTerminal().cols + 37, rows: 24)
        XCTAssertTrue(c.isStale)

        c.sendCurrentSize()

        XCTAssertFalse(c.isStale, "an on-screen pane should have re-driven the size to its own grid")
    }
}

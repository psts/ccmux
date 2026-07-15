import AppKit
import XCTest
@testable import ccmux

/// The "take over" trigger: a hosted pane is stale when the daemon's authoritative
/// width differs from this view's grid, and not stale once they match (e.g. after
/// this window drives its own size).
final class RemoteTermStalenessTests: XCTestCase {
    func testAuthoritativeSizeDrivesStaleness() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        let grid = c.terminalView.getTerminal().cols
        XCTAssertGreaterThan(grid, 0, "terminal should have a non-zero grid width")

        var events: [Bool] = []
        c.onStaleChanged = { events.append($0) }

        // Another lens drove a different width → stale.
        c.setAuthoritativeSize(cols: grid + 37, rows: 24)
        XCTAssertTrue(c.isStale)

        // The daemon confirms our own width → not stale.
        c.setAuthoritativeSize(cols: grid, rows: 24)
        XCTAssertFalse(c.isStale)

        // Only the two transitions should have fired (no spurious callbacks).
        XCTAssertEqual(events, [true, false])
    }

    func testUnknownSizeIsNeverStale() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        c.setAuthoritativeSize(cols: 0, rows: 0) // daemon omitted the size
        XCTAssertFalse(c.isStale)
    }
}

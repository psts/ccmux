import AppKit
import XCTest
@testable import ccmux

/// The "take over" trigger: a hosted pane is stale when the daemon's authoritative
/// size differs from this view's grid in EITHER dimension, and not stale once they
/// match (e.g. after this window drives its own size).
final class RemoteTermStalenessTests: XCTestCase {
    func testAuthoritativeSizeDrivesStaleness() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        let t = c.terminalView.getTerminal()
        XCTAssertGreaterThan(t.cols, 0, "terminal should have a non-zero grid width")
        XCTAssertGreaterThan(t.rows, 0, "terminal should have a non-zero grid height")

        var events: [Bool] = []
        c.onStaleChanged = { events.append($0) }

        // Another lens drove a different width → stale.
        c.setAuthoritativeSize(cols: t.cols + 37, rows: t.rows)
        XCTAssertTrue(c.isStale)

        // The daemon confirms our own size → not stale.
        c.setAuthoritativeSize(cols: t.cols, rows: t.rows)
        XCTAssertFalse(c.isStale)

        // Only the two transitions should have fired (no spurious callbacks).
        XCTAssertEqual(events, [true, false])
    }

    /// A rows-only divergence is the sneaky one: same width means no wrapped text
    /// to give it away, just every cursor address landing on the wrong line. It
    /// went undetected while staleness compared columns only.
    func testRowsOnlyDivergenceIsStale() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        let t = c.terminalView.getTerminal()

        c.setAuthoritativeSize(cols: t.cols, rows: t.rows + 9)
        XCTAssertTrue(c.isStale, "a height mismatch alone must surface the take-over control")

        c.setAuthoritativeSize(cols: t.cols, rows: t.rows)
        XCTAssertFalse(c.isStale)
    }

    func testUnknownSizeIsNeverStale() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        c.setAuthoritativeSize(cols: 0, rows: 0) // daemon omitted the size
        XCTAssertFalse(c.isStale)
    }

    /// An older daemon knows cols but not rows. The unknown dimension must read as
    /// "unknown", not "differs" — otherwise every pane against an old daemon would
    /// show a permanent take-over pill.
    func testUnknownRowsAloneIsNeverStale() {
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: nil)
        let t = c.terminalView.getTerminal()
        c.setAuthoritativeSize(cols: t.cols, rows: 0)
        XCTAssertFalse(c.isStale)
    }
}

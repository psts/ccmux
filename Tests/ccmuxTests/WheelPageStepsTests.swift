import XCTest
import CoreGraphics
import SwiftTerm

/// Covers the pure trackpad-wheel throttle math that drives PgUp/PgDn forwarding
/// on the alternate screen buffer (Claude Code fullscreen TUI). See
/// `TerminalView.wheelPageSteps` in the SwiftTerm fork.
final class WheelPageStepsTests: XCTestCase {
    private let threshold: CGFloat = 10

    private func steps(acc: CGFloat, delta: CGFloat) -> (pages: Int, accumulator: CGFloat) {
        TerminalView.wheelPageSteps(accumulator: acc, delta: delta, threshold: threshold)
    }

    func testZeroDeltaIsNoOp() {
        let r = steps(acc: 7, delta: 0)
        XCTAssertEqual(r.pages, 0)
        XCTAssertEqual(r.accumulator, 7)
    }

    func testAccumulatesBelowThreshold() {
        let r = steps(acc: 3, delta: 4)
        XCTAssertEqual(r.pages, 0)
        XCTAssertEqual(r.accumulator, 7)
    }

    func testSinglePageUpWithRemainder() {
        let r = steps(acc: 8, delta: 5)   // 13 -> one page, 3 left
        XCTAssertEqual(r.pages, 1)
        XCTAssertEqual(r.accumulator, 3)
    }

    func testExactThresholdEmitsOnePageAndZeroesOut() {
        let r = steps(acc: 0, delta: 10)
        XCTAssertEqual(r.pages, 1)
        XCTAssertEqual(r.accumulator, 0)
    }

    func testMultiplePagesFromLargeDelta() {
        let r = steps(acc: 0, delta: 25)  // 2 pages, 5 left
        XCTAssertEqual(r.pages, 2)
        XCTAssertEqual(r.accumulator, 5)
    }

    func testNegativeDeltaPagesDown() {
        let r = steps(acc: 0, delta: -25)
        XCTAssertEqual(r.pages, -2)
        XCTAssertEqual(r.accumulator, -5)
    }

    func testReversalDiscardsOppositeResidue() {
        // Was scrolling up (acc 8); a downward flick resets before applying.
        let r = steps(acc: 8, delta: -3)
        XCTAssertEqual(r.pages, 0)
        XCTAssertEqual(r.accumulator, -3)
    }

    func testContinuedSameDirectionDoesNotReset() {
        let r = steps(acc: -5, delta: -8)  // -13 -> one page down, -3 left
        XCTAssertEqual(r.pages, -1)
        XCTAssertEqual(r.accumulator, -3)
    }
}

import XCTest
@testable import ccmux

/// Pins the sidebar blink's clock: every copy of a row's background reads it,
/// so the name and the dashboard under it can only agree if it is a pure
/// function of the time.
final class AttentionGlowTests: XCTestCase {
    private func glow(_ seconds: TimeInterval) -> Double {
        attentionGlow(at: Date(timeIntervalSinceReferenceDate: seconds))
    }

    func testDimAtTheStartAndBrightHalfwayThrough() {
        XCTAssertEqual(glow(0), 0, accuracy: 1e-9)
        XCTAssertEqual(glow(0.7), 1, accuracy: 1e-9)
        XCTAssertEqual(glow(1.4), 0, accuracy: 1e-9)
    }

    func testMinutesApartStillInStep() {
        let now = 812_345_678.123
        for cycles in [1.0, 43.0, 1000.0] {
            XCTAssertEqual(glow(now), glow(now + 1.4 * cycles), accuracy: 1e-6)
        }
    }

    func testStaysBetweenDimAndBright() {
        for step in 0..<100 {
            let g = glow(812_345_678 + Double(step) * 0.037)
            XCTAssertTrue((0...1).contains(g), "glow \(g) at step \(step)")
        }
    }
}

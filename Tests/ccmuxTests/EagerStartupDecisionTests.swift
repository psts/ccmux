import XCTest
import CoreGraphics
@testable import ccmux

/// Pins the policy for replaying startup commands on relaunch (the lazy-restart fix).
/// Before this, only the displayed workspace's `claude` restarted on launch; non-displayed
/// workspaces waited until activated. `eagerStartupDecision` is the pure kernel of that fix.
final class EagerStartupDecisionTests: XCTestCase {
    private typealias Decision = WorkspaceManager.EagerStartupDecision

    func testDisplayedWorkspaceSkips() {
        // The displayed pane fires via the view layer at its exact laid-out size — never here.
        let d = WorkspaceManager.eagerStartupDecision(
            isDisplayed: true, isSingleLeaf: true,
            startupCommand: "claude", contentSize: CGSize(width: 1000, height: 700))
        XCTAssertEqual(d, .skip)
    }

    func testNoCommandSkips() {
        XCTAssertEqual(
            WorkspaceManager.eagerStartupDecision(
                isDisplayed: false, isSingleLeaf: true, startupCommand: nil, contentSize: nil),
            .skip)
    }

    func testEmptyCommandSkips() {
        XCTAssertEqual(
            WorkspaceManager.eagerStartupDecision(
                isDisplayed: false, isSingleLeaf: true, startupCommand: "", contentSize: nil),
            .skip)
    }

    func testSingleLeafFiresAtWindowContentSize() {
        let size = CGSize(width: 1000, height: 700)
        XCTAssertEqual(
            WorkspaceManager.eagerStartupDecision(
                isDisplayed: false, isSingleLeaf: true, startupCommand: "claude", contentSize: size),
            .fire(targetSize: size))
    }

    func testMultiPaneFiresAtFallbackSize() {
        // We don't reconstruct split geometry off-screen, so multi-pane ignores the content
        // size and uses the conservative fallback (nil → narrower, the safe wrap direction).
        XCTAssertEqual(
            WorkspaceManager.eagerStartupDecision(
                isDisplayed: false, isSingleLeaf: false,
                startupCommand: "claude", contentSize: CGSize(width: 1000, height: 700)),
            .fire(targetSize: nil))
    }

    func testSingleLeafWithUnknownContentSizeFiresAtFallback() {
        // Window not laid out yet (e.g. on another Space): no measured size → fallback fire.
        XCTAssertEqual(
            WorkspaceManager.eagerStartupDecision(
                isDisplayed: false, isSingleLeaf: true, startupCommand: "claude", contentSize: nil),
            .fire(targetSize: nil))
    }
}

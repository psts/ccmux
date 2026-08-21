import XCTest
@testable import ccmux

/// Pins hosted-workspace window ownership resolution: every live hosted
/// workspace ends up owned by EXACTLY one window. This is the guard against
/// the create/adopt race (a new session claimed by the creating window while
/// the reconcile-driven orphan sweep adopts it into the first window) and it
/// heals double-ownership that older builds persisted.
final class WindowOwnershipTests: XCTestCase {
    private let ws1 = UUID()
    private let ws2 = UUID()

    func testUnplacedOrphanIsNotAdopted() {
        // No group row means "not in our windows" — someone else's session, or
        // ours put away. Adopting it anyway is the multi-lens interference the
        // per-user views replaced; it renders under AVAILABLE instead.
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [[], [ws2]], displayed: [nil, nil],
            windowNames: ["A", "B"]),
            "an unplaced session must stay unowned (AVAILABLE), not land in window 0")
    }

    func testOrphanAdoptsIntoTheWindowMatchingItsGroup() throws {
        // A session created from web/phone with group "MIXED" must land in the
        // window of that name, not the first one.
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "MIXED"], owned: [[], []], displayed: [nil, nil],
            windowNames: ["CHARTLABS", "MIXED"]))
        XCTAssertEqual(resolved, [[], [ws1]])
    }

    func testGroupMatchIgnoresCase() throws {
        // Rows written from web/phone may not match a window name's
        // capitalisation — same rule as the peers bus.
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "chartlabs"], owned: [[], []], displayed: [nil, nil],
            windowNames: ["CHARTLABS", "B"]))
        XCTAssertEqual(resolved, [[ws1], []])
    }

    func testOrphanWithUnmatchedGroupStaysAvailable() {
        // Our row names a window that is not open: force-homing it into window 0
        // would let the group sync push window 0's name over the row we chose
        // from another lens. It waits in AVAILABLE instead.
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "NOPE"], owned: [[], []], displayed: [nil, nil],
            windowNames: ["A", "B"]),
            "a row naming an unopened window must not be force-homed")
    }

    func testDuplicateKeepsTheDisplayingWindow() throws {
        // ws1 owned by both windows; the SECOND window displays it — it wins.
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [[ws1], [ws1]], displayed: [nil, ws1],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[], [ws1]])
    }

    func testDuplicateWithoutDisplayKeepsFirstOwner() throws {
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [[ws1], [ws1]], displayed: [nil, nil],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[ws1], []])
    }

    func testNoChangeReturnsNil() {
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1, ws2], groups: [:], owned: [[ws1], [ws2]], displayed: [ws1, ws2],
            windowNames: ["A", "B"]),
            "clean single ownership → nil, so callers skip churn")
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [], displayed: [], windowNames: []),
            "no windows → nothing to do")
    }
}

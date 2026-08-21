import XCTest
@testable import ccmux

/// Pins hosted-workspace window ownership resolution under the SHARED windows
/// model (v2): the daemon's membership is authoritative — a workspace belongs
/// to the on-screen window whose name is its group, moves there when another
/// window held it, and is released everywhere when its window is not open
/// here (closed shared window, or ungrouped → AVAILABLE).
final class WindowOwnershipTests: XCTestCase {
    private let ws1 = UUID()
    private let ws2 = UUID()

    func testUngroupedIsNotAdopted() {
        // No membership means AVAILABLE — force-homing it into window 0 was
        // the original multi-lens interference.
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [[], [ws2]],
            windowNames: ["A", "B"]),
            "an ungrouped session must stay unowned (AVAILABLE), not land in window 0")
    }

    func testAdoptsIntoTheWindowMatchingItsGroup() throws {
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "MIXED"], owned: [[], []],
            windowNames: ["CHARTLABS", "MIXED"]))
        XCTAssertEqual(resolved, [[], [ws1]])
    }

    func testGroupMatchIgnoresCase() throws {
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "chartlabs"], owned: [[], []],
            windowNames: ["CHARTLABS", "B"]))
        XCTAssertEqual(resolved, [[ws1], []])
    }

    func testSharedMoveRelocatesBetweenOpenWindows() throws {
        // Someone moved ws1 to window B on another lens: this Mac follows.
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "B"], owned: [[ws1], []],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[], [ws1]])
    }

    func testMembershipInAClosedWindowReleasesOwnership() throws {
        // ws1 now belongs to a shared window that is not open on this Mac: it
        // must leave the local window (it is reachable by opening its window),
        // not squat where it no longer lives.
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "ELSEWHERE"], owned: [[ws1], []],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[], []])
    }

    func testDuplicateCollapsesToTheMembershipWindow() throws {
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "B"], owned: [[ws1], [ws1]],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[], [ws1]])
    }

    func testNoChangeReturnsNil() {
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1, ws2], groups: [ws1: "A", ws2: "B"], owned: [[ws1], [ws2]],
            windowNames: ["A", "B"]),
            "clean matching ownership → nil, so callers skip churn")
        XCTAssertNil(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [], windowNames: []),
            "no windows → nothing to do")
    }
}

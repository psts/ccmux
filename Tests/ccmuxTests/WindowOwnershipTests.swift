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

    func testOrphanAdoptsIntoFirstWindow() throws {
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [:], owned: [[], [ws2]], displayed: [nil, nil],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[ws1], [ws2]])
    }

    func testOrphanAdoptsIntoTheWindowMatchingItsGroup() throws {
        // A session created from web/phone with group "MIXED" must land in the
        // window of that name, not the first one.
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "MIXED"], owned: [[], []], displayed: [nil, nil],
            windowNames: ["CHARTLABS", "MIXED"]))
        XCTAssertEqual(resolved, [[], [ws1]])
    }

    func testOrphanWithUnknownGroupFallsBackToFirstWindow() throws {
        let resolved = try XCTUnwrap(WindowManager.reconcileHostedOwnership(
            workspaceIds: [ws1], groups: [ws1: "NOPE"], owned: [[], []], displayed: [nil, nil],
            windowNames: ["A", "B"]))
        XCTAssertEqual(resolved, [[ws1], []])
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

import XCTest
@testable import ccmux

/// Closing and restoring a window. The behavior under test: a window holding hosted
/// sessions is remembered, and restoring it gathers those sessions back — previously
/// such a window vanished with no "Restore Window" entry and its sessions fell into
/// whichever window happened to be first.
final class ClosedWindowTests: XCTestCase {

    private let local = UUID()
    private let hosted = UUID()
    private let other = UUID()

    // MARK: - closingPlan

    func testHostedOnlyWindowArchivesItsSessionsAndIsRecorded() {
        // Closing a window closes what is in it: the session is archived (revivable),
        // never left running with nowhere to appear.
        let plan = WindowManager.closingPlan(
            owned: [hosted], displayed: hosted,
            isHosted: { $0 == self.hosted }, isOwnedElsewhere: { _ in false })

        XCTAssertEqual(plan.archiveHosted, [hosted])
        XCTAssertEqual(plan.record, [hosted], "the window must be restorable")
        XCTAssertTrue(plan.closeLocally.isEmpty, "a hosted session has no local panes to tear down")
    }

    func testLocalWorkspacesAreBothClosedAndRecorded() {
        let plan = WindowManager.closingPlan(
            owned: [local], displayed: nil,
            isHosted: { _ in false }, isOwnedElsewhere: { _ in false })

        XCTAssertEqual(plan.closeLocally, [local])
        XCTAssertTrue(plan.archiveHosted.isEmpty)
        XCTAssertEqual(plan.record, [local])
    }

    func testMixedWindowClosesLocallyAndArchivesHosted() {
        let plan = WindowManager.closingPlan(
            owned: [local, hosted], displayed: nil,
            isHosted: { $0 == self.hosted }, isOwnedElsewhere: { _ in false })

        XCTAssertEqual(plan.closeLocally, [local])
        XCTAssertEqual(plan.archiveHosted, [hosted])
        XCTAssertEqual(Set(plan.record), [local, hosted])
    }

    func testWorkspaceOwnedByAnotherWindowIsLeftAlone() {
        let plan = WindowManager.closingPlan(
            owned: [local, other], displayed: nil,
            isHosted: { _ in false }, isOwnedElsewhere: { $0 == self.other })

        XCTAssertEqual(plan.closeLocally, [local], "another window still needs it")
        XCTAssertEqual(plan.record, [local], "and it is not this window's to restore")
    }

    func testDisplayedWorkspaceIsIncludedEvenIfOwnershipLostIt() {
        // Detach/move paths have historically dropped a workspace from ownedWorkspaceIds
        // while it was still on screen; missing it here leaks its PTYs.
        let plan = WindowManager.closingPlan(
            owned: [], displayed: local,
            isHosted: { _ in false }, isOwnedElsewhere: { _ in false })

        XCTAssertEqual(plan.closeLocally, [local])
    }

    func testEmptyWindowRecordsNothing() {
        let plan = WindowManager.closingPlan(
            owned: [], displayed: nil,
            isHosted: { _ in false }, isOwnedElsewhere: { _ in false })

        XCTAssertTrue(plan.record.isEmpty, "an empty window must not clutter Restore Window")
    }

    // MARK: - restorePlan

    func testRestoreSplitsHostedFromLocal() {
        let plan = WindowManager.restorePlan(
            ids: [hosted, local], isHosted: { $0 == self.hosted }, isReopenable: { $0 == self.local })

        XCTAssertEqual(plan.hosted, [hosted])
        XCTAssertEqual(plan.local, [local])
    }

    func testRestoreDropsSessionsThatNoLongerExist() {
        // Deleted from the daemon while the window was closed (going cold is no longer a
        // reason to drop — that is the normal state now). Claiming one anyway writes a
        // phantom id into the window descriptor that nothing prunes.
        let plan = WindowManager.restorePlan(
            ids: [hosted, local], isHosted: { _ in false }, isReopenable: { _ in false })

        XCTAssertTrue(plan.hosted.isEmpty)
        XCTAssertTrue(plan.local.isEmpty)
    }

    // MARK: - detachDecision

    func testDetachingTheWorkspaceYourOwnWindowShowsActuallyDetaches() {
        // The reported dead menu item: the owner is the window asking, so fronting it
        // changes nothing the user can see.
        XCTAssertEqual(
            WindowManager.detachDecision(askedByOwner: true, ownerDisplaysIt: true, ownerHasOthers: true),
            .detach(repointOwner: true))
    }

    func testDetachingAWindowsOnlyWorkspaceRevealsInstead() {
        // A new window would move it sideways, discard the old window's name, and leave
        // an empty descriptor that comes back on next launch.
        XCTAssertEqual(
            WindowManager.detachDecision(askedByOwner: true, ownerDisplaysIt: true, ownerHasOthers: false),
            .revealOwner)
    }

    func testDetachingFromAnotherWindowRevealsTheOwnerShowingIt() {
        XCTAssertEqual(
            WindowManager.detachDecision(askedByOwner: false, ownerDisplaysIt: true, ownerHasOthers: true),
            .revealOwner)
    }

    func testDetachingOneTheOwnerIsNotShowingLeavesItsDisplayAlone() {
        XCTAssertEqual(
            WindowManager.detachDecision(askedByOwner: false, ownerDisplaysIt: false, ownerHasOthers: true),
            .detach(repointOwner: false))
        XCTAssertEqual(
            WindowManager.detachDecision(askedByOwner: true, ownerDisplaysIt: false, ownerHasOthers: true),
            .detach(repointOwner: false))
    }

    // MARK: - Restoring what closing archived

    private func coldWorkspace(id: String, group: String = "Dasha") throws -> DaemonWorkspace {
        let json = """
        {"id":"\(id)","name":"kullio","repoPath":"/r","status":"cold","panes":[],
         "group":"\(group)","hostnames":[],"devCommand":"","host":""}
        """
        return try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))
    }

    func testAColdSessionIsFoundByTheAppIdItsRecordHolds() throws {
        // Closing archives, so by restore time the session is cold and has no
        // attachment — the live daemonIds map has forgotten it. The record only holds
        // the app UUID, and this deterministic mapping is the only route back to the
        // daemon id that revives it.
        let daemonId = "44442b4a-ce10-4aae-bb60-4f6e6935f654"
        let appId = RemoteWorkspaceBuilder.workspaceUUID(daemonId)
        let cold = [try coldWorkspace(id: daemonId)]

        XCTAssertEqual(RemoteWorkspaceBuilder.coldDaemonId(forApp: appId, in: cold), daemonId)
    }

    func testASessionTheDaemonNoLongerHasIsNotFound() throws {
        let cold = [try coldWorkspace(id: "44442b4a-ce10-4aae-bb60-4f6e6935f654")]

        XCTAssertNil(RemoteWorkspaceBuilder.coldDaemonId(forApp: other, in: cold))
        XCTAssertNil(RemoteWorkspaceBuilder.coldDaemonId(forApp: hosted, in: []))
    }

    func testRestoreCountsColdSessionsSoAnArchivedWindowComesBack() {
        // The predicate restore passes must accept cold sessions; keying off "is
        // attached right now" would find nothing and throw the window away.
        let plan = WindowManager.restorePlan(
            ids: [hosted, other], isHosted: { $0 == self.hosted }, isReopenable: { _ in false })

        XCTAssertEqual(plan.hosted, [hosted])
        XCTAssertTrue(plan.local.isEmpty)
    }

    // MARK: - Stale closed-window references

    /// A manager that cannot touch the user's real state.json — the save path has no
    /// test override — and whose git/process monitors are stopped when the test ends.
    private func makeManager() -> WorkspaceManager {
        let manager = WorkspaceManager(autosaves: false)
        addTeardownBlock {
            for workspace in manager.workspaces { manager.removeWorkspace(id: workspace.id) }
        }
        return manager
    }

    func testReopeningAWorkspaceClearsItsClosedWindowReference() {
        // The sidebar hides a closed workspace that any closed window still mentions.
        // A reference outliving the membership hid it from both restore menus for good.
        let manager = makeManager()
        let workspace = Workspace.create(name: "proj", repoPath: "/tmp/proj")
        manager.closedWorkspaces = [workspace]
        manager.closedWindows = [ClosedWindow(
            id: UUID(), windowName: "Dasha", workspaceIds: [workspace.id, other],
            displayedWorkspaceId: nil, frame: WindowFrame(x: 0, y: 0, width: 10, height: 10))]

        XCTAssertNotNil(manager.reopenWorkspace(id: workspace.id))

        XCTAssertEqual(manager.closedWindows.first?.workspaceIds, [other],
                       "the reopened workspace must no longer be claimed by a closed window")
    }

    func testDeletingAHostedSessionPrunesItFromClosedWindows() {
        // Another lens deletes the session while its window is closed. Nothing else
        // prunes hosted ids from a record, so the entry would linger in "Restore
        // Window" pointing at nothing.
        let manager = makeManager()
        let windowManager = WindowManager(workspaceManager: manager)
        manager.closedWindows = [ClosedWindow(
            id: UUID(), windowName: "Dasha", workspaceIds: [hosted],
            displayedWorkspaceId: hosted, frame: WindowFrame(x: 0, y: 0, width: 10, height: 10))]

        windowManager.hostedWorkspaceRemoved(id: hosted)

        XCTAssertTrue(manager.closedWindows.isEmpty, "a record with no members left is a dead menu entry")
    }

    func testClosedWindowIsPrunedWhenItsLastWorkspaceReopens() {
        let manager = makeManager()
        let workspace = Workspace.create(name: "proj", repoPath: "/tmp/proj")
        manager.closedWorkspaces = [workspace]
        manager.closedWindows = [ClosedWindow(
            id: UUID(), windowName: "Dasha", workspaceIds: [workspace.id],
            displayedWorkspaceId: nil, frame: WindowFrame(x: 0, y: 0, width: 10, height: 10))]

        XCTAssertNotNil(manager.reopenWorkspace(id: workspace.id))

        XCTAssertTrue(manager.closedWindows.isEmpty, "an emptied record is a dead menu entry")
    }
}

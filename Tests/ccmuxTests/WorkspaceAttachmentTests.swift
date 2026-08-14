import XCTest
@testable import ccmux

/// Pins the attachment's pane-controller reconcile: a daemon-side pane change must
/// warm a controller for each new pane and drop dead ones — without touching the
/// WS connection (that's the whole point of patching instead of rebuilding).
final class WorkspaceAttachmentTests: XCTestCase {

    private func pane(_ id: String) -> DaemonPane {
        try! JSONDecoder().decode(DaemonPane.self, from: Data(#"{"id":"\#(id)","cwd":"/r","title":"t"}"#.utf8))
    }

    func testSyncPanesWarmsNewAndDropsDead() {
        let att = WorkspaceAttachment(
            workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1"), pane("p2")])
        XCTAssertTrue(att.hasPane("p1"))
        XCTAssertTrue(att.hasPane("p2"))

        let dead = att.syncPanes([pane("p1"), pane("p3")])   // p2 died, p3 spawned
        XCTAssertEqual(dead, ["p2"])
        XCTAssertTrue(att.hasPane("p3"), "new pane pre-warmed so its first frames land")
        XCTAssertFalse(att.hasPane("p2"), "dead pane's controller dropped")
        XCTAssertTrue(att.hasPane("p1"), "surviving pane untouched")
    }

    func testSyncPanesNoChangeIsNoOp() {
        let att = WorkspaceAttachment(workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1")])
        XCTAssertEqual(att.syncPanes([pane("p1")]), [])
        XCTAssertTrue(att.hasPane("p1"))
    }

    func testReportFocusDedupesAndTracksState() {
        let att = WorkspaceAttachment(workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1")])
        XCTAssertNil(att.lastReportedFocus, "nothing reported until asked")
        XCTAssertNil(att.lastReportedPresent)
        XCTAssertEqual(att.anyPaneId, "p1")

        att.reportFocus(paneId: "p1", present: true)
        XCTAssertEqual(att.lastReportedFocus, "p1")
        XCTAssertEqual(att.lastReportedPresent, true)
        att.reportFocus(paneId: "p1", present: true)   // dedupe — no state churn
        XCTAssertEqual(att.lastReportedFocus, "p1")
        att.reportFocus(paneId: "", present: true)     // still here, just looking elsewhere
        XCTAssertEqual(att.lastReportedFocus, "")
        XCTAssertEqual(att.lastReportedPresent, true)
    }

    /// Presence changing on its own must still be reported. A workspace that was
    /// never displayed already sits at focus "", so when the screen locks only the
    /// presence half moves; deduping on the pane id alone would swallow the one
    /// signal that tells the daemon to start pushing to the phone.
    func testReportFocusSendsWhenOnlyPresenceChanges() {
        let att = WorkspaceAttachment(workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1")])
        att.reportFocus(paneId: "p1", present: true)
        att.reportFocus(paneId: "p1", present: false)
        XCTAssertEqual(att.lastReportedPresent, false, "a presence flip must not be deduped away")
        XCTAssertEqual(att.lastReportedFocus, "p1")
    }
}

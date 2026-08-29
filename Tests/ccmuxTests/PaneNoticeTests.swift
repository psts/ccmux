import XCTest
@testable import ccmux

/// Per-pane transient notices — the Mac half of the truncated-paste warning.
///
/// The Go side got four tests for the same rules (`noticeQueue` in
/// daemon/internal/api/notices_test.go); this side had none beyond frame
/// decoding, and CLAUDE.md requires the two lenses implement the identical
/// rule rather than an approximation. A regression here means the user misses
/// the only signal that half their paste is sitting in the pane.
///
/// These drive postPaneNotice/expirePaneNotice directly rather than waiting on
/// the 10s timer: the rule worth pinning is which notice wins and which expiry
/// is allowed to clear it, and neither needs wall-clock time to check.
@MainActor
final class PaneNoticeTests: XCTestCase {
    private let service = RemoteSessionService.shared

    override func tearDown() async throws {
        for pane in ["%1", "%2"] {
            service.expirePaneNotice(paneId: pane, ifStill: service.hostedNotice(paneId: pane) ?? "")
        }
        try await super.tearDown()
    }

    func testNewestNoticeWinsForAPane() {
        service.postPaneNotice(paneId: "%1", text: "first")
        service.postPaneNotice(paneId: "%1", text: "second")
        XCTAssertEqual(service.hostedNotice(paneId: "%1"), "second")
    }

    func testPanesDoNotCrowdEachOtherOut() {
        service.postPaneNotice(paneId: "%1", text: "for pane one")
        service.postPaneNotice(paneId: "%2", text: "for pane two")
        XCTAssertEqual(service.hostedNotice(paneId: "%1"), "for pane one")
        XCTAssertEqual(service.hostedNotice(paneId: "%2"), "for pane two")
    }

    /// The `ifStill:` token is what stops an older notice's expiry from cutting
    /// short a newer one. Without it, posting a second notice inside the first
    /// one's window means the first timer clears the second early, and the user
    /// loses the message that is actually current.
    func testStaleExpiryDoesNotClearANewerNotice() {
        service.postPaneNotice(paneId: "%1", text: "old")
        service.postPaneNotice(paneId: "%1", text: "new")

        service.expirePaneNotice(paneId: "%1", ifStill: "old") // the first timer firing late
        XCTAssertEqual(service.hostedNotice(paneId: "%1"), "new",
                       "an older notice's expiry must not clear the newer one")

        service.expirePaneNotice(paneId: "%1", ifStill: "new") // its own timer
        XCTAssertNil(service.hostedNotice(paneId: "%1"))
    }

    func testPaneWithNoNoticeReportsNil() {
        XCTAssertNil(service.hostedNotice(paneId: "%never-posted"))
    }
}

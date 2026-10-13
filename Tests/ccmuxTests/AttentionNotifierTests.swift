import XCTest
@testable import ccmux

/// The alert rule has two call sites in the app — the local hook listener and the
/// hosted firehose — and they drifted apart once: `done` stopped alerting on the
/// local path while the hosted path kept firing, so a session whose background
/// agents reported back one by one produced an alert per report. These pin the
/// rule itself rather than either call site.
final class AttentionNotifierTests: XCTestCase {
    /// `done` comes from the Stop hook alone, and Stop fires when Claude finishes
    /// responding, not when the work is finished. It flashes the sidebar; it must
    /// not alert.
    func testDoneDoesNotAlert() {
        XCTAssertFalse(AttentionNotifier.alerts(.done),
                       "done must not alert — Stop is not a finish, and each background agent reporting back produces one")
    }

    /// The signal that really means "finished, nothing more coming" is Claude
    /// Code's idle_prompt. It leaves a done row green and re-sends it as a
    /// reminder, which has to alert or a finished task never notifies.
    func testRemindedDoneAlerts() {
        XCTAssertTrue(AttentionNotifier.alerts(.done, remind: true),
                      "the idle reminder carries the completion alert on the done still showing")
    }

    /// A row seen since, or a fresh /clear, shows nothing: the reminder has
    /// nothing to alert for.
    func testRemindedNoneDoesNotAlert() {
        XCTAssertFalse(AttentionNotifier.alerts(.none, remind: true))
    }

    func testNeedsInputAlerts() {
        XCTAssertTrue(AttentionNotifier.alerts(.needsInput),
                      "needsInput is a permission prompt or a question")
    }

    func testNoneDoesNotAlert() {
        XCTAssertFalse(AttentionNotifier.alerts(.none))
    }

    /// `post` is the enforcement point: it guards on `alerts` before building any
    /// notification, so a call site that forgets the rule still cannot alert.
    func testEveryNonAlertingStateIsRejectedByTheRule() {
        for state in [AttentionState.done, .none] {
            XCTAssertFalse(AttentionNotifier.alerts(state), "\(state) must not alert")
        }
    }
}

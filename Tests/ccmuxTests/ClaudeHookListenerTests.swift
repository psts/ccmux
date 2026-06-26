import XCTest
@testable import ccmux

final class ClaudeHookListenerTests: XCTestCase {

    private func outcome(_ type: String, _ notif: String? = nil) -> ClaudeHookListener.EventOutcome {
        ClaudeHookListener.outcome(forEvent: type, notificationType: notif)
    }

    // MARK: - needs input

    func testPermissionRequestNeedsInput() {
        XCTAssertEqual(outcome("permission_request"), .set(.needsInput))
    }

    func testAskUserQuestionNeedsInput() {
        XCTAssertEqual(outcome("ask_user_question"), .set(.needsInput))
    }

    func testBlockingNotificationsNeedInput() {
        XCTAssertEqual(outcome("notification", "idle_prompt"), .set(.needsInput))
        XCTAssertEqual(outcome("notification", "permission_prompt"), .set(.needsInput))
        XCTAssertEqual(outcome("notification", "elicitation_dialog"), .set(.needsInput))
    }

    func testNonBlockingNotificationsIgnored() {
        // auth_success and unknown subtypes are informational, not "needs you".
        XCTAssertEqual(outcome("notification", "auth_success"), .ignore)
        XCTAssertEqual(outcome("notification", nil), .ignore)
        XCTAssertEqual(outcome("notification", "something_new"), .ignore)
    }

    // MARK: - done

    func testStopIsDone() {
        XCTAssertEqual(outcome("stop"), .set(.done))
    }

    // MARK: - clear

    func testUserPromptSubmitClears() {
        XCTAssertEqual(outcome("user_prompt_submit"), .clear)
    }

    func testSessionEndClears() {
        XCTAssertEqual(outcome("session_end"), .clear)
    }

    // MARK: - unknown

    func testUnknownEventIgnored() {
        XCTAssertEqual(outcome("pre_tool_use"), .ignore)
        XCTAssertEqual(outcome(""), .ignore)
    }
}

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

    // MARK: - which claims get held while agents run

    private func endsTurn(_ type: String, _ notif: String? = nil) -> Bool {
        ClaudeHookListener.endsTurn(forEvent: type, notificationType: notif)
    }

    /// Both assert the turn is over. Stop fires when the main loop stops talking,
    /// which is exactly what it does while waiting for background agents.
    func testTurnEndingClaimsAreHeldable() {
        XCTAssertTrue(endsTurn("stop"))
        XCTAssertTrue(endsTurn("notification", "idle_prompt"))
    }

    /// Being blocked on the human is not being finished. Holding one of these
    /// would strand the very agents being waited on.
    func testBlockingPromptsAreNeverHeld() {
        XCTAssertFalse(endsTurn("notification", "permission_prompt"))
        XCTAssertFalse(endsTurn("notification", "elicitation_dialog"))
        XCTAssertFalse(endsTurn("permission_request"))
        XCTAssertFalse(endsTurn("ask_user_question"))
        XCTAssertFalse(endsTurn("user_prompt_submit"))
        XCTAssertFalse(endsTurn("session_end"))
    }
}

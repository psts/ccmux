import XCTest
@testable import ccmux

/// claude's TUI positions words with cursor-move escapes, so the cells *between* words
/// are NUL (an empty SwiftTerm cell's `getCharacter()` returns "\0", not " "). These tests
/// pin the detection used to auto-confirm claude's startup prompts against that reality —
/// the original spaced match (`"I am using this for local development"`) silently never
/// fired because the rendered grid contains NULs, not spaces.
final class StartupPromptDetectionTests: XCTestCase {

    func testDetectsDevChannelsPromptWithNulSeparators() {
        let grid = "1.\0I\0am\0using\0this\0for\0local\0development\n2.\0Exit\nEnter\0to\0confirm\0·\0Esc\0to\0cancel\n"
        XCTAssertTrue(TerminalStore.isStartupConfirmPrompt(grid))
    }

    func testDetectsTrustFolderPromptWithNulSeparators() {
        let grid = "1.\0Yes,\0I\0trust\0this\0folder\n2.\0No,\0exit\n"
        XCTAssertTrue(TerminalStore.isStartupConfirmPrompt(grid))
    }

    func testRegressionSpacedMatchNeverFiredButCompactDoes() {
        let grid = "1.\0I\0am\0using\0this\0for\0local\0development\n"
        // Why the old code failed: the spaced phrase isn't in the NUL-separated grid.
        XCTAssertFalse(grid.contains("I am using this for local development"))
        // Why the new code works.
        XCTAssertTrue(TerminalStore.isStartupConfirmPrompt(grid))
    }

    func testIgnoresUnrelatedToolPermissionPrompt() {
        // A later tool prompt also ends with "Enter to confirm" — must NOT be auto-confirmed.
        let grid = "Do\0you\0want\0to\0proceed?\n1.\0Yes\n2.\0No\nEnter\0to\0confirm\0·\0Esc\0to\0cancel\n"
        XCTAssertFalse(TerminalStore.isStartupConfirmPrompt(grid))
    }

    func testIgnoresNormalOutput() {
        XCTAssertFalse(TerminalStore.isStartupConfirmPrompt("just some normal terminal output\n$ "))
    }
}

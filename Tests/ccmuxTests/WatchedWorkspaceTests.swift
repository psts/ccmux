import XCTest
@testable import ccmux

/// The rule that decides whether an attention signal is worth announcing. It ran
/// for a while without the Space term, and the gap was invisible in normal use:
/// switch to a Space that has another app's window and macOS activates that app,
/// so `appActive` goes false and the alert fires. Switch to an empty Space, or one
/// holding only ccmux's own windows, and nothing takes activation away — ccmux
/// stays frontmost with a key window you cannot see, and every alert for it was
/// dropped as "already watching".
final class WatchedWorkspaceTests: XCTestCase {
    private let target = UUID()

    /// The one case that must suppress: you are looking straight at it.
    func testWatchedWhenFrontmostOnThisSpaceShowingIt() {
        XCTAssertTrue(WindowManager.watched(
            appActive: true,
            keyWindowOnActiveSpace: true,
            displayedByKeyWindow: target,
            target: target
        ))
    }

    /// The bug. Frontmost and displaying it, but the window is parked on a Space
    /// you walked away from, so nothing about it is on your screen.
    func testNotWatchedWhenTheKeyWindowIsOnAnotherSpace() {
        XCTAssertFalse(WindowManager.watched(
            appActive: true,
            keyWindowOnActiveSpace: false,
            displayedByKeyWindow: target,
            target: target
        ), "a window on a Space you left is not being watched, however key it is")
    }

    func testNotWatchedWhenAnotherAppIsFrontmost() {
        XCTAssertFalse(WindowManager.watched(
            appActive: false,
            keyWindowOnActiveSpace: true,
            displayedByKeyWindow: target,
            target: target
        ))
    }

    /// A second ccmux window on this Space, showing something else.
    func testNotWatchedWhenTheKeyWindowShowsADifferentWorkspace() {
        XCTAssertFalse(WindowManager.watched(
            appActive: true,
            keyWindowOnActiveSpace: true,
            displayedByKeyWindow: UUID(),
            target: target
        ))
    }

    /// An empty window (welcome screen) displays nothing, so it watches nothing.
    func testNotWatchedWhenTheKeyWindowDisplaysNothing() {
        XCTAssertFalse(WindowManager.watched(
            appActive: true,
            keyWindowOnActiveSpace: true,
            displayedByKeyWindow: nil,
            target: target
        ))
    }

    /// Every term is load-bearing: drop any one and the suppression stops being
    /// true. Pins that nobody "simplifies" the rule back to two conditions.
    func testEveryTermIsRequired() {
        let flags = [(false, true, true), (true, false, true), (true, true, false)]
        for (active, onSpace, showsTarget) in flags {
            XCTAssertFalse(WindowManager.watched(
                appActive: active,
                keyWindowOnActiveSpace: onSpace,
                displayedByKeyWindow: showsTarget ? target : UUID(),
                target: target
            ), "active=\(active) onSpace=\(onSpace) showsTarget=\(showsTarget) must not suppress")
        }
    }
}

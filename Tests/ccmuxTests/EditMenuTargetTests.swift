import AppKit
import XCTest
@testable import ccmux

/// Cmd+Z crashed ccmux with a SIGSEGV, and AppKit named the culprit in the crash's
/// application-specific info: "Performing @selector(undo:) from sender NSMenuItem".
///
/// The Undo item had no target, so AppKit dispatched it into the responder chain,
/// and that walk starts at `window.firstResponder`. Undo was the only Edit action
/// that walked the chain end to end — SwiftTerm implements cut/copy/paste/selectAll,
/// so those stop at the terminal — which is why Cmd+Z was the key that found a stale
/// link after a pane was closed.
///
/// An explicit target is the fix: AppKit sends straight to it and never walks. These
/// tests fail if anyone restores the bare `Selector(("undo:"))` form.
@MainActor
final class EditMenuTargetTests: XCTestCase {
    private func editMenu(of delegate: AppDelegate) throws -> NSMenu {
        // buildMainMenu installs into NSApp, which does not exist until something
        // asks for it in a test process.
        _ = NSApplication.shared
        delegate.buildMainMenu()
        let main = try XCTUnwrap(NSApp.mainMenu, "buildMainMenu did not install a main menu")
        let edit = main.items.compactMap(\.submenu).first { $0.title == "Edit" }
        return try XCTUnwrap(edit, "no Edit menu")
    }

    private func item(named title: String, in menu: NSMenu) throws -> NSMenuItem {
        try XCTUnwrap(menu.items.first { $0.title == title }, "no \(title) item")
    }

    func testUndoAndRedoAreAimedAtAnExplicitTarget() throws {
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)

        for title in ["Undo", "Redo"] {
            let entry = try item(named: title, in: menu)
            XCTAssertTrue(entry.target === delegate,
                          "\(title) has no explicit target, so AppKit will walk the responder chain")
            XCTAssertNotEqual(entry.action, Selector(("undo:")))
            XCTAssertNotEqual(entry.action, Selector(("redo:")))
        }
    }

    func testUndoKeepsItsShortcut() throws {
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)

        let undo = try item(named: "Undo", in: menu)
        XCTAssertEqual(undo.keyEquivalent, "z")
        XCTAssertEqual(undo.keyEquivalentModifierMask, [.command])

        let redo = try item(named: "Redo", in: menu)
        XCTAssertEqual(redo.keyEquivalent, "z")
        XCTAssertEqual(redo.keyEquivalentModifierMask, [.command, .shift])
    }

    /// With no key window there is nothing to undo, so the items grey out rather than
    /// offering an action that would quietly do nothing.
    func testUndoIsDisabledWhenNothingCanUndo() throws {
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)
        let undo = try item(named: "Undo", in: menu)

        XCTAssertFalse(delegate.validateMenuItem(undo))
    }

    /// Firing the action with nothing focused must be a no-op, not a trap: menu
    /// validation and the key equivalent are separate paths into it.
    func testFiringUndoWithNothingFocusedIsSafe() throws {
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)
        let undo = try item(named: "Undo", in: menu)

        _ = NSApp.sendAction(try XCTUnwrap(undo.action), to: undo.target, from: undo)
    }
}

import AppKit
import XCTest
@testable import ccmux

/// Cmd+Z crashed ccmux with a SIGSEGV, and AppKit named the culprit in the crash's
/// application-specific info: "Performing @selector(undo:) from sender NSMenuItem".
///
/// The Undo item had no target, so AppKit dispatched it into the responder chain,
/// and that walk starts at `window.firstResponder`.
///
/// Which Edit actions walk the WHOLE chain is what decides the exposure, and it
/// turns on what SwiftTerm exposes to the Objective-C runtime: `paste(_:)` and
/// `copy(_:)` are @objc and `selectAll(_:)` overrides NSResponder, so those three
/// stop at the terminal. `cut(sender:)` is neither @objc nor named `cut:`, so Cut
/// answers nobody and walks as far as Undo did — it was a second way to reach the
/// same crash, and it is targeted here too. Undo and Redo have no implementation
/// anywhere.
///
/// An explicit target is the fix: AppKit sends straight to it and never walks. These
/// tests fail if anyone restores the bare `Selector(("undo:"))` form.
@MainActor
final class EditMenuTargetTests: XCTestCase {
    private func editMenu(of delegate: AppDelegate) throws -> NSMenu {
        try XCTUnwrap(delegate.editMenuItem().submenu, "no Edit submenu")
    }

    /// The Edit menu must also be the one the app installs, not just one a test can
    /// build — otherwise these assertions could pass while the real menu differs.
    func testTheInstalledEditMenuIsTheOneUnderTest() throws {
        // buildMainMenu installs into NSApp, which does not exist until something
        // asks for it in a test process.
        _ = NSApplication.shared
        let delegate = AppDelegate()
        delegate.buildMainMenu()

        let main = try XCTUnwrap(NSApp.mainMenu, "buildMainMenu did not install a main menu")
        let installed = try XCTUnwrap(
            main.items.compactMap(\.submenu).first { $0.title == "Edit" }, "no Edit menu installed")
        let undo = try item(named: "Undo", in: installed)
        XCTAssertTrue(undo.target === delegate)
    }

    private func item(named title: String, in menu: NSMenu) throws -> NSMenuItem {
        try XCTUnwrap(menu.items.first { $0.title == title }, "no \(title) item")
    }

    /// Every Edit action that nothing in the responder chain answers must be
    /// targeted. These three are exactly those: Undo and Redo have no implementation
    /// anywhere, and SwiftTerm does not expose `cut:`.
    func testUnansweredEditActionsAreAimedAtAnExplicitTarget() throws {
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)

        for title in ["Undo", "Redo", "Cut"] {
            let entry = try item(named: title, in: menu)
            XCTAssertTrue(entry.target === delegate,
                          "\(title) has no explicit target, so AppKit will walk the responder chain")
            for bare in ["undo:", "redo:", "cut:"] {
                XCTAssertNotEqual(entry.action, Selector((bare)),
                                  "\(title) still dispatches the bare \(bare) into the chain")
            }
        }
    }

    /// The three that a terminal does answer stay on the responder chain — they
    /// resolve at the first responder, and re-routing them would change behaviour
    /// this crash never implicated.
    func testAnsweredEditActionsAreLeftOnTheResponderChain() throws {
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)

        for title in ["Copy", "Paste", "Select All"] {
            let entry = try item(named: title, in: menu)
            XCTAssertNil(entry.target, "\(title) should still resolve through the responder chain")
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
        _ = NSApplication.shared // sendAction goes through NSApp
        let delegate = AppDelegate()
        let menu = try editMenu(of: delegate)

        for title in ["Undo", "Redo", "Cut"] {
            let entry = try item(named: title, in: menu)
            _ = NSApp.sendAction(try XCTUnwrap(entry.action), to: entry.target, from: entry)
        }
    }
}

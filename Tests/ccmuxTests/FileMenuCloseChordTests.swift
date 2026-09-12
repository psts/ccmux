import AppKit
import XCTest
@testable import ccmux

/// Closing a hosted terminal tab is a tmux kill-window on the daemon with no
/// undo, so the wider verb must not sit on the reflex chord: Cmd+W is Close
/// Tab, Close Pane needs Cmd+Option+W, Close Window stays on Cmd+Shift+W.
///
/// Close Tab and Close Pane share the letter "w", so the modifier mask is the
/// whole difference. Drop the one line that sets the mask on Close Pane and
/// both items answer plain Cmd+W; AppKit then fires whichever comes first in
/// the menu. These tests pin the three chords. They run only where `swift
/// test` runs, which is a developer's Mac and no CI job (see CLAUDE.md).
@MainActor
final class FileMenuCloseChordTests: XCTestCase {
    /// The installed File menu, not one a test could build differently.
    private func installedFileMenu(of delegate: AppDelegate) throws -> NSMenu {
        _ = NSApplication.shared // buildMainMenu installs into NSApp
        delegate.buildMainMenu()
        let main = try XCTUnwrap(NSApp.mainMenu, "buildMainMenu did not install a main menu")
        return try XCTUnwrap(
            main.items.compactMap(\.submenu).first { $0.title == "File" }, "no File menu installed")
    }

    private func item(named title: String, in menu: NSMenu) throws -> NSMenuItem {
        try XCTUnwrap(menu.items.first { $0.title == title }, "no \(title) item")
    }

    /// One string per physical chord. AppKit reads an uppercase key equivalent
    /// as Shift+letter, so "W"+[.command] and "w"+[.command, .shift] must
    /// collide here too; both spellings already coexist in buildMainMenu.
    private func chord(of entry: NSMenuItem) -> String {
        var mask = entry.keyEquivalentModifierMask
        let key = entry.keyEquivalent
        if key != key.lowercased() { mask.insert(.shift) }
        return "\(key.lowercased())+\(mask.rawValue)"
    }

    func testCmdWClosesTheTabOnly() throws {
        let menu = try installedFileMenu(of: AppDelegate())
        let entry = try item(named: "Close Tab", in: menu)
        XCTAssertEqual(entry.keyEquivalent, "w")
        XCTAssertEqual(entry.keyEquivalentModifierMask, [.command])
        XCTAssertEqual(NSStringFromSelector(try XCTUnwrap(entry.action)), "closeFocusedTab")
    }

    func testClosePaneNeedsOption() throws {
        let menu = try installedFileMenu(of: AppDelegate())
        let entry = try item(named: "Close Pane", in: menu)
        XCTAssertEqual(entry.keyEquivalent, "w")
        XCTAssertEqual(entry.keyEquivalentModifierMask, [.command, .option])
        XCTAssertEqual(NSStringFromSelector(try XCTUnwrap(entry.action)), "closeFocusedPane")
    }

    /// No two Close items may answer the same chord, whatever else changes.
    func testTheThreeCloseChordsAreDistinct() throws {
        let menu = try installedFileMenu(of: AppDelegate())
        var chords = Set<String>()
        for title in ["Close Tab", "Close Pane", "Close Window"] {
            let entry = try item(named: title, in: menu)
            XCTAssertTrue(chords.insert(chord(of: entry)).inserted,
                          "\(title) shares its chord with another Close item")
        }
    }

    /// With no key window there is no focused pane, so the action must return
    /// without touching anything rather than trap on a missing controller.
    /// `perform` on purpose: the item has no target and this delegate is not
    /// NSApp.delegate, so `NSApp.sendAction` would find no responder and pass
    /// without reaching the guard at all.
    func testClosingATabWithNothingFocusedIsSafe() throws {
        let delegate = AppDelegate()
        let menu = try installedFileMenu(of: delegate)
        let entry = try item(named: "Close Tab", in: menu)
        _ = delegate.perform(try XCTUnwrap(entry.action))
    }
}

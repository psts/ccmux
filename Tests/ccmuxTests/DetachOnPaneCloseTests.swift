import AppKit
import XCTest
@testable import ccmux

/// The helper that clears a dying terminal out of the responder chain is only worth
/// anything if something calls it. `ResponderDetachTests` pins the helper; this pins
/// the call, so deleting the line from the close path fails a test instead of quietly
/// restoring the Cmd+Z use-after-free.
@MainActor
final class DetachOnPaneCloseTests: XCTestCase {
    private func pane(_ id: String) -> DaemonPane {
        try! JSONDecoder().decode(DaemonPane.self, from: Data(#"{"id":"\#(id)","cwd":"/r","title":"t"}"#.utf8))
    }

    private func makeWindow() -> NSWindow {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 400, height: 300),
            styleMask: [.titled], backing: .buffered, defer: false)
        window.makeKeyAndOrderFront(nil)
        return window
    }

    /// A hosted pane the daemon has dropped: its controller — and so its terminal
    /// view — is released by syncPanes, and the window must not be left pointing at
    /// it.
    func testSyncPanesDetachesADeadPanesTerminal() throws {
        let att = WorkspaceAttachment(
            workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1"), pane("p2")])
        let doomed = try XCTUnwrap(att.terminalView(forPane: "p2"))

        let window = makeWindow()
        window.contentView?.addSubview(doomed)
        XCTAssertTrue(window.makeFirstResponder(doomed))
        XCTAssertTrue(window.firstResponder === doomed, "precondition: the doomed pane has the keyboard")

        _ = att.syncPanes([pane("p1")]) // p2 died

        XCTAssertFalse(window.firstResponder === doomed,
                       "the window still points at a terminal whose last owner just let go")
        XCTAssertNil(doomed.superview)
    }

    /// Tearing down a whole workspace releases every controller at once, so each of
    /// its terminals needs the same treatment.
    func testDetachAllTerminalsClearsTheFocusedOne() throws {
        let att = WorkspaceAttachment(
            workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1"), pane("p2")])
        let focused = try XCTUnwrap(att.terminalView(forPane: "p1"))

        let window = makeWindow()
        window.contentView?.addSubview(focused)
        XCTAssertTrue(window.makeFirstResponder(focused))

        att.detachAllTerminals()

        XCTAssertFalse(window.firstResponder === focused)
        XCTAssertNil(focused.superview)
    }

    /// Closing one pane must not take the keyboard away from a different one.
    func testASurvivingPaneKeepsFocus() throws {
        let att = WorkspaceAttachment(
            workspaceId: UUID(), daemonId: "w1", repoPath: "/r", panes: [pane("p1"), pane("p2")])
        let survivor = try XCTUnwrap(att.terminalView(forPane: "p1"))
        let doomed = try XCTUnwrap(att.terminalView(forPane: "p2"))

        let window = makeWindow()
        window.contentView?.addSubview(survivor)
        window.contentView?.addSubview(doomed)
        XCTAssertTrue(window.makeFirstResponder(survivor))

        _ = att.syncPanes([pane("p1")])

        XCTAssertTrue(window.firstResponder === survivor, "closing another pane stole the keyboard")
    }
}

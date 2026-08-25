import AppKit
import XCTest
@testable import ccmux

/// A recorded stand-in for the attach socket, so a test can see exactly which
/// commands a controller sent (the real client owns a live socket and offers
/// nothing to observe).
private final class SpyAttach: AttachCommandSink {
    let workspaceId = "ws-test"
    var sent: [DaemonCommand] = []
    func send(_ command: DaemonCommand) { sent.append(command) }
}

/// Activation must send BOTH halves: the size re-assert (which repaints only if
/// the size actually changed daemon-side) and the explicit repaint (which covers
/// the usual case where it did not). Losing either half regresses to the
/// stale-until-Ctrl+L bug, and every other test would stay green — so this pins
/// the pairing through the one method all activation paths call.
final class RemoteTermActivationTests: XCTestCase {
    func testActivationSendsResizeThenRepaint() {
        let spy = SpyAttach()
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: spy)
        let host = NSView(frame: NSRect(x: 0, y: 0, width: 800, height: 480))
        host.addSubview(c.terminalView)

        c.reassertOnActivation()

        let t = c.terminalView.getTerminal()
        XCTAssertEqual(spy.sent, [
            .resize(pane: "p1", cols: t.cols, rows: t.rows),
            .repaint(pane: "p1"),
        ])
    }

    /// The superview guard applies to both halves: an off-screen pane must not
    /// crunch the shared size, and must not drive capture-pane either.
    func testOffScreenActivationSendsNothing() {
        let spy = SpyAttach()
        let c = RemoteTermController(paneId: "p1", workingDirectory: "/tmp", attach: spy)
        XCTAssertNil(c.terminalView.superview, "precondition: the pane is not on screen")

        c.reassertOnActivation()

        XCTAssertTrue(spy.sent.isEmpty, "an off-screen pane sent \(spy.sent)")
    }
}

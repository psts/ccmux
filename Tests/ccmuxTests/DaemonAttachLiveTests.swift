import XCTest
@testable import ccmux

/// Live integration test for the native lens ↔ ccmuxd attach path. Skips unless a
/// real daemon + workspace are provided via env:
///
///   CCMUXD_URL=http://127.0.0.1:7900 \
///   CCMUX_LIVE_WS=<workspace-uuid> \
///   swift test --filter DaemonAttachLiveTests
///
/// It drives the actual `DaemonAttachClient` against a real tmux-backed pane:
/// asserts the initial snapshot/output arrives, then that typed input round-trips
/// back as output — the end-to-end proof the flip rests on.
final class DaemonAttachLiveTests: XCTestCase {

    private func liveWorkspace() throws -> String {
        guard let ws = ProcessInfo.processInfo.environment["CCMUX_LIVE_WS"], !ws.isEmpty else {
            throw XCTSkip("set CCMUX_LIVE_WS to a live daemon workspace id to run")
        }
        return ws
    }

    func testAttachStreamsSnapshotThenEchoesInput() throws {
        let wsId = try liveWorkspace()
        let client = DaemonAttachClient(workspaceId: wsId)

        var buffer = Data()
        var firstPaneId: String?
        let sawStartup = expectation(description: "startup output arrives")
        let sawEcho = expectation(description: "typed input echoes back")
        var startupSeen = false
        var echoSent = false

        client.onEvent = { event in
            switch event {
            case .hello(let panes):
                firstPaneId = panes.first?.id
            case .snapshot(_, let bytes), .output(_, let bytes):
                buffer.append(contentsOf: bytes)
                let text = String(decoding: buffer, as: UTF8.self)
                if !startupSeen, text.contains("HOSTED_PANE_READY") {
                    startupSeen = true
                    sawStartup.fulfill()
                    // Now prove input round-trips: type a unique token + Enter.
                    if let pane = firstPaneId, !echoSent {
                        echoSent = true
                        buffer.removeAll()
                        client.send(.input(pane: pane, bytes: ArraySlice(Array("echo RT_9Z_TOKEN\r".utf8))))
                    }
                } else if startupSeen, text.contains("RT_9Z_TOKEN") {
                    sawEcho.fulfill()
                }
            default:
                break
            }
        }

        client.connect()
        wait(for: [sawStartup, sawEcho], timeout: 15)
        client.disconnect()
        XCTAssertNotNil(firstPaneId, "hello frame should carry the pane id")
    }
}

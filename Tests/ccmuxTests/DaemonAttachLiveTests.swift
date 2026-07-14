import XCTest
import Darwin
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

    /// Live proof the native firehose client speaks `/v1/events`: connect the real
    /// `DaemonEventsClient` to a running daemon and assert a decoded `hello` arrives
    /// carrying current attention for the live workspace — the sidebar-flash feed a
    /// lens rides without holding a per-workspace attach.
    func testFirehoseHelloArrivesFromLiveDaemon() throws {
        let wsId = try liveWorkspace()
        let client = DaemonEventsClient()

        let sawHello = expectation(description: "firehose hello arrives")
        var helloForWorkspace = false
        client.onEvent = { event in
            if case .hello(let entries) = event {
                helloForWorkspace = entries.contains { $0.workspace == wsId }
                sawHello.fulfill()
            }
        }

        client.connect()
        wait(for: [sawHello], timeout: 15)
        client.disconnect()
        XCTAssertTrue(helloForWorkspace, "hello should seed attention for the live workspace")
    }

    /// End-to-end proof of the headline feature: fire a real Claude Code hook at the
    /// daemon's hooks socket and assert the native firehose client sees the resulting
    /// `needs_input` attention change for the workspace — *without any attach*. Needs
    /// `CCMUX_HOOKS_SOCK` (defaults to the daemon's `/tmp/ccmux-hooks.sock`).
    func testFirehoseDeliversHookAttentionChange() throws {
        let wsId = try liveWorkspace()
        let hookSock = ProcessInfo.processInfo.environment["CCMUX_HOOKS_SOCK"] ?? "/tmp/ccmux-hooks.sock"
        let client = DaemonEventsClient()

        let sawNeedsInput = expectation(description: "firehose reports needs_input from a hook")
        var firedHook = false
        client.onEvent = { event in
            switch event {
            case .hello(let entries):
                // Fire the hook for a real pane of this workspace once connected.
                guard !firedHook, let pane = entries.first(where: { $0.workspace == wsId })?.pane else { return }
                firedHook = true
                let msg = #"{"type":"permission_request","cwd":"/tmp","pane_id":"\#(pane)"}"#
                XCTAssertTrue(Self.writeUnixSocket(path: hookSock, payload: msg), "hook write should succeed")
            case .attention(let workspace, _, let state):
                if workspace == wsId, state == .needsInput { sawNeedsInput.fulfill() }
            case .unknown:
                break
            }
        }

        client.connect()
        wait(for: [sawNeedsInput], timeout: 15)
        client.disconnect()
    }

    /// Minimal blocking write to a Unix-domain stream socket (the daemon's hooks
    /// wire) — no process spawn, just POSIX connect+write.
    private static func writeUnixSocket(path: String, payload: String) -> Bool {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return false }
        defer { close(fd) }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(path.utf8CString)
        guard pathBytes.count <= MemoryLayout.size(ofValue: addr.sun_path) else { return false }
        withUnsafeMutablePointer(to: &addr.sun_path) {
            $0.withMemoryRebound(to: CChar.self, capacity: pathBytes.count) { dst in
                for (i, b) in pathBytes.enumerated() { dst[i] = b }
            }
        }
        let connected = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                connect(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connected == 0 else { return false }
        let bytes = Array(payload.utf8)
        let written = bytes.withUnsafeBytes { write(fd, $0.baseAddress, $0.count) }
        return written == bytes.count
    }
}

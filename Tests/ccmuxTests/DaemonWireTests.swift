import XCTest
@testable import ccmux

/// Pins the lens-side decode of ccmuxd's REST + attach-WS contract
/// (daemon/internal/{model,api}). These are the seams the native flip rides on;
/// a silent field-shape drift here would blank a hosted pane.
final class DaemonWireTests: XCTestCase {

    // MARK: - REST workspace/pane decode

    func testWorkspaceDecodesWithLivePanes() throws {
        let json = """
        {"id":"ws-1","name":"ccmux","repoPath":"/repo","createdBy":"patric",
         "createdAt":1720000000000,"tmuxSession":"ccmux-ccmux-abcd1234","status":"live",
         "layoutVersion":0,
         "panes":[{"id":"pane-1","workspaceId":"ws-1","title":"claude","cwd":"/repo",
                   "status":"live","attention":"needs_input"}]}
        """
        let ws = try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))
        XCTAssertEqual(ws.id, "ws-1")
        XCTAssertTrue(ws.isLive)
        XCTAssertEqual(ws.panes.count, 1)
        XCTAssertEqual(ws.panes[0].id, "pane-1")
        XCTAssertEqual(ws.panes[0].attention, .needsInput)
    }

    func testWorkspaceToleratesNullPanesAndMissingOptionals() throws {
        // Go marshals an empty ([]*Pane) slice as `null`, and omitempty drops layoutJson.
        let json = #"{"id":"ws-2","name":"x","repoPath":"/r","status":"cold","layoutVersion":0,"panes":null}"#
        let ws = try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))
        XCTAssertEqual(ws.status, .cold)
        XCTAssertFalse(ws.isLive)
        XCTAssertTrue(ws.panes.isEmpty)
    }

    func testUnknownStatusAndAttentionDoNotThrow() throws {
        let json = #"{"id":"p","title":"t","cwd":"/","status":"zombie","attention":"pondering"}"#
        let p = try JSONDecoder().decode(DaemonPane.self, from: Data(json.utf8))
        XCTAssertEqual(p.status, .unknown)
        XCTAssertEqual(p.attention, .unknown)
    }

    // MARK: - Attention → app flash mapping

    func testAttentionStateMapping() {
        XCTAssertEqual(DaemonAttention.needsInput.appAttentionState, .needsInput)
        XCTAssertEqual(DaemonAttention.done.appAttentionState, .done)
        XCTAssertEqual(DaemonAttention.running.appAttentionState, .none)
        XCTAssertEqual(DaemonAttention.idle.appAttentionState, .none)
        XCTAssertEqual(DaemonAttention.unknown.appAttentionState, .none)
    }

    // MARK: - Attach frame codec (server → client)

    func testHelloFrameDecodes() {
        let text = #"{"t":"hello","panes":[{"id":"p1","title":"claude","cwd":"/repo","attention":"idle"}]}"#
        guard case .hello(let panes)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected hello")
        }
        XCTAssertEqual(panes.count, 1)
        XCTAssertEqual(panes[0].id, "p1")
    }

    func testOutputFrameBase64RoundTrips() {
        let payload: [UInt8] = Array("hello\u{1b}[0m\r\n".utf8)
        let b64 = Data(payload).base64EncodedString()
        let text = #"{"t":"output","pane":"p1","data":"\#(b64)"}"#
        guard case .output(let pane, let bytes)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected output")
        }
        XCTAssertEqual(pane, "p1")
        XCTAssertEqual(bytes, payload)
    }

    func testSnapshotFrameDecodes() {
        let b64 = Data("screen".utf8).base64EncodedString()
        let text = #"{"t":"snapshot","pane":"p2","data":"\#(b64)"}"#
        guard case .snapshot(let pane, let bytes)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected snapshot")
        }
        XCTAssertEqual(pane, "p2")
        XCTAssertEqual(bytes, Array("screen".utf8))
    }

    func testAttentionFrameDecodes() {
        let text = #"{"t":"attention","pane":"p1","state":"done"}"#
        guard case .attention(let pane, let state)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected attention")
        }
        XCTAssertEqual(pane, "p1")
        XCTAssertEqual(state, .done)
    }

    func testPresenceFrameDecodes() {
        let text = """
        {"t":"presence","clients":[
          {"id":"1","user":"Patric","readonly":false,"driving":true,"verified":true},
          {"id":"2","user":"bob","device":"iphone","readonly":true,"driving":false,"verified":false}]}
        """
        guard case .presence(let clients)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected presence")
        }
        XCTAssertEqual(clients.count, 2)
        XCTAssertTrue(clients[0].driving)
        XCTAssertTrue(clients[1].readonly)
    }

    func testPaneAddedAndClosedDecode() {
        if case .paneAdded(let p)? = DaemonEvent.decode(text: #"{"t":"pane-added","pane":"p9"}"#) {
            XCTAssertEqual(p, "p9")
        } else { XCTFail("expected pane-added") }
        if case .paneClosed(let p)? = DaemonEvent.decode(text: #"{"t":"pane-closed","pane":"p9"}"#) {
            XCTAssertEqual(p, "p9")
        } else { XCTFail("expected pane-closed") }
    }

    func testUnknownFrameTypeIsCaptured() {
        guard case .unknown(let t)? = DaemonEvent.decode(text: #"{"t":"future-frame"}"#) else {
            return XCTFail("expected unknown")
        }
        XCTAssertEqual(t, "future-frame")
    }

    func testMalformedJSONDecodesToNil() {
        XCTAssertNil(DaemonEvent.decode(text: "not json"))
    }

    // MARK: - Firehose frame codec (/v1/events, server → client)

    func testFirehoseHelloDecodesEntries() {
        let text = """
        {"t":"hello","attention":[
          {"workspace":"ws-1","pane":"p1","state":"needs_input"},
          {"workspace":"ws-2","pane":"p2","state":"idle"}]}
        """
        guard case .hello(let entries)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("expected hello")
        }
        XCTAssertEqual(entries.count, 2)
        XCTAssertEqual(entries[0].workspace, "ws-1")
        XCTAssertEqual(entries[0].pane, "p1")
        XCTAssertEqual(entries[0].state, .needsInput)
        XCTAssertEqual(entries[1].state, .idle)
    }

    func testFirehoseHelloWithNoAttentionDecodesEmpty() {
        guard case .hello(let entries)? = DaemonFirehoseEvent.decode(text: #"{"t":"hello"}"#) else {
            return XCTFail("expected hello")
        }
        XCTAssertTrue(entries.isEmpty)
    }

    func testFirehoseAttentionCarriesWorkspace() {
        let text = #"{"t":"attention","workspace":"ws-9","pane":"p3","state":"needs_input"}"#
        guard case .attention(let workspace, let pane, let state)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("expected attention")
        }
        XCTAssertEqual(workspace, "ws-9")
        XCTAssertEqual(pane, "p3")
        XCTAssertEqual(state, .needsInput)
    }

    func testFirehoseUnknownFrameIsCaptured() {
        guard case .unknown(let t)? = DaemonFirehoseEvent.decode(text: #"{"t":"workspace-added","workspace":"w"}"#) else {
            return XCTFail("expected unknown")
        }
        XCTAssertEqual(t, "workspace-added")
    }

    func testFirehoseMalformedJSONDecodesToNil() {
        XCTAssertNil(DaemonFirehoseEvent.decode(text: "}{"))
    }

    // MARK: - Attach command codec (client → server)

    func testInputCommandEncodesBase64() throws {
        let cmd = DaemonCommand.input(pane: "p1", bytes: ArraySlice(Array("ls\r".utf8)))
        let obj = try XCTUnwrap(cmd.jsonData().flatMap {
            try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
        })
        XCTAssertEqual(obj["t"] as? String, "input")
        XCTAssertEqual(obj["pane"] as? String, "p1")
        let decoded = Data(base64Encoded: obj["data"] as! String)!
        XCTAssertEqual([UInt8](decoded), Array("ls\r".utf8))
    }

    func testResizeCommandEncodes() throws {
        let cmd = DaemonCommand.resize(pane: "p1", cols: 120, rows: 40)
        let obj = try XCTUnwrap(cmd.jsonData().flatMap {
            try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
        })
        XCTAssertEqual(obj["t"] as? String, "resize")
        XCTAssertEqual(obj["cols"] as? Int, 120)
        XCTAssertEqual(obj["rows"] as? Int, 40)
    }

    func testFocusCommandEncodes() throws {
        let cmd = DaemonCommand.focus(pane: "p3")
        let obj = try XCTUnwrap(cmd.jsonData().flatMap {
            try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
        })
        XCTAssertEqual(obj["t"] as? String, "focus")
        XCTAssertEqual(obj["pane"] as? String, "p3")
    }
}

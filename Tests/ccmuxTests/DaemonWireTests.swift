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
         "layoutVersion":0,"group":"ChartLabs",
         "panes":[{"id":"pane-1","workspaceId":"ws-1","title":"claude","cwd":"/repo",
                   "status":"live","attention":"needs_input"}]}
        """
        let ws = try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))
        XCTAssertEqual(ws.id, "ws-1")
        XCTAssertEqual(ws.group, "ChartLabs")
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

    // MARK: - Git dashboard (workspace.git) decode

    func testWorkspaceGitDecodesAndMapsToInfo() throws {
        let json = """
        {"id":"ws-g","name":"x","repoPath":"/r","status":"live","layoutVersion":0,"panes":null,
         "git":{"isGitRepo":true,"branch":"feature","trackingBranch":"origin/feature",
                "ahead":2,"behind":1,"defaultBranch":"main","aheadOfDefault":4,
                "modifiedFiles":[{"path":"a/b.txt","status":"M"}],
                "untrackedFiles":[{"path":"new.log","status":"?"}]}}
        """
        let ws = try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))
        let info = try XCTUnwrap(ws.git).asInfo
        XCTAssertTrue(info.isGitRepo)
        XCTAssertEqual(info.branch, "feature")
        XCTAssertEqual(info.trackingBranch, "origin/feature")
        XCTAssertEqual(info.ahead, 2)
        XCTAssertEqual(info.behind, 1)
        XCTAssertEqual(info.defaultBranch, "main")
        XCTAssertEqual(info.aheadOfDefault, 4)
        XCTAssertEqual(info.behindDefault, 0, "omitted count defaults to 0")
        XCTAssertEqual(info.modifiedFiles.map(\.path), ["a/b.txt"])
        XCTAssertEqual(info.untrackedFiles.first?.status, .untracked)
        XCTAssertEqual(info.totalChanges, 2)
    }

    func testWorkspaceWithoutGitDecodes() throws {
        // Daemon hasn't collected yet (git omitted) — old-daemon compat too.
        let json = #"{"id":"ws-h","name":"x","repoPath":"/r","status":"live","layoutVersion":0,"panes":null}"#
        let ws = try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))
        XCTAssertNil(ws.git)
    }

    func testFirehoseWorkspaceGitKindTriggersRefetch() {
        let text = #"{"t":"workspace-git","workspace":"ws-1"}"#
        guard case .workspaceChanged(let kind, let ws)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("expected workspaceChanged")
        }
        XCTAssertEqual(kind, "workspace-git")
        XCTAssertEqual(ws, "ws-1")
    }

    // MARK: - Projects (GET /v1/projects) decode

    func testProjectListDecodes() throws {
        let json = """
        {"root":"/srv/projects","path":"","projects":[
          {"name":"alpha","path":"/srv/projects/alpha","git":true},
          {"name":"beta","path":"/srv/projects/beta","git":false}]}
        """
        let list = try JSONDecoder().decode(DaemonProjectList.self, from: Data(json.utf8))
        XCTAssertEqual(list.root, "/srv/projects")
        XCTAssertEqual(list.path, "")
        XCTAssertNil(list.parent, "no parent at the root")
        XCTAssertEqual(list.projects.map(\.name), ["alpha", "beta"])
        XCTAssertTrue(list.projects[0].git)
        XCTAssertEqual(list.projects[1].id, "/srv/projects/beta")
    }

    func testProjectListDecodesSubpathWithParent() throws {
        let json = #"{"root":"/srv/projects","path":"group/inner","parent":"group","projects":[]}"#
        let list = try JSONDecoder().decode(DaemonProjectList.self, from: Data(json.utf8))
        XCTAssertEqual(list.path, "group/inner")
        XCTAssertEqual(list.parent, "group")
    }

    func testProjectListToleratesNullProjects() throws {
        // Defensive: a Go nil slice would marshal as `null`.
        let json = #"{"root":"/srv/projects","projects":null}"#
        let list = try JSONDecoder().decode(DaemonProjectList.self, from: Data(json.utf8))
        XCTAssertTrue(list.projects.isEmpty)
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

    func testClipboardFrameDecodes() {
        // A tmux copy must arrive as its OWN case — falling into .unknown (or
        // worse, output) would drop the text or paint it into the terminal.
        let b64 = Data("copied text".utf8).base64EncodedString()
        let text = #"{"t":"clipboard","pane":"%3","data":"\#(b64)"}"#
        guard case .clipboard(let pane, let bytes)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected clipboard")
        }
        XCTAssertEqual(pane, "%3")
        XCTAssertEqual(String(bytes: bytes, encoding: .utf8), "copied text")
    }

    func testNoticeFrameDecodes() {
        // A truncated paste must arrive as its OWN case. Falling into .unknown
        // would silently drop the only signal the user gets that half their
        // paste is sitting in the pane — the whole reason the frame exists.
        // `notice` is plain prose, NOT base64 like `data`.
        let text = #"{"t":"notice","pane":"%3","notice":"Paste was cut short: 10 of 99 bytes reached this pane."}"#
        guard case .notice(let pane, let body)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected notice")
        }
        XCTAssertEqual(pane, "%3")
        XCTAssertTrue(body.contains("cut short"), "notice text lost: \(body)")
        XCTAssertTrue(body.contains("10"), "notice lost its byte counts: \(body)")
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

    func testPaneSizeFrameDecodes() {
        guard case .paneSize(let pane, let cols, let rows)? =
            DaemonEvent.decode(text: #"{"t":"pane-size","pane":"p1","cols":137,"rows":42}"#) else {
            return XCTFail("expected pane-size")
        }
        XCTAssertEqual(pane, "p1")
        XCTAssertEqual(cols, 137)
        XCTAssertEqual(rows, 42)
    }

    func testHelloPaneCarriesSize() {
        let text = #"{"t":"hello","panes":[{"id":"p1","title":"c","cwd":"/r","attention":"idle","cols":100,"rows":30}]}"#
        guard case .hello(let panes)? = DaemonEvent.decode(text: text) else {
            return XCTFail("expected hello")
        }
        XCTAssertEqual(panes[0].cols, 100)
        XCTAssertEqual(panes[0].rows, 30)
        // Older daemons omit cols/rows → tolerated as 0.
        let old = #"{"t":"hello","panes":[{"id":"p1","title":"c","cwd":"/r"}]}"#
        guard case .hello(let p2)? = DaemonEvent.decode(text: old) else { return XCTFail("expected hello") }
        XCTAssertEqual(p2[0].cols, 0)
    }

    // Tolerating unknown frames is what makes every new frame kind additive for
    // an older lens — `notice` was added this way. If this ever throws or
    // crashes instead, shipping a new frame becomes a breaking change for
    // everyone not yet updated.
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
        guard case .attention(let workspace, let pane, let state, _)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("expected attention")
        }
        XCTAssertEqual(workspace, "ws-9")
        XCTAssertEqual(pane, "p3")
        XCTAssertEqual(state, .needsInput)
    }

    func testFirehoseWorkspaceLifecycleDecodes() {
        for kind in ["workspace-added", "workspace-removed", "workspace-status"] {
            guard case .workspaceChanged(let k, let ws)? = DaemonFirehoseEvent.decode(text: #"{"t":"\#(kind)","workspace":"w1"}"#) else {
                return XCTFail("expected workspaceChanged for \(kind)")
            }
            XCTAssertEqual(k, kind)
            XCTAssertEqual(ws, "w1")
        }
    }

    func testFirehoseUnknownFrameIsCaptured() {
        guard case .unknown(let t)? = DaemonFirehoseEvent.decode(text: #"{"t":"future-firehose-frame"}"#) else {
            return XCTFail("expected unknown")
        }
        XCTAssertEqual(t, "future-firehose-frame")
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
        let cmd = DaemonCommand.focus(pane: "p3", present: true)
        let obj = try XCTUnwrap(cmd.jsonData().flatMap {
            try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
        })
        XCTAssertEqual(obj["t"] as? String, "focus")
        XCTAssertEqual(obj["pane"] as? String, "p3")
        XCTAssertEqual(obj["present"] as? Bool, true)
    }

    /// Repaint carries no size and no data: it only names the pane whose screen
    /// the daemon should re-capture. The daemon matches on `t == "repaint"`
    /// exactly; a drifted spelling would be silently dropped by its read switch
    /// and the activated pane would stay stale — the bug the verb exists to fix.
    func testRepaintCommandEncodes() throws {
        let cmd = DaemonCommand.repaint(pane: "p4")
        let obj = try XCTUnwrap(cmd.jsonData().flatMap {
            try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
        })
        XCTAssertEqual(obj["t"] as? String, "repaint")
        XCTAssertEqual(obj["pane"] as? String, "p4")
        XCTAssertEqual(obj.count, 2, "repaint is pane-only; stray fields invite contract drift")
    }

    /// Presence must be sent even when it is false and no pane is focused. The
    /// daemon reads an absent `present` as "this lens is too old to know" and falls
    /// back to treating a focused pane as presence — so omitting it here would
    /// silently restore the behaviour the field exists to replace.
    func testFocusCommandCarriesPresenceWhenAbsentAndUnfocused() throws {
        let cmd = DaemonCommand.focus(pane: "", present: false)
        let obj = try XCTUnwrap(cmd.jsonData().flatMap {
            try? JSONSerialization.jsonObject(with: $0) as? [String: Any]
        })
        XCTAssertEqual(obj["pane"] as? String, "")
        XCTAssertEqual(obj["present"] as? Bool, false, "present must be explicit, never omitted")
    }
}

// MARK: - The daemon owns the alert decision

/// The app must not decide whether an attention warrants a notification; it obeys
/// the daemon's `alert` flag. The two sides kept their own copies of the rule and
/// drifted twice — most recently the app alerted on every `done` long after the
/// daemon had stopped pushing on them, turning one burst of background agents into
/// an alert per agent.
final class FirehoseAlertFlagTests: XCTestCase {
    func testAlertFlagIsDecoded() {
        let text = #"{"t":"attention","workspace":"w1","pane":"p1","state":"needs_input","alert":true}"#
        guard case .attention(_, _, _, let alert)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("did not decode as an attention event")
        }
        XCTAssertTrue(alert, "the daemon asked for an alert and the app must carry it through")
    }

    /// A daemon that predates the flag omits it entirely. That has to read as "do
    /// not alert" rather than crashing or defaulting to noisy.
    func testMissingAlertFlagDefaultsToSilent() {
        let text = #"{"t":"attention","workspace":"w1","pane":"p1","state":"needs_input"}"#
        guard case .attention(_, _, _, let alert)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("an attention frame without the flag must still decode")
        }
        XCTAssertFalse(alert, "an absent flag means an older daemon; stay quiet rather than guess")
    }

    func testAlertFalseIsRespected() {
        let text = #"{"t":"attention","workspace":"w1","pane":"p1","state":"needs_input","alert":false}"#
        guard case .attention(_, _, _, let alert)? = DaemonFirehoseEvent.decode(text: text) else {
            return XCTFail("did not decode as an attention event")
        }
        XCTAssertFalse(alert)
    }
}

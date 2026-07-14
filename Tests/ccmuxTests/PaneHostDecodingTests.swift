import XCTest
@testable import ccmux

/// The lens pivot adds `TerminalConfig.host` and `Workspace.mode`. Every state.json
/// written before the pivot omits both keys; these tests pin that such files still
/// decode (to the untouched local path) rather than throwing `keyNotFound`.
final class PaneHostDecodingTests: XCTestCase {

    // MARK: - PaneHost round-trip

    func testPaneHostLocalRoundTrips() throws {
        let data = try JSONEncoder().encode(PaneHost.local)
        XCTAssertEqual(try JSONDecoder().decode(PaneHost.self, from: data), .local)
    }

    func testPaneHostHostedRoundTrips() throws {
        let host = PaneHost.hosted(paneId: "pane-abc")
        let data = try JSONEncoder().encode(host)
        XCTAssertEqual(try JSONDecoder().decode(PaneHost.self, from: data), host)
        XCTAssertEqual(host.hostedPaneId, "pane-abc")
        XCTAssertTrue(host.isHosted)
        XCTAssertFalse(PaneHost.local.isHosted)
        XCTAssertNil(PaneHost.local.hostedPaneId)
    }

    // MARK: - TerminalConfig legacy decode

    func testLegacyTerminalConfigDecodesAsLocalHost() throws {
        // A pre-pivot config: no `host` key.
        let json = #"{"id":"11111111-1111-1111-1111-111111111111","shell":"/bin/zsh","workingDirectory":"/repo"}"#
        let config = try JSONDecoder().decode(TerminalConfig.self, from: Data(json.utf8))
        XCTAssertEqual(config.host, .local)
        XCTAssertEqual(config.workingDirectory, "/repo")
    }

    func testTerminalConfigWithHostedHostRoundTrips() throws {
        var config = TerminalConfig(id: UUID(), shell: "/bin/zsh", workingDirectory: "/repo")
        config.host = .hosted(paneId: "p42")
        let data = try JSONEncoder().encode(config)
        let back = try JSONDecoder().decode(TerminalConfig.self, from: data)
        XCTAssertEqual(back.host, .hosted(paneId: "p42"))
    }

    func testMemberwiseInitDefaultsToLocal() {
        // The synthesized memberwise init must survive the custom decoder extension.
        let config = TerminalConfig(id: UUID(), shell: "/bin/zsh", workingDirectory: "/repo")
        XCTAssertEqual(config.host, .local)
    }

    // MARK: - Workspace legacy decode

    func testLegacyWorkspaceDecodesAsLocalMode() throws {
        let ws = Workspace.create(name: "demo", repoPath: "/repo")
        // Re-encode then strip the `mode` key to simulate a pre-pivot file.
        var obj = try JSONSerialization.jsonObject(with: JSONEncoder().encode(ws)) as! [String: Any]
        obj.removeValue(forKey: "mode")
        let legacy = try JSONSerialization.data(withJSONObject: obj)
        let decoded = try JSONDecoder().decode(Workspace.self, from: legacy)
        XCTAssertEqual(decoded.mode, .local)
        XCTAssertEqual(decoded.name, "demo")
    }

    func testHostedWorkspaceModeRoundTrips() throws {
        var ws = Workspace.create(name: "demo", repoPath: "/repo")
        ws.mode = .hosted
        let data = try JSONEncoder().encode(ws)
        XCTAssertEqual(try JSONDecoder().decode(Workspace.self, from: data).mode, .hosted)
    }
}

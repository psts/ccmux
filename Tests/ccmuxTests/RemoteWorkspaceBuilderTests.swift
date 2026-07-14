import XCTest
@testable import ccmux

/// Pins how hosted daemon workspaces become app-side SplitTree layouts, and that the
/// id mapping is stable (unstable ids would churn the terminal views on every poll).
final class RemoteWorkspaceBuilderTests: XCTestCase {

    private func pane(_ id: String, cwd: String = "/repo", title: String = "") -> DaemonPane {
        let json = #"{"id":"\#(id)","cwd":"\#(cwd)","title":"\#(title)"}"#
        return try! JSONDecoder().decode(DaemonPane.self, from: Data(json.utf8))
    }

    func testWorkspaceUUIDParsesRealUUID() {
        let id = "11111111-2222-3333-4444-555555555555"
        XCTAssertEqual(RemoteWorkspaceBuilder.workspaceUUID(id), UUID(uuidString: id))
    }

    func testDeterministicUUIDIsStableAndDistinct() {
        // Non-UUID id → deterministic fallback: same seed same id, different seed different id.
        let a1 = RemoteWorkspaceBuilder.paneUUID("not-a-uuid")
        let a2 = RemoteWorkspaceBuilder.paneUUID("not-a-uuid")
        let b = RemoteWorkspaceBuilder.paneUUID("other-seed")
        XCTAssertEqual(a1, a2)
        XCTAssertNotEqual(a1, b)
    }

    func testTerminalConfigIsHostedAndFallsBackToRepoPath() {
        let cfg = RemoteWorkspaceBuilder.terminalConfig(for: pane("p1", cwd: ""), repoPath: "/repo")
        XCTAssertEqual(cfg.host, .hosted(paneId: "p1"))
        XCTAssertEqual(cfg.workingDirectory, "/repo")           // empty cwd → repoPath
        XCTAssertEqual(cfg.id, RemoteWorkspaceBuilder.paneUUID("p1"))
    }

    func testBuildTreeEmptyReturnsNil() {
        XCTAssertNil(RemoteWorkspaceBuilder.buildTree(panes: [], repoPath: "/repo"))
    }

    func testBuildTreeSinglePane() throws {
        let (tree, focused) = try XCTUnwrap(
            RemoteWorkspaceBuilder.buildTree(panes: [pane("p1")], repoPath: "/repo"))
        XCTAssertEqual(tree.allLeaves.count, 1)
        XCTAssertEqual(tree.allLeaves.first?.id, focused)
    }

    func testBuildTreeMultiPaneAllHosted() throws {
        let panes = [pane("p1"), pane("p2"), pane("p3")]
        let (tree, focused) = try XCTUnwrap(
            RemoteWorkspaceBuilder.buildTree(panes: panes, repoPath: "/repo"))
        let leaves = tree.allLeaves
        XCTAssertEqual(leaves.count, 3)
        // Focus lands on the first pane's leaf.
        XCTAssertTrue(leaves.contains { $0.id == focused })
        // Every leaf hosts exactly one hosted terminal.
        let hostedIds: [String] = leaves.compactMap { leaf in
            guard case .terminal(let cfg) = leaf.content.tabs[0] else { return nil }
            return cfg.host.hostedPaneId
        }
        XCTAssertEqual(Set(hostedIds), ["p1", "p2", "p3"])
    }

    func testPaneSignatureTracksIdsInOrder() {
        XCTAssertEqual(
            RemoteWorkspaceBuilder.paneSignature([pane("a"), pane("b")]), ["a", "b"])
    }
}

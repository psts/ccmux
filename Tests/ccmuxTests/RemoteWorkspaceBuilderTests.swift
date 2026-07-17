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

    // MARK: - insertingPane (user-initiated hosted terminal placement)

    private func twoLeafTree() -> SplitTree<PaneTabs> {
        let a = PaneTabs(single: .terminal(RemoteWorkspaceBuilder.terminalConfig(for: pane("p1"), repoPath: "/r")))
        let b = PaneTabs(single: .terminal(RemoteWorkspaceBuilder.terminalConfig(for: pane("p2"), repoPath: "/r")))
        return .split(id: UUID(), direction: .horizontal, ratio: 0.5,
                      first: .leaf(id: a.id, content: a), second: .leaf(id: b.id, content: b))
    }

    func testInsertingPaneAsTabOnClickedLeaf() throws {
        let tree = twoLeafTree()
        let firstLeaf = tree.allLeaves[0].id
        let placed = try XCTUnwrap(RemoteWorkspaceBuilder.insertingPane(
            pane("p3"), into: tree, at: firstLeaf, direction: nil, repoPath: "/r"))
        XCTAssertEqual(placed.focusLeafId, firstLeaf)
        let leaf = try XCTUnwrap(placed.tree.allLeaves.first { $0.id == firstLeaf })
        XCTAssertEqual(leaf.content.tabs.count, 2, "tab appended to the clicked leaf, not the last one")
        XCTAssertEqual(leaf.content.activeTabId, RemoteWorkspaceBuilder.paneUUID("p3"))
    }

    func testInsertingPaneAsSplitOfClickedLeaf() throws {
        let tree = twoLeafTree()
        let firstLeaf = tree.allLeaves[0].id
        let placed = try XCTUnwrap(RemoteWorkspaceBuilder.insertingPane(
            pane("p3"), into: tree, at: firstLeaf, direction: .vertical, repoPath: "/r"))
        XCTAssertEqual(placed.tree.leafCount, 3)
        let newLeaf = try XCTUnwrap(placed.tree.allLeaves.first { $0.id == placed.focusLeafId })
        XCTAssertEqual(newLeaf.content.tabs.first?.id, RemoteWorkspaceBuilder.paneUUID("p3"), "focus lands on the new split leaf")
    }

    func testInsertingPaneDedupesWhenReconcileWonTheRace() {
        let tree = twoLeafTree()
        XCTAssertNil(RemoteWorkspaceBuilder.insertingPane(
            pane("p2"), into: tree, at: tree.allLeaves[0].id, direction: nil, repoPath: "/r"),
            "pane already merged in — leave it where the merge put it")
    }

    // MARK: - updatingTitles (live pane-title sync into the tree)

    func testUpdatingTitlesRewritesChangedHostedTitles() throws {
        let tree = twoLeafTree()
        let renamed = try! JSONDecoder().decode(
            DaemonPane.self, from: Data(#"{"id":"p1","cwd":"/r","title":"Claude"}"#.utf8))
        let updated = try XCTUnwrap(RemoteWorkspaceBuilder.updatingTitles(tree, panes: [renamed, pane("p2")]))
        guard case .terminal(let cfg) = updated.allLeaves[0].content.tabs[0] else { return XCTFail() }
        XCTAssertEqual(cfg.title, "Claude")
        guard case .terminal(let cfg2) = updated.allLeaves[1].content.tabs[0] else { return XCTFail() }
        XCTAssertEqual(cfg2.title, nil, "empty daemon title stays nil")
    }

    func testUpdatingTitlesReturnsNilWhenNothingChanged() {
        let tree = twoLeafTree()
        XCTAssertNil(RemoteWorkspaceBuilder.updatingTitles(tree, panes: [pane("p1"), pane("p2")]),
                     "no change → nil, so callers skip republishing")
    }

    func testInsertingPaneFallsBackToLastLeafWhenTargetVanished() throws {
        let tree = twoLeafTree()
        let placed = try XCTUnwrap(RemoteWorkspaceBuilder.insertingPane(
            pane("p3"), into: tree, at: UUID(), direction: .vertical, repoPath: "/r"))
        XCTAssertEqual(placed.tree.leafCount, 2, "vanished target → tab on the last leaf, no split")
        let last = try XCTUnwrap(placed.tree.allLeaves.last)
        XCTAssertEqual(last.content.tabs.count, 2)
        XCTAssertEqual(placed.focusLeafId, last.id)
    }
}

import XCTest
@testable import ccmux

/// Pins the lens-side layout-blob codec + restore (Phase 7 layout sync). These are
/// the pure seams: a hosted workspace serializes its SplitTree to the blob the
/// daemon versions, and on the way back merges it with the live pane set — pruning
/// dead panes, landing new ones as tabs — so daemon-side pane churn never resets
/// the arrangement. Only a garbage or fully-dead blob falls back to the chain.
final class HostedLayoutTests: XCTestCase {

    private func panes(_ ids: [String]) -> [DaemonPane] {
        let json = "[" + ids.map { #"{"id":"\#($0)","title":"t","cwd":"/r"}"# }.joined(separator: ",") + "]"
        return try! JSONDecoder().decode([DaemonPane].self, from: Data(json.utf8))
    }

    /// A non-default arrangement (vertical, ratio 0.25) so it's distinguishable from
    /// buildTree's default (horizontal, 0.5) chain.
    private func verticalTree(_ ps: [DaemonPane]) -> SplitTree<PaneTabs> {
        let a = PaneTabs(single: .terminal(RemoteWorkspaceBuilder.terminalConfig(for: ps[0], repoPath: "/r")))
        let b = PaneTabs(single: .terminal(RemoteWorkspaceBuilder.terminalConfig(for: ps[1], repoPath: "/r")))
        return .split(id: UUID(), direction: .vertical, ratio: 0.25,
                      first: .leaf(id: a.id, content: a), second: .leaf(id: b.id, content: b))
    }

    func testCodecRoundTripIsByteStableAndPreservesPanes() {
        let tree = verticalTree(panes(["p1", "p2"]))
        let blob = HostedLayoutCodec.encode(tree)
        XCTAssertFalse(blob.isEmpty)
        XCTAssertEqual(blob, HostedLayoutCodec.encode(tree), "encoding must be deterministic")

        let decoded = HostedLayoutCodec.decode(blob)
        XCTAssertNotNil(decoded)
        XCTAssertEqual(HostedLayoutCodec.encode(decoded!), blob, "decode→encode must round-trip identically")
        XCTAssertEqual(HostedLayoutCodec.hostedPaneIds(decoded!), Set(["p1", "p2"]))
    }

    func testDecodeGarbageReturnsNil() {
        XCTAssertNil(HostedLayoutCodec.decode("not json"))
        XCTAssertNil(HostedLayoutCodec.decode(""))
    }

    func testBuildTreeRestoresSavedArrangement() {
        let ps = panes(["p1", "p2"])
        let blob = HostedLayoutCodec.encode(verticalTree(ps))
        guard let (tree, _) = RemoteWorkspaceBuilder.buildTree(panes: ps, repoPath: "/r", layoutBlob: blob) else {
            return XCTFail("nil tree")
        }
        XCTAssertEqual(HostedLayoutCodec.encode(tree), blob, "restored tree should equal the saved arrangement")
    }

    func testBuildTreeFallsBackWhenBlobPaneSetIsStale() {
        let ps = panes(["p1", "p2"])
        // Blob refers to panes that no longer exist → must not be trusted.
        let staleBlob = HostedLayoutCodec.encode(verticalTree(panes(["p9", "p8"])))
        guard let (tree, _) = RemoteWorkspaceBuilder.buildTree(panes: ps, repoPath: "/r", layoutBlob: staleBlob) else {
            return XCTFail("nil tree")
        }
        XCTAssertEqual(HostedLayoutCodec.hostedPaneIds(tree), Set(["p1", "p2"]), "fell back over live panes")
        XCTAssertNotEqual(HostedLayoutCodec.encode(tree), staleBlob, "must not adopt the stale arrangement")
    }

    func testBuildTreeFallsBackOnGarbageBlob() {
        let ps = panes(["p1"])
        guard let (tree, _) = RemoteWorkspaceBuilder.buildTree(panes: ps, repoPath: "/r", layoutBlob: "{bogus") else {
            return XCTFail("nil tree")
        }
        XCTAssertEqual(HostedLayoutCodec.hostedPaneIds(tree), Set(["p1"]))
    }

    func testBuildTreeNilBlobUsesDefaultChain() {
        let ps = panes(["p1", "p2", "p3"])
        guard let (tree, _) = RemoteWorkspaceBuilder.buildTree(panes: ps, repoPath: "/r", layoutBlob: nil) else {
            return XCTFail("nil tree")
        }
        XCTAssertEqual(HostedLayoutCodec.hostedPaneIds(tree), Set(["p1", "p2", "p3"]))
    }

    // MARK: - Merge (daemon-side pane churn must not reset the arrangement)

    func testBuildTreeMergesNewPaneAsTabPreservingArrangement() throws {
        let ps = panes(["p1", "p2", "p3"])                  // p3 just spawned (dev-server start)
        let blob = HostedLayoutCodec.encode(verticalTree(Array(ps.prefix(2))))
        let (tree, _) = try XCTUnwrap(RemoteWorkspaceBuilder.buildTree(panes: ps, repoPath: "/r", layoutBlob: blob))
        XCTAssertEqual(HostedLayoutCodec.hostedPaneIds(tree), Set(["p1", "p2", "p3"]))
        XCTAssertEqual(tree.allLeaves.count, 2, "no new split — the pane lands as a tab")
        guard case .split(_, let dir, let ratio, _, _) = tree else { return XCTFail("expected split root") }
        XCTAssertEqual(dir, .vertical)
        XCTAssertEqual(ratio, 0.25, "saved geometry survives")
        let last = try XCTUnwrap(tree.allLeaves.last)
        XCTAssertEqual(last.content.tabs.count, 2, "new pane joins the last leaf")
        XCTAssertEqual(last.content.activeTabId, RemoteWorkspaceBuilder.paneUUID("p3"), "fresh pane's tab is active")
    }

    func testBuildTreePrunesRemovedPaneCollapsingItsLeaf() throws {
        let two = panes(["p1", "p2"])
        let blob = HostedLayoutCodec.encode(verticalTree(two))
        let (tree, _) = try XCTUnwrap(RemoteWorkspaceBuilder.buildTree(panes: [two[0]], repoPath: "/r", layoutBlob: blob))
        XCTAssertEqual(HostedLayoutCodec.hostedPaneIds(tree), Set(["p1"]))
        XCTAssertEqual(tree.allLeaves.count, 1, "emptied leaf collapses; its sibling takes the space")
    }

    func testBuildTreeStartStopRoundTripsToOriginalArrangement() throws {
        let two = panes(["p1", "p2"])
        let original = HostedLayoutCodec.encode(verticalTree(two))
        // Start: p3 appears and the arrangement gains it as a tab…
        let started = try XCTUnwrap(
            RemoteWorkspaceBuilder.buildTree(panes: panes(["p1", "p2", "p3"]), repoPath: "/r", layoutBlob: original))
        // …stop: p3 vanishes and merging the started blob back yields the original bytes.
        let stopped = try XCTUnwrap(
            RemoteWorkspaceBuilder.buildTree(panes: two, repoPath: "/r", layoutBlob: HostedLayoutCodec.encode(started.tree)))
        XCTAssertEqual(HostedLayoutCodec.encode(stopped.tree), original)
    }

    func testBuildTreePruneKeepsNonHostedTabsAndFixesActiveTab() throws {
        // A leaf pairing hosted p2 with a scratchpad tab, p2 active; p2 then dies.
        let ps = panes(["p1", "p2"])
        let p1Tabs = PaneTabs(single: .terminal(RemoteWorkspaceBuilder.terminalConfig(for: ps[0], repoPath: "/r")))
        let scratch = PaneContent.scratchpad(ScratchpadConfig(id: UUID(), title: "notes", content: ""))
        let hosted2 = PaneContent.terminal(RemoteWorkspaceBuilder.terminalConfig(for: ps[1], repoPath: "/r"))
        let mixed = PaneTabs(tabs: [hosted2, scratch], activeTabId: hosted2.id)
        let tree = SplitTree<PaneTabs>.split(
            id: UUID(), direction: .horizontal, ratio: 0.5,
            first: .leaf(id: p1Tabs.id, content: p1Tabs), second: .leaf(id: mixed.id, content: mixed))
        let (merged, _) = try XCTUnwrap(
            RemoteWorkspaceBuilder.buildTree(panes: [ps[0]], repoPath: "/r", layoutBlob: HostedLayoutCodec.encode(tree)))
        XCTAssertEqual(merged.allLeaves.count, 2, "leaf with a surviving scratchpad tab must not collapse")
        let survivor = try XCTUnwrap(merged.allLeaves.last)
        XCTAssertEqual(survivor.content.tabs.map(\.id), [scratch.id])
        XCTAssertEqual(survivor.content.activeTabId, scratch.id, "active pointer moves off the dead tab")
    }
}

import XCTest
@testable import ccmux

/// Pins the lens-side layout-blob codec + restore (Phase 7 layout sync). These are
/// the pure seams: a hosted workspace serializes its SplitTree to the blob the
/// daemon versions, and rebuilds the exact arrangement on the way back — falling
/// back to a default chain rather than rendering a stale/broken layout.
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
}

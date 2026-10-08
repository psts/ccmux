import XCTest
@testable import ccmux

/// The Mac keeps attention per pane, like the web lens. It used to keep one
/// value per workspace, so one pane going idle wiped another pane's needs_input.
final class PaneAttentionTests: XCTestCase {
    private func entry(_ pane: String, _ state: DaemonAttention, ws: String = "w") -> DaemonAttentionEntry {
        DaemonAttentionEntry(workspace: ws, pane: pane, state: state)
    }

    func testRollupNeedsInputBeatsDone() {
        let panes = ["a": entry("a", .done), "b": entry("b", .needsInput), "c": entry("c", .idle)]
        XCTAssertEqual(RemoteSessionService.rollup(panes), .needsInput)
    }

    func testOnePaneGoingIdleKeepsTheOtherLit() {
        var panes = ["a": entry("a", .needsInput), "b": entry("b", .done)]
        panes["b"] = entry("b", .idle)
        XCTAssertEqual(RemoteSessionService.rollup(panes), .needsInput)
    }

    func testRollupOfNothingIsNone() {
        XCTAssertEqual(RemoteSessionService.rollup(nil), .none)
        XCTAssertEqual(RemoteSessionService.rollup(["a": entry("a", .running)]), .none)
    }

    func testPruneDropsGoneWorkspacesAndPanes() throws {
        let json = """
        {"id":"w","name":"n","repoPath":"/r","status":"live",
         "panes":[{"id":"a","workspaceId":"w","title":"t","cwd":"/r","status":"live"}]}
        """
        let live = [try JSONDecoder().decode(DaemonWorkspace.self, from: Data(json.utf8))]
        let current = [
            "w": ["a": entry("a", .done), "closed": entry("closed", .needsInput)],
            "gone": ["x": entry("x", .needsInput, ws: "gone")],
        ]
        let pruned = RemoteSessionService.prunedAttention(current, live: live)
        XCTAssertEqual(Set(pruned.keys), ["w"])
        XCTAssertEqual(pruned["w"].map { Set($0.keys) }, ["a"])
    }
}

import XCTest
@testable import ccmux

/// Pins the hosted-workspace interception on SplitTreeController: terminal
/// creation (new tab / split) must route to the daemon hook instead of spawning
/// a local shell, and closing a hosted tab must surface its daemon pane id for a
/// remote kill. Local workspaces (hooks nil) keep the original behavior.
final class SplitTreeControllerHostedTests: XCTestCase {

    private func pane(_ id: String) -> DaemonPane {
        try! JSONDecoder().decode(DaemonPane.self, from: Data(#"{"id":"\#(id)","cwd":"/r","title":"t"}"#.utf8))
    }

    private func hosted(_ id: String) -> PaneContent {
        .terminal(RemoteWorkspaceBuilder.terminalConfig(for: pane(id), repoPath: "/r"))
    }

    func testAddLocalTerminalTabRoutesToHook() {
        let c = SplitTreeController(workingDirectory: "/r")
        var requests: [(leaf: UUID, direction: SplitDirection?)] = []
        c.onHostedTerminalRequest = { requests.append(($0, $1)) }
        let leafId = c.tree.allLeaves[0].id

        c.addTab(leafId: leafId, newContent: .defaultTerminal(workingDirectory: "/r"))
        XCTAssertEqual(requests.count, 1)
        XCTAssertEqual(requests[0].leaf, leafId)
        XCTAssertNil(requests[0].direction, "nil direction = tab, not split")
        XCTAssertEqual(c.tree.allLeaves[0].content.tabs.count, 1, "no local tab may be added")
    }

    func testAddHostedTerminalTabPassesThrough() {
        let c = SplitTreeController(workingDirectory: "/r")
        var fired = false
        c.onHostedTerminalRequest = { _, _ in fired = true }
        let leafId = c.tree.allLeaves[0].id

        c.addTab(leafId: leafId, newContent: hosted("p1"))
        XCTAssertFalse(fired, "already-hosted content is the service inserting the spawned pane")
        XCTAssertEqual(c.tree.allLeaves[0].content.tabs.count, 2)
    }

    func testAddNonTerminalTabUnaffected() {
        let c = SplitTreeController(workingDirectory: "/r")
        var fired = false
        c.onHostedTerminalRequest = { _, _ in fired = true }
        let leafId = c.tree.allLeaves[0].id

        c.addTab(leafId: leafId, newContent: .scratchpad(ScratchpadConfig(id: UUID(), title: "n", content: "")))
        XCTAssertFalse(fired, "browser/scratchpad/files tabs stay lens-local")
        XCTAssertEqual(c.tree.allLeaves[0].content.tabs.count, 2)
    }

    func testSplitRoutesToHook() {
        let c = SplitTreeController(workingDirectory: "/r")
        var requests: [(leaf: UUID, direction: SplitDirection?)] = []
        c.onHostedTerminalRequest = { requests.append(($0, $1)) }
        let leafId = c.tree.allLeaves[0].id

        c.splitPane(id: leafId, direction: .vertical)
        XCTAssertEqual(requests.count, 1)
        XCTAssertEqual(requests[0].direction, .vertical)
        XCTAssertEqual(c.tree.leafCount, 1, "no local split may happen")
    }

    func testCloseHostedTabReportsDaemonPaneId() {
        let c = SplitTreeController(workingDirectory: "/r")
        let leafId = c.tree.allLeaves[0].id
        c.addTab(leafId: leafId, newContent: hosted("p1"))   // hooks nil — plain insert
        var killed: [String] = []
        c.onHostedPaneClosed = { killed.append($0) }

        c.closeTab(leafId: leafId, tabId: RemoteWorkspaceBuilder.paneUUID("p1"))
        XCTAssertEqual(killed, ["p1"])
        XCTAssertEqual(c.tree.allLeaves[0].content.tabs.count, 1)
    }

    func testClosePaneReportsEveryHostedTab() {
        let c = SplitTreeController(workingDirectory: "/r")
        // Two leaves: the default local one, plus a leaf holding two hosted tabs.
        var doomed = PaneTabs(single: hosted("p1"))
        doomed.addTab(hosted("p2"))
        c.tree = c.tree.splitLeaf(targetId: c.tree.allLeaves[0].id, direction: .horizontal, newContent: doomed)
        var killed: [String] = []
        c.onHostedPaneClosed = { killed.append($0) }

        c.closePane(id: c.tree.allLeaves[1].id)
        XCTAssertEqual(killed.sorted(), ["p1", "p2"])
        XCTAssertEqual(c.tree.leafCount, 1)
    }

    func testLocalWorkspaceKeepsLocalBehavior() {
        let c = SplitTreeController(workingDirectory: "/r")   // hooks nil
        let leafId = c.tree.allLeaves[0].id
        c.addTab(leafId: leafId, newContent: .defaultTerminal(workingDirectory: "/r"))
        XCTAssertEqual(c.tree.allLeaves[0].content.tabs.count, 2, "local workspace adds a local tab")
        c.splitPane(id: leafId, direction: .horizontal)
        XCTAssertEqual(c.tree.leafCount, 2, "local workspace splits locally")
    }
}

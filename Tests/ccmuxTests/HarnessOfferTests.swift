import XCTest
@testable import ccmux

/// Pins the harness bar's decision logic, which lives as pure functions on
/// PaneShellState: which panes get offered a bar, what it suggests, and how
/// the per-pane state map evicts panes that vanished. The daemon's own guard
/// (409 on a busy pane) backstops a wrong bar, but a wrong bar is still a
/// wrong UI.
final class HarnessOfferTests: XCTestCase {
    private typealias Shell = RemoteSessionService.PaneShellState

    private let ws = UUID()
    private let otherWs = UUID()

    private func harnessList(_ names: [String]) throws -> [DaemonHarness] {
        let json = "[" + names.map { #"{"name":"\#($0)","command":"\#($0)"}"# }.joined(separator: ",") + "]"
        return try JSONDecoder().decode([DaemonHarness].self, from: Data(json.utf8))
    }

    private func shell(
        harness: String = "", atShell: Bool = true, dormant: Bool = false, devServer: Bool = false
    ) -> Shell {
        Shell(workspace: ws, harness: harness, atShell: atShell, dormant: dormant, devServer: devServer)
    }

    // MARK: offer gating

    func testFreshShellSuggestsFolderRulePreselect() throws {
        let offer = shell().offer(harnesses: try harnessList(["claude", "pi"]), resolvedHarness: "pi")
        XCTAssertEqual(offer?.suggested, "pi")
        XCTAssertEqual(offer?.restart, false)
        XCTAssertEqual(offer?.harnesses.map(\.name), ["claude", "pi"])
    }

    func testFreshShellWithoutPreselectSuggestsClaude() throws {
        let offer = shell().offer(harnesses: try harnessList(["claude"]), resolvedHarness: nil)
        XCTAssertEqual(offer?.suggested, "claude")
    }

    func testDormantPaneSuggestsItsOwnHarnessAsRestart() throws {
        // A dormant pane's claude exited; the bar restarts what it ran, not
        // what the folder rule would pick for a fresh shell.
        let offer = shell(harness: "claude", atShell: false, dormant: true)
            .offer(harnesses: try harnessList(["claude", "pi"]), resolvedHarness: "pi")
        XCTAssertEqual(offer?.suggested, "claude")
        XCTAssertEqual(offer?.restart, true)
    }

    func testDevServerPaneNeverGetsABar() throws {
        // The dev-server pane's shell coming back means the dev server
        // exited, not that it wants a harness.
        let offer = shell(devServer: true).offer(harnesses: try harnessList(["claude"]), resolvedHarness: nil)
        XCTAssertNil(offer)
    }

    func testRunningPaneGetsNoBar() throws {
        let offer = shell(harness: "claude", atShell: false)
            .offer(harnesses: try harnessList(["claude"]), resolvedHarness: nil)
        XCTAssertNil(offer)
    }

    func testEmptyHarnessListMeansNoBar() throws {
        XCTAssertNil(shell().offer(harnesses: [], resolvedHarness: "claude"))
    }

    // MARK: merging / eviction

    private func panes(_ rows: [(id: String, atShell: Bool)]) throws -> [DaemonPane] {
        let json = "[" + rows.map { #"{"id":"\#($0.id)","atShell":\#($0.atShell)}"# }.joined(separator: ",") + "]"
        return try JSONDecoder().decode([DaemonPane].self, from: Data(json.utf8))
    }

    func testMergingEvictsOnlyThisWorkspacesVanishedPanes() throws {
        let current: [String: Shell] = [
            "gone": shell(),
            "stays": shell(atShell: false),
            "elsewhere": Shell(workspace: otherWs, harness: "", atShell: true, dormant: false, devServer: false),
        ]
        let next = Shell.merging(current, panes: try panes([(id: "stays", atShell: true)]), workspace: ws)
        XCTAssertNil(next["gone"], "a pane missing from its workspace's list must drop out")
        XCTAssertEqual(next["stays"]?.atShell, true, "a surviving pane is refreshed from the daemon")
        XCTAssertNotNil(next["elsewhere"], "another workspace's panes pass through untouched")
    }

    func testMergingAddsNewPanesUnderTheirWorkspace() throws {
        let next = Shell.merging([:], panes: try panes([(id: "p1", atShell: true)]), workspace: ws)
        XCTAssertEqual(next["p1"]?.workspace, ws)
        XCTAssertEqual(next["p1"]?.atShell, true)
    }
}

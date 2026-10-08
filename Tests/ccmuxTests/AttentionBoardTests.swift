import XCTest
@testable import ccmux

/// The attention board's rules. The web lens's smoke_test.js checks the same
/// table against board.js; keep the two saying the same thing.
final class AttentionBoardTests: XCTestCase {
    private func claim(_ pane: String, _ state: DaemonAttention, _ reason: String, _ since: Int64) -> BoardClaim {
        BoardClaim(pane: pane, state: state, reason: reason, since: since,
                   name: pane, chat: false, workingDirectory: "/")
    }

    private lazy var turn = claim("turn", .done, "finished", 100)
    private lazy var perm = claim("perm", .needsInput, "permission", 300)
    private lazy var old = claim("old", .needsInput, "", 0)
    private lazy var nudge = claim("nudge", .needsInput, "finished", 50)

    func testBlockedFirstThenTheLongestWait() {
        let order = [turn, perm, old, nudge].sorted(by: AttentionBoard.ordered).map(\.pane)
        XCTAssertEqual(order, ["old", "perm", "nudge", "turn"])
        XCTAssertTrue(old.isBlocked, "a needs_input with no reason is blocked")
        XCTAssertFalse(nudge.isBlocked, "Claude's idle reminder is your turn")
    }

    func testCapByWidth() {
        XCTAssertEqual(AttentionBoard.cap(forWidth: 390), 1)
        XCTAssertEqual(AttentionBoard.cap(forWidth: 1100), 4)
        XCTAssertEqual(AttentionBoard.cap(forWidth: 1600), 6)
    }

    func testTheCapSplitsTilesFromTheStrip() {
        let out = AttentionBoard.layout([turn, perm, old], cap: 2, active: nil, previous: [], snoozes: [:], now: Date())
        XCTAssertEqual(out.visible.map(\.pane), ["old", "perm"])
        XCTAssertEqual(out.waiting.map(\.pane), ["turn"])
    }

    func testAnActiveTileFreezesTheSet() {
        let free = AttentionBoard.layout([turn, perm, old], cap: 2, active: nil, previous: [], snoozes: [:], now: Date())
        let arrival = claim("new", .needsInput, "question", 5)
        let frozen = AttentionBoard.layout([turn, perm, arrival], cap: 2, active: "perm",
                                           previous: free.visible, snoozes: [:], now: Date())
        XCTAssertEqual(frozen.visible.map { $0.pane + ($0.handled ? "!" : "") }, ["old!", "perm"])
        XCTAssertEqual(frozen.waiting.map(\.pane), ["new", "turn"])
    }

    func testNotNowHoldsUntilTheClaimChangesOrTimeRunsOut() {
        let now = Date()
        let snoozes = ["perm": BoardSnooze(since: 300, until: now.addingTimeInterval(60))]
        let later = AttentionBoard.layout([perm], cap: 4, active: nil, previous: [], snoozes: snoozes, now: now)
        let renewed = AttentionBoard.layout([claim("perm", .needsInput, "permission", 400)], cap: 4, active: nil,
                                            previous: [], snoozes: snoozes, now: now)
        let expired = AttentionBoard.layout([perm], cap: 4, active: nil, previous: [], snoozes: snoozes,
                                            now: now.addingTimeInterval(120))
        XCTAssertTrue(later.visible.isEmpty)
        XCTAssertEqual(renewed.visible.count, 1)
        XCTAssertEqual(expired.visible.count, 1)
    }

    func testAScreenTileHoldsItsPaneAtTheBoardsTextSize() {
        let cell = BoardCell(width: 7.2, height: 14.4)
        XCTAssertEqual(AttentionBoard.grid(width: 634, height: 350, cell: cell), BoardGrid(cols: 88, rows: 24))
        XCTAssertNil(AttentionBoard.grid(width: 140, height: 350, cell: cell), "too narrow to drive a pane to")
        XCTAssertEqual(AttentionBoard.font(width: 634, height: 350, cols: 88, rows: 24, cell: cell), 12)
        XCTAssertEqual(AttentionBoard.font(width: 634, height: 350, cols: 176, rows: 24, cell: cell), 6.0, accuracy: 0.01,
                       "a pane another lens made wider shrinks to fit across")
        XCTAssertEqual(AttentionBoard.font(width: 634, height: 350, cols: 88, rows: 48, cell: cell), 6.08, accuracy: 0.01,
                       "a taller one shrinks so its bottom line shows")
        XCTAssertEqual(AttentionBoard.font(width: 100, height: 100, cols: 400, rows: 100, cell: cell), 4)
    }
}

/// The board's tile switching, driven through the model with its daemon
/// swapped out. The web lens's activate takes the same steps in the same order.
@MainActor
final class AttentionBoardModelTests: XCTestCase {
    private func claim(_ pane: String, _ since: Int64) -> BoardClaim {
        BoardClaim(pane: pane, state: .needsInput, reason: "permission", since: since,
                   name: pane, chat: false, workingDirectory: "/")
    }

    func testSwitchingTilesKeepsTheGridStill() {
        var claims = [claim("a", 10), claim("b", 11), claim("c", 12), claim("d", 13)]
        var acted: [String] = []
        let model = AttentionBoardModel()
        model.width = 1100 // four tiles
        model.claimsFor = { _ in claims }
        model.actedSink = { pane in
            acted.append(pane)
            claims.removeAll { $0.pane == pane } // the daemon retires it
        }
        model.refresh()
        model.activate("a")

        // An older blocked claim arrives while a tile is active: it waits.
        claims.append(claim("e", 1))
        model.refresh()
        XCTAssertEqual(model.visible.map(\.pane), ["a", "b", "c", "d"])
        XCTAssertEqual(model.waiting.map(\.pane), ["e"])

        // Switching tiles moves on from the old one without re-sorting.
        model.activate("d")
        model.refresh()
        XCTAssertEqual(acted, ["a"])
        XCTAssertEqual(model.active, "d")
        XCTAssertEqual(model.visible.map { $0.pane + ($0.handled ? "!" : "") }, ["a!", "b", "c", "d"])

        // A handled tile can still be opened.
        model.activate("a")
        XCTAssertEqual(model.active, "a")
        XCTAssertEqual(acted, ["a", "d"])
    }

    /// Not now on the tile you are in is not dealing with it: nothing is sent
    /// and the claim stays. Moving on from the next one does send. (Pane names
    /// are this test's own: BoardSnoozes is app-wide.)
    func testNotNowOnTheActiveTileSendsNothing() {
        let claims = [claim("snz-1", 10), claim("snz-2", 11)]
        var acted: [String] = []
        let model = AttentionBoardModel()
        model.claimsFor = { _ in claims }
        model.actedSink = { acted.append($0) }
        model.refresh()

        model.activate("snz-1")
        model.snooze(claims[0])
        XCTAssertNil(model.active)
        XCTAssertEqual(acted, [])
        XCTAssertEqual(model.visible.map(\.pane), ["snz-2"], "the snoozed claim is hidden, not retired")

        model.activate("snz-2")
        model.next()
        XCTAssertEqual(acted, ["snz-2"])
    }

    /// A screen tile sizes its pane once per tile size, not on every paint,
    /// and a chat tile has no screen to size. Same steps as the web lens's.
    func testATileSizesItsPaneOncePerSize() {
        let chat = BoardClaim(pane: "sz-chat", state: .needsInput, reason: "permission", since: 11,
                              name: "sz-chat", chat: true, workingDirectory: "/")
        var sent: [String] = []
        let model = AttentionBoardModel()
        model.claimsFor = { _ in [self.claim("sz-1", 10), chat] }
        model.cell = BoardCell(width: 7.2, height: 14.4)
        model.resizeSink = { pane, grid, _ in sent.append("\(pane) \(grid.cols)x\(grid.rows)") }
        model.refresh()

        model.tileArea("sz-1", width: 634, height: 350)
        model.tileArea("sz-1", width: 634, height: 350) // the next paint, same tile
        model.tileArea("sz-chat", width: 634, height: 350)
        model.tileArea("sz-1", width: 900, height: 350) // the tile grew
        XCTAssertEqual(sent, ["sz-1 88x24", "sz-1 125x24"])
    }

    /// A size that did not take is sent again on the next tick, with no new
    /// area from the tile. The web lens's smoke test checks the same.
    func testAFailedSizeIsSentAgainOnTheNextTick() async throws {
        var sent = 0
        let model = AttentionBoardModel()
        model.claimsFor = { _ in [self.claim("sz-2", 10)] }
        model.cell = BoardCell(width: 7.2, height: 14.4)
        model.resizeSink = { _, _, done in
            sent += 1
            done(false)
        }
        model.refresh()

        model.tileArea("sz-2", width: 634, height: 350)
        try await Task.sleep(nanoseconds: 100_000_000) // the failure lands
        model.retrySizing()
        XCTAssertEqual(sent, 2)
    }
}

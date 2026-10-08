import Foundation

/// One pane waiting on a human, as an attention-board tile draws it: the
/// firehose's claim (PaneClaimStore) plus the workspace's name and whether
/// the pane serves a chat.
struct BoardClaim: Equatable, Identifiable {
    let pane: String
    let state: DaemonAttention
    let reason: String
    let since: Int64
    let name: String
    /// An agent pane whose harness serves a chat (AgentPaneRef.hasChat).
    let chat: Bool
    let workingDirectory: String
    /// Dealt with elsewhere while the board was frozen; drawn dimmed until
    /// the person moves on.
    var handled = false

    var id: String { pane }

    /// A permission or a question, as against "your turn". A needs_input
    /// with no reason comes from an agent too old to say, and every such
    /// signal was one of the two.
    var isBlocked: Bool { state == .needsInput && reason != "finished" }

    var reasonLabel: String {
        if handled { return "handled" }
        switch reason {
        case "permission": return "wants permission"
        case "question": return "asked a question"
        default: return isBlocked ? "needs you" : "your turn"
        }
    }
}

/// "Not now" on one claim: hidden until `until`, or until the claim changes
/// (a new start time is a new claim and comes straight back).
struct BoardSnooze: Equatable {
    let since: Int64
    let until: Date
}

/// A pane size in characters: what a screen tile holds at the board's text size.
struct BoardGrid: Equatable {
    let cols: Int
    let rows: Int
}

/// One character's box at AttentionBoard.tileFont, measured by each lens in
/// the font it draws a screen tile with.
struct BoardCell: Equatable {
    let width: Double
    let height: Double
}

/// The attention board's rules: which claims get a tile, in what order, how
/// many, when the set holds still, and what size a screen tile's pane and
/// text are. The web lens's board.js holds the same
/// rules in the same words; its smoke_test.js and AttentionBoardTests check
/// the same table, so change both together.
enum AttentionBoard {
    static let snoozeFor: TimeInterval = 30 * 60

    /// Blocked first, then the longest wait (no recorded start counts as the
    /// oldest), then by name so equal claims hold still.
    static func ordered(_ a: BoardClaim, _ b: BoardClaim) -> Bool {
        if a.isBlocked != b.isBlocked { return a.isBlocked }
        if a.since != b.since { return a.since < b.since }
        return a.name.localizedCompare(b.name) == .orderedAscending
    }

    /// How many tiles a board this wide shows: one on a phone-narrow window,
    /// four on a laptop, six on a big screen. The rest wait in the strip.
    static func cap(forWidth width: Double) -> Int {
        width < 700 ? 1 : width < 1300 ? 4 : 6
    }

    static func columns(for count: Int) -> Int {
        count <= 1 ? 1 : count <= 4 ? 2 : 3
    }

    /// A screen tile draws its text at the size the terminal does
    /// (RemoteTermController: 12pt Monaco).
    static let tileFont = 12.0

    /// The pane size that fills a tile's text area at tileFont, or nil for a
    /// tile too small to drive a pane to: a crushed pane is worse than small
    /// text (the web lens's MIN_COLS).
    static func grid(width: Double, height: Double, cell: BoardCell) -> BoardGrid? {
        guard cell.width > 0, cell.height > 0 else { return nil }
        let cols = Int(width / cell.width), rows = Int(height / cell.height)
        return cols >= 20 && rows >= 5 ? BoardGrid(cols: cols, rows: rows) : nil
    }

    /// The text size that shows a cols x rows screen whole in a tile: tileFont
    /// when the pane is the tile's size, smaller when another lens has it
    /// bigger, never below 4.
    static func font(width: Double, height: Double, cols: Int, rows: Int, cell: BoardCell) -> Double {
        guard cols > 0, cell.width > 0, cell.height > 0 else { return tileFont }
        let across = width / (Double(cols) * cell.width)
        let down = rows > 0 ? height / (Double(rows) * cell.height) : 1
        return max(4, tileFont * min(1, across, down))
    }

    static func isSnoozed(_ c: BoardClaim, in snoozes: [String: BoardSnooze], now: Date) -> Bool {
        guard let s = snoozes[c.pane] else { return false }
        return s.since == c.since && s.until > now
    }

    /// Picks the tiles. While a tile is active the set is frozen, so nothing
    /// moves under the person typing: arrivals wait in the strip, and a tile
    /// whose claim was dealt with elsewhere stays, marked handled, until they
    /// move on.
    static func layout(_ claims: [BoardClaim], cap: Int, active: String?, previous: [BoardClaim],
                       snoozes: [String: BoardSnooze], now: Date) -> (visible: [BoardClaim], waiting: [BoardClaim]) {
        let live = claims.filter { !isSnoozed($0, in: snoozes, now: now) }.sorted(by: ordered)
        guard let active, previous.contains(where: { $0.pane == active }) else {
            return (Array(live.prefix(cap)), Array(live.dropFirst(cap)))
        }
        let byPane = Dictionary(live.map { ($0.pane, $0) }, uniquingKeysWith: { first, _ in first })
        let shown = Set(previous.map(\.pane))
        let visible = previous.map { old -> BoardClaim in
            if let fresh = byPane[old.pane] { return fresh }
            var gone = old
            gone.handled = true
            return gone
        }
        return (visible, live.filter { !shown.contains($0.pane) })
    }

    static func waited(since: Int64, now: Date) -> String {
        guard since > 0 else { return "" }
        let minutes = Int((now.timeIntervalSince1970 * 1000 - Double(since)) / 60000)
        if minutes < 1 { return "just now" }
        return minutes < 60 ? "\(minutes) min" : "\(minutes / 60) h"
    }
}

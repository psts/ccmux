import XCTest
@testable import ccmux

/// The read-back after every hello merges history into the rows on screen by
/// the bus's message number. A replace lost a message that arrived live while
/// the read was in flight (the daemon reads history outside the lock its
/// broadcast holds), and the overlay keys its rows on this id, so a duplicate
/// would garble the list.
final class PeerMessageMergeTests: XCTestCase {
    private func msg(_ id: Int, _ text: String = "") -> PeerMessage {
        PeerMessage(id: id, from_id: "a", to_id: "b", from_name: "a", to_name: "b", text: text, sent_at: "")
    }

    private func ids(_ ms: [PeerMessage]) -> [Int] { ms.map(\.id) }

    /// A live row newer than the history read stays, after the history.
    func testKeepsALiveRowTheReadMissed() {
        let merged = PeerMessage.merged(history: [msg(1), msg(2)], onScreen: [msg(1), msg(3)])
        XCTAssertEqual(ids(merged), [1, 2, 3])
    }

    /// A row both on screen and in history is drawn once.
    func testNoDuplicates() {
        let merged = PeerMessage.merged(history: [msg(5), msg(6)], onScreen: [msg(5), msg(6)])
        XCTAssertEqual(ids(merged), [5, 6])
    }

    /// The history window is the last N messages; a row on screen older than
    /// it stays rather than vanishing on a reconnect.
    func testKeepsRowsOlderThanTheWindow() {
        let merged = PeerMessage.merged(history: [msg(10), msg(11)], onScreen: [msg(2), msg(10)])
        XCTAssertEqual(ids(merged), [2, 10, 11])
    }

    /// History's copy of a row wins, and order is by number whatever order
    /// the rows came in.
    func testHistoryWinsAndOrderIsByNumber() {
        let merged = PeerMessage.merged(history: [msg(3, "saved")], onScreen: [msg(4), msg(3, "live")])
        XCTAssertEqual(ids(merged), [3, 4])
        XCTAssertEqual(merged.first?.text, "saved")
    }

    /// A row with no number (a frame that carried none) is kept, at the end.
    func testUnnumberedRowsStayAtTheEnd() {
        let merged = PeerMessage.merged(history: [msg(1)], onScreen: [msg(-1, "loose"), msg(2)])
        XCTAssertEqual(ids(merged), [1, 2, -1])
    }

    func testEmptyHistoryKeepsTheScreen() {
        XCTAssertEqual(ids(PeerMessage.merged(history: [], onScreen: [msg(1), msg(2)])), [1, 2])
    }

    /// A read that came back full may not reach back to the screen: more may
    /// have been sent while disconnected than it covers. Older rows go rather
    /// than sit above an unmarked hole; newer live rows and overlap stay.
    func testFullReadDropsRowsOlderThanIt() {
        let merged = PeerMessage.merged(history: [msg(20), msg(21)], onScreen: [msg(3), msg(4), msg(21), msg(22)], limit: 2)
        XCTAssertEqual(ids(merged), [20, 21, 22])
    }

    /// A read short of the limit covers everything the window holds, so
    /// older rows on screen stay.
    func testShortReadKeepsOlderRows() {
        let merged = PeerMessage.merged(history: [msg(20)], onScreen: [msg(3), msg(20)], limit: 2)
        XCTAssertEqual(ids(merged), [3, 20])
    }

    // MARK: - Live rows

    private func frame(seq: Int?) -> PeerWSMessage {
        let seqField = seq.map { #""seq":\#($0),"# } ?? ""
        return PeerWSMessage.fromListenFrame(
            #"{"type":"message",\#(seqField)"from_id":"a","from_name":"a","to_id":"b","to_name":"b","text":"t","sent_at":"s"}"#)!
    }

    /// A live frame takes its bus number as the row id.
    func testLiveRowTakesItsNumber() {
        var next = -1
        let row = PeerMessage.liveRow(frame(seq: 9), onScreen: [msg(8)], nextLocalId: &next)
        XCTAssertEqual(row?.id, 9)
        XCTAssertEqual(next, -1, "a numbered frame uses no local id")
    }

    /// The race the read-back opens: history already brought message 9, then
    /// its live copy lands. Drawn once.
    func testLiveRowAlreadyOnScreenIsSkipped() {
        var next = -1
        XCTAssertNil(PeerMessage.liveRow(frame(seq: 9), onScreen: [msg(9)], nextLocalId: &next))
    }

    func testUnnumberedFrameGetsTheNextLocalId() {
        var next = -1
        XCTAssertEqual(PeerMessage.liveRow(frame(seq: nil), onScreen: [], nextLocalId: &next)?.id, -1)
        XCTAssertEqual(PeerMessage.liveRow(frame(seq: nil), onScreen: [], nextLocalId: &next)?.id, -2)
        XCTAssertEqual(next, -3)
    }
}

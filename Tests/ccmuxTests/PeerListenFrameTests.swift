import XCTest
@testable import ccmux

/// The bus's listen stream opens with a hello and then carries one frame per
/// message. Only a message frame yields something to draw; the frames are the
/// daemon's real shapes (daemon/internal/peers/conn.go, listenHello and
/// messageFrame).
final class PeerListenFrameTests: XCTestCase {
    func testHelloCarriesNoMessage() {
        XCTAssertNil(PeerWSMessage.fromListenFrame(#"{"type":"hello"}"#))
    }

    func testMessageFrameDecodes() throws {
        let m = try XCTUnwrap(PeerWSMessage.fromListenFrame(
            #"{"type":"message","seq":7,"from_id":"a1","from_name":"hq","to_id":"b2","to_name":"ccmux","text":"hi","sent_at":"2026-10-02T08:00:00.000Z"}"#))
        XCTAssertEqual(m.from_name, "hq")
        XCTAssertEqual(m.to_name, "ccmux")
        XCTAssertEqual(m.text, "hi")
        XCTAssertEqual(m.seq, 7, "the number a read-back merges on")
    }

    /// Any other kind is skipped, as the web lens skips it, so the daemon can
    /// add frame kinds without breaking an older app.
    func testUnknownKindIsSkipped() {
        XCTAssertNil(PeerWSMessage.fromListenFrame(#"{"type":"presence","from_id":"a1"}"#))
    }

    func testNonJSONIsSkipped() {
        XCTAssertNil(PeerWSMessage.fromListenFrame("not json"))
    }

    /// A message frame missing a field is dropped (and logged), not drawn half.
    func testMalformedMessageIsDropped() {
        XCTAssertNil(PeerWSMessage.fromListenFrame(#"{"type":"message","text":"no sender"}"#))
    }
}

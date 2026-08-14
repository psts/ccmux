import XCTest
@testable import ccmux

/// The Peer Messages overlay sat empty for three days behind the caveat
/// "Couldn't confirm which bus to read", while the daemon was answering 200 the
/// whole time. The decode was `[String: String]` and the body carries
/// `"partial": false` — a Bool — so every answer was thrown away and the failure
/// was logged as "answered HTTP 200", which reads like success.
///
/// These run against the real daemon payload rather than a hand-built one.
final class PeerViewerAnswerTests: XCTestCase {
    private func parse(_ json: String) -> PeerViewerAnswer? {
        try? PeerViewerAnswer.parse(Data(json.utf8)).get()
    }

    /// The shape a federated member host answers with. The token is synthetic on
    /// purpose: a real one is a live fleet-wide read credential, and this repo is
    /// public. Nothing here needs a real value — the assertion is that the field
    /// round-trips, not what it contains.
    func testParsesAFederatedAnswer() throws {
        let answer = try XCTUnwrap(parse(
            #"{"bus":"/v1/hubbus","partial":false,"token":"test-viewer-token"}"#))

        XCTAssertEqual(answer.bus, "/v1/hubbus")
        XCTAssertEqual(answer.token, "test-viewer-token")
        XCTAssertFalse(answer.partial)
    }

    /// The daemon's honest "I know of a hub but hold no key for it": it points the
    /// lens at the local registry and flags the view as incomplete.
    func testParsesThePartialAnswer() throws {
        let answer = try XCTUnwrap(parse(#"{"bus":"","partial":true,"token":""}"#))

        XCTAssertEqual(answer.bus, "")
        XCTAssertEqual(answer.token, "")
        XCTAssertTrue(answer.partial)
    }

    /// A daemon with no hub at all — the app reads its local bus, confidently.
    func testParsesTheStandaloneAnswer() throws {
        let answer = try XCTUnwrap(parse(#"{"bus":"","partial":false,"token":""}"#))

        XCTAssertEqual(answer.bus, "")
        XCTAssertFalse(answer.partial)
    }

    /// App and daemon ship separately, so a daemon older than `partial` has to
    /// keep working rather than failing the whole resolve over one absent flag.
    func testADaemonPredatingPartialStillResolves() throws {
        let answer = try XCTUnwrap(parse(#"{"bus":"/v1/hubbus","token":"abc"}"#))

        XCTAssertEqual(answer.bus, "/v1/hubbus")
        XCTAssertEqual(answer.token, "abc")
        XCTAssertFalse(answer.partial, "absent partial must read as 'not partial', not as a failure")
    }

    /// A field the app has never heard of must not sink the answer either — the
    /// mismatch that caused this bug ran in that direction too.
    func testAnUnknownFieldIsIgnored() throws {
        let answer = try XCTUnwrap(parse(
            #"{"bus":"/v1/hubbus","partial":false,"token":"abc","somethingNew":{"a":1}}"#))

        XCTAssertEqual(answer.bus, "/v1/hubbus")
    }

    /// Genuinely unreadable bodies still have to be distinguishable from an answer,
    /// so the caller can say so instead of drawing a confident empty panel.
    func testRubbishIsRejected() {
        XCTAssertNil(parse("not json at all"))
        XCTAssertNil(parse("[]"))
        XCTAssertNil(parse(""))
    }

    /// The failure carries the decoding error out rather than collapsing to nil.
    /// Which key and which type IS the diagnosis for a contract drift — throwing
    /// it away is what let the last one hide behind "answered HTTP 200".
    func testAFailureNamesWhatWentWrong() throws {
        let result = PeerViewerAnswer.parse(Data(#"{"bus":123}"#.utf8))
        guard case .failure(let error) = result else {
            return XCTFail("a body with a wrong-typed field decoded successfully")
        }
        XCTAssertTrue(error is DecodingError, "the decoding error should survive, got \(error)")
    }
}

import XCTest
@testable import ccmux

final class WebSocketPumpTests: XCTestCase {

    // MARK: - Backoff ladder

    /// The ladder doubles and then flattens. It is spelled out rather than
    /// recomputed, so a change to the formula has to be a deliberate edit here.
    func testBackoffDoublesThenCaps() {
        XCTAssertEqual(WebSocketPump.backoff(attempt: 1), 0.5)
        XCTAssertEqual(WebSocketPump.backoff(attempt: 2), 1.0)
        XCTAssertEqual(WebSocketPump.backoff(attempt: 3), 2.0)
        XCTAssertEqual(WebSocketPump.backoff(attempt: 4), 4.0)
        XCTAssertEqual(WebSocketPump.backoff(attempt: 5), 5.0, "capped")
        XCTAssertEqual(WebSocketPump.backoff(attempt: 50), 5.0, "still capped, never unbounded")
    }

    /// A long outage must not park a lens for minutes. The cap is the property
    /// that matters most here: the whole branch exists because a socket could stay
    /// dead indefinitely without anyone noticing.
    func testBackoffNeverExceedsFiveSeconds() {
        for attempt in 1...500 {
            XCTAssertLessThanOrEqual(WebSocketPump.backoff(attempt: attempt), 5.0,
                                     "attempt \(attempt) backed off past the cap")
        }
    }

    func testBackoffIsZeroBeforeAnyFailure() {
        XCTAssertEqual(WebSocketPump.backoff(attempt: 0), 0)
    }

    // MARK: - A URL that cannot be built

    /// A nil URL is a misconfigured origin, not a transient fault. The pump must
    /// say so and settle on `.closed`.
    ///
    /// It used to return silently: no state change, no log, no retry ever
    /// scheduled. Because both clients start their own state at `.connecting`, the
    /// UI showed a reconnect spinner that could never resolve, for the rest of the
    /// process, with nothing anywhere naming the cause.
    func testNilURLReportsClosedRatherThanHangingOnConnecting() {
        let pump = WebSocketPump(label: "test-nil-url") { nil }
        let reachedClosed = expectation(description: "pump reports .closed")
        pump.onState = { state in
            if state == .closed { reachedClosed.fulfill() }
        }

        pump.connect()

        wait(for: [reachedClosed], timeout: 2.0)
    }

    /// disconnect() on a pump that never connected must be safe and must not
    /// report anything but closed. Teardown runs this path whenever a workspace is
    /// removed before its socket came up.
    func testDisconnectBeforeConnectIsSafe() {
        let pump = WebSocketPump(label: "test-never-connected") { nil }
        var states: [DaemonConnectionState] = []
        pump.onState = { states.append($0) }

        pump.disconnect()
        pump.disconnect() // idempotent

        let settled = expectation(description: "queue drained")
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { settled.fulfill() }
        wait(for: [settled], timeout: 2.0)

        XCTAssertFalse(states.contains(.connected), "nothing ever connected")
    }

    /// forceReconnect on a disconnected pump must stay disconnected. The wake
    /// handler calls it across every attachment without checking, so a stopped
    /// service must not be resurrected by a wake.
    func testForceReconnectAfterDisconnectStaysClosed() {
        let pump = WebSocketPump(label: "test-force-after-close") { nil }
        var sawConnecting = false
        pump.onState = { if $0 == .connecting { sawConnecting = true } }

        pump.disconnect()
        pump.forceReconnect()

        let settled = expectation(description: "queue drained")
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { settled.fulfill() }
        wait(for: [settled], timeout: 2.0)

        XCTAssertFalse(sawConnecting, "a disconnected pump must not dial on wake")
    }

    /// Sending with no socket must not crash and must not block the caller.
    /// Keystrokes take this path during a reconnect.
    func testSendWithNoSocketIsSafe() {
        let pump = WebSocketPump(label: "test-send-no-socket") { nil }
        pump.send("{\"t\":\"focus\"}")

        let settled = expectation(description: "queue drained")
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { settled.fulfill() }
        wait(for: [settled], timeout: 2.0)
    }
}

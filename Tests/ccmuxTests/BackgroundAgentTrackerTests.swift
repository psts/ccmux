import XCTest
@testable import ccmux

/// The Swift twin of `daemon/internal/hooks/agents_test.go`. Both trackers exist
/// because hosted panes reach the daemon and local panes reach this app, so these
/// cases are kept deliberately parallel — a rule that holds in one and not the
/// other would make the two kinds of pane behave differently.
final class BackgroundAgentTrackerTests: XCTestCase {
    private var clock: Date!
    private var tracker: BackgroundAgentTracker!

    override func setUp() {
        clock = Date(timeIntervalSince1970: 1_785_000_000)
        tracker = BackgroundAgentTracker(now: { [unowned self] in self.clock })
    }

    func testCountsLiveAgentsPerSession() {
        _ = tracker.observe(event: "subagent_start", sessionId: "s1", agentId: "a1")
        _ = tracker.observe(event: "subagent_start", sessionId: "s1", agentId: "a2")
        _ = tracker.observe(event: "subagent_start", sessionId: "s2", agentId: "a3")

        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 2)
        XCTAssertEqual(tracker.outstanding(sessionId: "s2"), 1, "sessions must not pool their agents")

        _ = tracker.observe(event: "subagent_stop", sessionId: "s1", agentId: "a1")
        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 1)
        _ = tracker.observe(event: "subagent_stop", sessionId: "s1", agentId: "a2")
        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 0)
    }

    /// SubagentStop fires for agents SubagentStart never announced — 229 stops
    /// against 87 starts in the log this was built from. Counting those would drive
    /// the count negative and suppress a genuine finish.
    func testStopForUnknownAgentIsIgnored() {
        _ = tracker.observe(event: "subagent_start", sessionId: "s1", agentId: "a1")

        let outcome = tracker.observe(event: "subagent_stop", sessionId: "s1", agentId: "never-started")

        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 1, "an unknown stop must not decrement")
        guard case .tracked(let detail) = outcome else {
            return XCTFail("a subagent stop is always agent bookkeeping")
        }
        XCTAssertTrue(detail.contains("ignored"), "the trace should say the stop was ignored, got: \(detail)")
    }

    /// An agent that starts and never stops would otherwise silence its session's
    /// done alerts forever. A real leaked id was present in the log.
    func testLeakedAgentExpires() {
        _ = tracker.observe(event: "subagent_start", sessionId: "s1", agentId: "leaked")

        clock = clock.addingTimeInterval(BackgroundAgentTracker.agentTTL - 1)
        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 1, "just inside the TTL")

        clock = clock.addingTimeInterval(2)
        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 0, "past the TTL")
    }

    /// A restarted session cannot be waiting on the previous one's agents, and
    /// their stops will never arrive.
    func testSessionStartClearsButIsNotConsumed() {
        _ = tracker.observe(event: "subagent_start", sessionId: "s1", agentId: "a1")

        let outcome = tracker.observe(event: "session_start", sessionId: "s1", agentId: "")

        XCTAssertEqual(outcome, .notAnAgentEvent,
                       "session_start still means something to the caller; it must not be swallowed here")
        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 0)
    }

    func testUnattributableAgentsAreNotTracked() {
        _ = tracker.observe(event: "subagent_start", sessionId: "", agentId: "a1")
        _ = tracker.observe(event: "subagent_start", sessionId: "s1", agentId: "")

        XCTAssertEqual(tracker.outstanding(sessionId: "s1"), 0)
        XCTAssertEqual(tracker.outstanding(sessionId: ""), 0)
    }

    func testUnrelatedEventsPassThrough() {
        for event in ["stop", "notification", "user_prompt_submit", "permission_request"] {
            XCTAssertEqual(tracker.observe(event: event, sessionId: "s1", agentId: ""), .notAnAgentEvent,
                           "\(event) is not agent bookkeeping")
        }
    }
}

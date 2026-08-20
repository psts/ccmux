import XCTest
@testable import ccmux

/// The reason this exists: two Explore agents started, the main loop went quiet,
/// and 60 seconds later Claude Code's idle reminder claimed the workspace was
/// waiting for a human. It was waiting for the agents.
final class SubagentTrackerTests: XCTestCase {

    func testAgentsAreCountedPerSession() {
        let t = SubagentTracker()
        XCTAssertEqual(t.start(session: "s1", agent: "a1"), 1)
        XCTAssertEqual(t.start(session: "s1", agent: "a2"), 2)
        XCTAssertEqual(t.start(session: "s2", agent: "a3"), 1)
        XCTAssertEqual(t.busy(session: "s1"), 2)
        XCTAssertEqual(t.busy(session: "s2"), 1)
    }

    func testStoppingTheLastAgentEndsTheHold() {
        let t = SubagentTracker()
        t.start(session: "s1", agent: "a1")
        let result = t.stop(session: "s1", agent: "a1")
        XCTAssertTrue(result.known)
        XCTAssertEqual(result.left, 0)
        XCTAssertEqual(t.busy(session: "s1"), 0)
    }

    /// A subagent emits a SubagentStop at every turn of its own inner loop, under
    /// an id that never started — 1793 of them against 548 real starts in one
    /// trace. If those decremented anything, one long agent would unmute itself.
    func testInnerLoopStopDoesNotFreeTheAgent() {
        let t = SubagentTracker()
        t.start(session: "s1", agent: "real")
        for id in ["turn-1", "turn-2", "turn-3"] {
            let result = t.stop(session: "s1", agent: id)
            XCTAssertFalse(result.known, "\(id) was never started and must not count as a stop")
        }
        XCTAssertEqual(t.busy(session: "s1"), 1)
    }

    /// An agent whose stop is lost stops counting after the timeout, so the worst
    /// case degrades to the old noisy behaviour instead of permanent silence.
    func testLostStopExpires() {
        let t = SubagentTracker()
        let longAgo = Date().addingTimeInterval(-SubagentTracker.agentTimeout - 60)
        t.start(session: "s1", agent: "a1", now: longAgo)
        XCTAssertEqual(t.busy(session: "s1"), 0)
    }

    /// Agents belong to a session. Without a session id there is nothing to
    /// attribute them to, and a shared empty key would let one anonymous session
    /// mute another.
    func testAgentsWithoutASessionAreIgnored() {
        let t = SubagentTracker()
        XCTAssertEqual(t.start(session: "", agent: "a1"), 0)
        XCTAssertEqual(t.start(session: "s1", agent: ""), 0)
        XCTAssertEqual(t.busy(session: ""), 0)
    }

    func testClearForgetsASessionsAgents() {
        let t = SubagentTracker()
        t.start(session: "s1", agent: "a1")
        t.clear(session: "s1")
        XCTAssertEqual(t.busy(session: "s1"), 0)
    }

    func testDescribeReadsAsACount() {
        XCTAssertEqual(SubagentTracker.describe(1), "1 subagent running")
        XCTAssertEqual(SubagentTracker.describe(2), "2 subagents running")
    }
}

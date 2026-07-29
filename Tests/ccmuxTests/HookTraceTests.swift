import XCTest
@testable import ccmux

/// These redirect `HookTrace` at a scratch file via `CCMUX_HOOK_TRACE`, the same
/// variable the daemon and the hook script honour. Without that, the suite would
/// append invented decisions to `~/Library/Logs/ccmux-hooks.jsonl` — the file a
/// developer reads to find out what really happened.
final class HookTraceTests: XCTestCase {
    private var scratch: String!

    override func setUpWithError() throws {
        scratch = NSTemporaryDirectory() + "ccmux-hooktrace-\(UUID().uuidString).jsonl"
        setenv("CCMUX_HOOK_TRACE", scratch, 1)
    }

    override func tearDownWithError() throws {
        unsetenv("CCMUX_HOOK_TRACE")
        try? FileManager.default.removeItem(atPath: scratch)
    }

    /// Every line the app writes has to parse as one JSON object with the keys the
    /// shared format is built on — the tail viewer keys off `stage` and `decision`,
    /// and the daemon writes the same shape from Go.
    func testWrittenLineIsOneJSONObjectWithTheSharedKeys() throws {
        HookTrace.write(decision: "posted", fields: [
            "event": "stop",
            "trace_id": "abc123",
            "workspace_id": "WS-1",
        ])

        let obj = try lastObject()
        XCTAssertEqual(obj["stage"], "local", "the app's lines must be tagged so a reader can tell them from the daemon's")
        XCTAssertEqual(obj["decision"], "posted")
        XCTAssertEqual(obj["event"], "stop")
        XCTAssertEqual(obj["trace_id"], "abc123", "the hook script's id has to survive into the app's line")
        XCTAssertNotNil(obj["ts"], "correlation falls back to timestamps wherever the trace id doesn't reach")
    }

    /// The app's lines share a file with the daemon's and the hook script's, both
    /// of which stamp local time with an offset. `ISO8601DateFormatter` defaults to
    /// GMT, which silently shifts the app's lines away from every other writer's
    /// and breaks the timestamp correlation that push lines depend on.
    func testTimestampIsLocalTimeNotGMT() throws {
        HookTrace.write(decision: "posted", fields: ["event": "tz-check"])

        let ts = try XCTUnwrap(lastObject()["ts"])
        let expected = offsetSuffix(for: TimeZone.current)
        XCTAssertTrue(ts.hasSuffix(expected),
                      "timestamp \(ts) does not end in this machine's UTC offset \(expected); the app is stamping a different zone from the daemon")
    }

    /// Several writers append concurrently. A torn line makes the log unparseable
    /// exactly when it matters, so each write has to land whole.
    func testConcurrentWritesStayWhole() throws {
        let group = DispatchGroup()
        for i in 0..<40 {
            group.enter()
            DispatchQueue.global().async {
                HookTrace.write(decision: "posted", fields: ["event": "e\(i)", "detail": String(repeating: "x", count: 200)])
                group.leave()
            }
        }
        XCTAssertEqual(group.wait(timeout: .now() + 5), .success)
        settle()

        let lines = try String(contentsOfFile: scratch, encoding: .utf8)
            .split(separator: "\n").map(String.init)
        XCTAssertEqual(lines.count, 40)
        for line in lines {
            XCTAssertNoThrow(try JSONSerialization.jsonObject(with: Data(line.utf8)), "torn line: \(line)")
        }
    }

    /// The path is the one contract the three writers share. If this drifts from
    /// `internal/hooktrace.defaultPath` and the shell script's default, the log
    /// silently splits into separate files.
    func testDefaultPathIsTheSharedLogLocation() {
        unsetenv("CCMUX_HOOK_TRACE")
        XCTAssertTrue(HookTrace.path.hasSuffix("Library/Logs/ccmux-hooks.jsonl"),
                      "path is \(HookTrace.path); the daemon and hooks/ccmux-notify.sh both default here")
    }

    /// An override that only applies if it was set before the first line was
    /// written would be a trap for anyone redirecting a running app.
    func testEnvOverrideIsHonouredPerWrite() {
        XCTAssertEqual(HookTrace.path, scratch)
    }

    // MARK: - Helpers

    /// The writer is asynchronous on its own queue; give it a moment to land.
    private func settle() {
        let landed = expectation(description: "trace line written")
        DispatchQueue.global().asyncAfter(deadline: .now() + 0.3) { landed.fulfill() }
        wait(for: [landed], timeout: 2)
    }

    private func lastObject() throws -> [String: String] {
        settle()
        let text = try String(contentsOfFile: scratch, encoding: .utf8)
        let line = try XCTUnwrap(text.split(separator: "\n").last.map(String.init),
                                 "nothing appended to \(scratch!)")
        return try XCTUnwrap(JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: String],
                             "trace line is not a flat JSON object: \(line)")
    }

    /// RFC 3339 renders UTC itself as "Z" and everything else as ±HH:MM.
    private func offsetSuffix(for zone: TimeZone) -> String {
        let seconds = zone.secondsFromGMT(for: Date())
        if seconds == 0 { return "Z" }
        let sign = seconds < 0 ? "-" : "+"
        let total = abs(seconds) / 60
        return String(format: "%@%02d:%02d", sign, total / 60, total % 60)
    }
}

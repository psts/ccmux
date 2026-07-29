import Foundation

/// Appends the native app's notification decisions to the shared hook trace at
/// `~/Library/Logs/ccmux-hooks.jsonl`.
///
/// The same file is written by `hooks/ccmux-notify.sh` (every hook Claude Code
/// fires) and by the daemon (`internal/hooktrace`), so one `tail -f` shows a hook
/// arriving, where it routed, and whether it became a notification. This type is
/// the app's third of that: it records the local macOS alert — posted, or
/// suppressed because you were already looking at the workspace.
///
/// Writing is best-effort in every direction. A trace that fails must never change
/// what the app shows, so there is no error to handle and nothing to report.
enum HookTrace {
    /// Overridable via `CCMUX_HOOK_TRACE`, the same variable the daemon and the
    /// hook script read, so pointing all three at one scratch file is one export.
    ///
    /// Read on each write rather than resolved once: an override that only takes
    /// effect if it was set before the first trace line is a trap, and tests need
    /// to redirect this away from the real log they'd otherwise pollute.
    static var path: String {
        if let override = ProcessInfo.processInfo.environment["CCMUX_HOOK_TRACE"], !override.isEmpty {
            return override
        }
        let home = FileManager.default.homeDirectoryForCurrentUser
        return home.appendingPathComponent("Library/Logs/ccmux-hooks.jsonl").path
    }

    /// Matches `internal/hooktrace.maxBytes`. Past it the file restarts: this is a
    /// tail buffer for a debugging session, not history worth keeping.
    private static let maxBytes: UInt64 = 8 << 20

    private static let queue = DispatchQueue(label: "com.ccmux.hooktrace")

    /// Local time with an offset, matching what the daemon (Go RFC 3339) and the
    /// hook script (Python `astimezone().isoformat()`) write. `ISO8601DateFormatter`
    /// defaults to GMT, which would put the app's lines an hour or more away from
    /// everyone else's in the same file — and timestamps are the only way to
    /// correlate a push line, which carries no trace id.
    private static let timestamp: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        f.timeZone = TimeZone.current
        return f
    }()

    /// Record one decision. `decision` is the one-word outcome the log is read
    /// for; `fields` carries whatever else that branch knows.
    static func write(decision: String, fields: [String: String] = [:]) {
        var line: [String: String] = [
            "ts": timestamp.string(from: Date()),
            "stage": "local",
            "decision": decision,
        ]
        line.merge(fields) { _, new in new }

        queue.async { append(line) }
    }

    private static func append(_ line: [String: String]) {
        guard let data = try? JSONSerialization.data(withJSONObject: line, options: [.sortedKeys]) else { return }
        var payload = data
        payload.append(0x0A) // newline

        let fd = open(path, O_WRONLY | O_APPEND | O_CREAT, 0o644)
        guard fd >= 0 else { return }
        defer { close(fd) }

        var st = stat()
        if fstat(fd, &st) == 0, UInt64(st.st_size) > maxBytes {
            _ = ftruncate(fd, 0)
        }
        // One write() per line. O_APPEND makes it atomic against the daemon and
        // the hook script appending to the same file at the same moment.
        _ = payload.withUnsafeBytes { buf in
            Darwin.write(fd, buf.baseAddress, buf.count)
        }
    }
}

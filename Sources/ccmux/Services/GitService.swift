import Foundation

enum GitService {
    /// Coordinates the two async signals of a `run`: stdout EOF (from the read source)
    /// and process exit (from `terminationHandler`). The continuation must resume exactly
    /// once, and only when *both* are in — EOF guarantees stdout is fully drained, exit
    /// gives the status. Whichever signal lands second produces the resume payload;
    /// `resumed` gates against a double resume. All state is lock-guarded since the two
    /// signals fire on different queues.
    private final class RunState: @unchecked Sendable {
        private var data = Data()
        private var sawEOF = false
        private var exitCode: Int32?
        private var resumed = false
        private let lock = NSLock()

        func append(_ chunk: Data) {
            lock.lock(); defer { lock.unlock() }
            data.append(chunk)
        }

        /// Resume payload if both signals are in and we haven't resumed yet; else nil.
        private func readyLocked() -> (stdout: String, exitCode: Int32)? {
            guard sawEOF, let code = exitCode, !resumed else { return nil }
            resumed = true
            return (String(data: data, encoding: .utf8) ?? "", code)
        }

        func markEOF() -> (stdout: String, exitCode: Int32)? {
            lock.lock(); defer { lock.unlock() }
            sawEOF = true
            return readyLocked()
        }

        func markExit(_ code: Int32) -> (stdout: String, exitCode: Int32)? {
            lock.lock(); defer { lock.unlock() }
            exitCode = code
            return readyLocked()
        }

        /// Claim the one-and-only resume for the exec-failure path; nil if already taken.
        func claimFailure() -> Bool {
            lock.lock(); defer { lock.unlock() }
            guard !resumed else { return false }
            resumed = true
            return true
        }
    }

    /// Run a git command, returning its stdout and exit code.
    ///
    /// Drains the child's stdout with a `DispatchSource` read source over the pipe's
    /// raw file descriptor, reading via POSIX `read()` — instead of `waitUntilExit()`
    /// + `readDataToEndOfFile()`. The blocking variants would park one GCD worker
    /// thread per inflight git invocation; with many workspaces this can saturate the
    /// 64-thread soft limit and stall pane I/O across the app.
    ///
    /// We deliberately avoid `NSFileHandle.readabilityHandler` + `availableData`: that
    /// API *raises an ObjC `NSException`* (EBADF "Bad file descriptor") when a pipe read
    /// fails, which under concurrent fd churn happens intermittently. The handler runs
    /// on a dispatch queue where Swift `do/catch` can't catch ObjC exceptions, so it
    /// aborts the process. POSIX `read()` instead returns `-1`/`errno` on error and `0`
    /// at EOF, so the same conditions are handled inline and never crash.
    ///
    /// Two async signals drive completion, joined by `RunState` (resume only when both
    /// are in): the read source appends bytes as the child writes and, on `read()` == 0
    /// (EOF) or < 0 (error), cancels itself and marks EOF (stdout fully drained); the
    /// non-blocking `terminationHandler` supplies the exit code. We must NOT call the
    /// blocking `waitUntilExit()` here — doing so on a GCD worker parks that thread, and
    /// under many concurrent invocations it starves the dispatch pool (Foundation's own
    /// child-reaper source can't get a thread to deliver exits), deadlocking. Reading
    /// only in the source (never in `terminationHandler`) also avoids the partial-output
    /// race between the two queues.
    ///
    /// `exitCode == -1` is a sentinel for "couldn't exec git at all" (the
    /// `process.run()` throw path), which callers treat as a transient failure.
    static func run(args: [String], in directory: String) async -> (stdout: String, exitCode: Int32) {
        await withCheckedContinuation { continuation in
            let process = Process()
            process.executableURL = URL(fileURLWithPath: "/usr/bin/git")
            process.arguments = args
            process.currentDirectoryURL = URL(fileURLWithPath: directory)

            let pipe = Pipe()
            process.standardOutput = pipe
            process.standardError = FileHandle.nullDevice

            // Keep the read FileHandle captured so the fd stays valid for the source's
            // lifetime; the Pipe closes it on dealloc — we never close it ourselves.
            let readHandle = pipe.fileHandleForReading
            let readFD = readHandle.fileDescriptor
            let state = RunState()
            let queue = DispatchQueue(label: "com.ccmux.git-read")
            let source = DispatchSource.makeReadSource(fileDescriptor: readFD, queue: queue)

            source.setEventHandler {
                var buffer = [UInt8](repeating: 0, count: 64 * 1024)
                let n = buffer.withUnsafeMutableBytes { read(readFD, $0.baseAddress, $0.count) }
                if n > 0 {
                    state.append(Data(buffer[0..<n]))
                } else {
                    // n == 0 → EOF; n < 0 → read error. Either way, stop draining.
                    source.cancel()
                }
            }

            source.setCancelHandler {
                _ = readHandle  // retain the fd owner until the source is fully torn down
                if let result = state.markEOF() { continuation.resume(returning: result) }
            }

            process.terminationHandler = { proc in
                if let result = state.markExit(proc.terminationStatus) {
                    continuation.resume(returning: result)
                }
            }

            do {
                try process.run()
                source.resume()
            } catch {
                // Couldn't exec git. Claim the single resume with the -1 sentinel; the
                // terminationHandler won't fire (never launched). The source starts
                // suspended, and releasing a suspended source traps in libdispatch — so
                // resume() before cancel() to let it tear down cleanly (its markEOF()
                // then no-ops, since claimFailure() already took the resume).
                if state.claimFailure() {
                    continuation.resume(returning: ("", -1))
                }
                source.resume()
                source.cancel()
            }
        }
    }

    static func diff(repoPath: String, target: DiffTarget) async -> String {
        var args: [String]
        switch target {
        case .staged:
            args = ["diff", "--cached"]
        case .unstaged:
            args = ["diff"]
        case .commit(let sha):
            args = ["diff", "\(sha)^", sha]
        case .range(let from, let to):
            args = ["diff", from, to]
        }
        return await run(args: args, in: repoPath).stdout
    }

    // MARK: - Full Status (for sidebar dashboard)

    /// Fetch complete git status for a workspace in a single `git status` call.
    ///
    /// Repo-ness comes from the exit code, not a separate probe:
    /// - exit 128 → genuinely not a git repo → cleared `GitStatusInfo()`.
    /// - exit -1 (couldn't exec git) → `nil`, a transient failure the caller treats
    ///   as "keep previous status".
    /// - exit 0 → parse the porcelain v2 output.
    ///
    /// `--porcelain=v2 --branch` yields branch, upstream, ahead/behind, and every
    /// file change in one invocation. The only thing it can't give is ahead/behind
    /// vs the *default* branch, so that needs one extra `rev-list` — but only when
    /// we're not already on the default branch. `cachedDefaultBranch` lets the
    /// caller detect the default once and skip re-detecting it on every refresh.
    static func fullStatus(path: String, cachedDefaultBranch: String?) async -> GitStatusInfo? {
        let result = await run(args: ["status", "--porcelain=v2", "--branch"], in: path)
        if result.exitCode == 128 { return GitStatusInfo() }
        if result.exitCode != 0 { return nil }

        var info = parseStatusV2(result.stdout)
        info.isGitRepo = true

        // Default-branch comparison (the "vs main ↑↓" row). The caller resolves and
        // caches the default branch (see GitStatusMonitor); we just consume it here,
        // and skip the extra call when on the default branch (the sidebar hides the
        // row in that case anyway).
        // The cached value may be a remote-tracking ref ("origin/main" — fresh
        // clone, no local main). Count with the ref, display (and compare the
        // current branch against) the short name, so the row reads "vs main".
        let shortDefault = cachedDefaultBranch.map {
            $0.hasPrefix("origin/") ? String($0.dropFirst("origin/".count)) : $0
        }
        info.defaultBranch = shortDefault
        if let defaultBranch = cachedDefaultBranch, !defaultBranch.isEmpty, info.branch != shortDefault {
            let ab = await run(
                args: ["rev-list", "--count", "--left-right", "\(defaultBranch)...HEAD"],
                in: path
            )
            if ab.exitCode == 0 {
                let parts = ab.stdout.trimmingCharacters(in: .whitespacesAndNewlines).split(separator: "\t")
                if parts.count == 2 {
                    info.behindDefault = Int(parts[0]) ?? 0
                    info.aheadOfDefault = Int(parts[1]) ?? 0
                }
            }
        }
        return info
    }

    /// Detect the branch to compare the working branch against — the release/trunk
    /// branch, preferring `main`, then `master`, then the remote default (origin/HEAD).
    ///
    /// `main`/`master` come first on purpose. A repo can set its remote default
    /// (origin/HEAD) to an *integration* branch like `dev` that you work on directly
    /// and merge into `main` to deploy. Trusting origin/HEAD there would make ccmux
    /// think `dev` is the base and hide the comparison — when what you actually want
    /// is "dev vs main" (am I ahead of the release branch?). Falling back to
    /// origin/HEAD only when neither main nor master exists still handles repos whose
    /// trunk is genuinely named something else.
    ///
    /// Fresh clones checked out on the integration branch have no LOCAL main —
    /// remote-tracking origin/main|master slots in before origin/HEAD so the
    /// row still shows "vs main" instead of hiding (dev vs dev). The result is
    /// a REF (possibly "origin/main"); `status` derives the display name.
    ///
    /// Meant to be called once per repo and cached by the caller.
    static func detectDefaultBranch(path: String) async -> String? {
        for name in ["main", "master"] {
            if !(await run(args: ["rev-parse", "--verify", "refs/heads/\(name)"], in: path)).stdout.isEmpty { return name }
        }
        for name in ["origin/main", "origin/master"] {
            if !(await run(args: ["rev-parse", "--verify", "refs/remotes/\(name)"], in: path)).stdout.isEmpty { return name }
        }
        let originHead = await run(args: ["symbolic-ref", "--short", "refs/remotes/origin/HEAD"], in: path)
            .stdout.trimmingCharacters(in: .whitespacesAndNewlines)
        if !originHead.isEmpty {
            // "origin/dev" → "dev"
            return originHead.split(separator: "/").last.map(String.init)
        }
        return nil
    }

    // MARK: - Porcelain v2 parsing

    /// Parse `git status --porcelain=v2 --branch` output into a `GitStatusInfo`
    /// (without `isGitRepo`/default-branch fields, which the caller fills in).
    ///
    /// Pure function on the raw string — unit-tested without spawning git.
    static func parseStatusV2(_ output: String) -> GitStatusInfo {
        var info = GitStatusInfo()
        var oid = ""
        var head = ""

        for line in output.split(separator: "\n", omittingEmptySubsequences: true) {
            let chars = Array(line)
            guard let first = chars.first else { continue }

            if first == "#" {
                parseHeader(line, oid: &oid, head: &head, info: &info)
                continue
            }
            guard chars.count > 3 else {
                if first == "?" { info.untrackedFiles.append(.init(path: String(line.dropFirst(2)), status: .untracked)) }
                continue
            }
            switch first {
            case "1":
                categorize(x: chars[2], y: chars[3], path: lastField(line, fieldCount: 9), into: &info)
            case "2":
                let raw = lastField(line, fieldCount: 10)
                let newPath = raw.split(separator: "\t", maxSplits: 1).first.map(String.init) ?? raw
                categorize(x: chars[2], y: chars[3], path: newPath, into: &info)
            case "?":
                info.untrackedFiles.append(.init(path: String(line.dropFirst(2)), status: .untracked))
            default:
                break  // 'u' (unmerged) / '!' (ignored) — not surfaced in the dashboard
            }
        }

        // Detached HEAD reports "(detached)" for branch.head; use the short SHA instead.
        info.branch = (head == "(detached)") ? String(oid.prefix(7)) : head
        return info
    }

    /// Parse a `# branch.*` header line into `info` (and the oid/head locals).
    private static func parseHeader(_ line: Substring, oid: inout String, head: inout String, info: inout GitStatusInfo) {
        let parts = line.dropFirst(2).split(separator: " ", maxSplits: 1)
        guard let keySub = parts.first else { return }
        let key = String(keySub)
        let value = parts.count > 1 ? String(parts[1]) : ""
        switch key {
        case "branch.oid": oid = value
        case "branch.head": head = value
        case "branch.upstream": info.trackingBranch = value.isEmpty ? nil : value
        case "branch.ab":
            for token in value.split(separator: " ") {
                if token.hasPrefix("+") { info.ahead = Int(token.dropFirst()) ?? 0 }
                else if token.hasPrefix("-") { info.behind = Int(token.dropFirst()) ?? 0 }
            }
        default:
            break
        }
    }

    /// The path field of a changed-entry line is everything after the fixed-count
    /// leading fields. Splitting with a maxSplits cap keeps spaces in the path intact.
    private static func lastField(_ line: Substring, fieldCount: Int) -> String {
        let pieces = line.split(separator: " ", maxSplits: fieldCount - 1, omittingEmptySubsequences: false)
        return String(pieces.last ?? "")
    }

    /// Map a porcelain XY code (X = index/staged, Y = worktree) into the file buckets.
    private static func categorize(x: Character, y: Character, path: String, into info: inout GitStatusInfo) {
        switch x {
        case "M": info.stagedFiles.append(.init(path: path, status: .modified))
        case "A": info.stagedFiles.append(.init(path: path, status: .added))
        case "D": info.stagedFiles.append(.init(path: path, status: .deleted))
        case "R", "C": info.stagedFiles.append(.init(path: path, status: .renamed))
        default: break
        }
        switch y {
        case "M": info.modifiedFiles.append(.init(path: path, status: .modified))
        case "D": info.deletedFiles.append(.init(path: path, status: .deleted))
        default: break
        }
    }
}

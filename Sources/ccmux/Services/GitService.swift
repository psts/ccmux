import Foundation

enum GitService {
    /// Collector for stdout bytes streamed asynchronously off a Pipe — no parked thread.
    private final class OutputCollector: @unchecked Sendable {
        private var data = Data()
        private var resumed = false
        private let lock = NSLock()

        func append(_ chunk: Data) {
            lock.lock(); defer { lock.unlock() }
            data.append(chunk)
        }

        /// Returns the buffered data exactly once; subsequent calls return nil.
        func takeIfFirst() -> Data? {
            lock.lock(); defer { lock.unlock() }
            guard !resumed else { return nil }
            resumed = true
            return data
        }
    }

    /// Run a git command, returning its stdout and exit code.
    ///
    /// Drains the child's stdout via `Pipe.readabilityHandler` instead of
    /// `waitUntilExit()` + `readDataToEndOfFile()`. The blocking variants would
    /// park one GCD worker thread per inflight git invocation; with many workspaces
    /// this can saturate the 64-thread soft limit and stall pane I/O across the app.
    ///
    /// The handler fires with non-empty data as the child writes, and once with
    /// empty data when the child closes its write end (EOF). EOF means the child
    /// has finished writing and is exiting, so the `waitUntilExit()` that follows
    /// returns immediately and lets us read `terminationStatus`. We deliberately
    /// avoid `Process.terminationHandler` because it runs on a different queue from
    /// `readabilityHandler` and racing reads against the same FileHandle yields
    /// partial/empty output.
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

            let pipeHandle = pipe.fileHandleForReading
            let collector = OutputCollector()

            pipeHandle.readabilityHandler = { handle in
                let chunk = handle.availableData
                if chunk.isEmpty {
                    handle.readabilityHandler = nil
                    if let data = collector.takeIfFirst() {
                        process.waitUntilExit()
                        let text = String(data: data, encoding: .utf8) ?? ""
                        continuation.resume(returning: (text, process.terminationStatus))
                    }
                } else {
                    collector.append(chunk)
                }
            }

            do {
                try process.run()
            } catch {
                pipeHandle.readabilityHandler = nil
                if collector.takeIfFirst() != nil {
                    continuation.resume(returning: ("", -1))
                }
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
        info.defaultBranch = cachedDefaultBranch
        if let defaultBranch = cachedDefaultBranch, !defaultBranch.isEmpty, info.branch != defaultBranch {
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

    /// Detect the default branch (origin/HEAD, else main, else master).
    /// Meant to be called once per repo and cached by the caller.
    static func detectDefaultBranch(path: String) async -> String? {
        let originHead = await run(args: ["symbolic-ref", "--short", "refs/remotes/origin/HEAD"], in: path)
            .stdout.trimmingCharacters(in: .whitespacesAndNewlines)
        if !originHead.isEmpty {
            // "origin/main" → "main"
            return originHead.split(separator: "/").last.map(String.init)
        }
        if !(await run(args: ["rev-parse", "--verify", "main"], in: path)).stdout.isEmpty { return "main" }
        if !(await run(args: ["rev-parse", "--verify", "master"], in: path)).stdout.isEmpty { return "master" }
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

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

    /// Run a git command and return stdout.
    ///
    /// Drains the child's stdout via `Pipe.readabilityHandler` instead of
    /// `waitUntilExit()` + `readDataToEndOfFile()`. The blocking variants would
    /// park one GCD worker thread per inflight git invocation; with many workspaces
    /// this can saturate the 64-thread soft limit and stall pane I/O across the app.
    ///
    /// The handler fires with non-empty data as the child writes, and once with
    /// empty data when the child closes its write end of the pipe (EOF). EOF is
    /// our completion signal — by that point the child has finished writing.
    /// We deliberately avoid `Process.terminationHandler` because it runs on a
    /// different queue from `readabilityHandler` and racing reads against the
    /// same FileHandle yields partial/empty output (the source of the flicker
    /// in the previous version of this function).
    static func run(args: [String], in directory: String) async -> String {
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
                        continuation.resume(returning: String(data: data, encoding: .utf8) ?? "")
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
                    continuation.resume(returning: "")
                }
            }
        }
    }

    static func isGitRepo(path: String) async -> Bool {
        // Cheap filesystem check first. Avoids spawning `git` for the common
        // case (~all repos have `.git` at the root), which used to fail under
        // the startup spawn-burst and paint "Not a git repository" over real
        // repos until the next 30s poll tick.
        let gitPath = (path as NSString).appendingPathComponent(".git")
        if FileManager.default.fileExists(atPath: gitPath) { return true }
        // Fallback for worktrees / unusual layouts where `.git` isn't at the root.
        let result = await run(args: ["rev-parse", "--git-dir"], in: path)
        return !result.isEmpty
    }

    static func repoRoot(path: String) async -> String? {
        let result = await run(args: ["rev-parse", "--show-toplevel"], in: path)
        let trimmed = result.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }

    static func currentBranch(path: String) async -> String {
        let result = await run(args: ["branch", "--show-current"], in: path)
        return result.trimmingCharacters(in: .whitespacesAndNewlines)
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
        return await run(args: args, in: repoPath)
    }

    static func status(path: String) async -> String {
        await run(args: ["status", "--porcelain"], in: path)
    }

    // MARK: - Full Status (for sidebar dashboard)

    /// Fetch complete git status for a workspace: branch, tracking, ahead/behind, file changes.
    static func fullStatus(path: String) async -> GitStatusInfo {
        guard await isGitRepo(path: path) else {
            return GitStatusInfo()
        }

        // Run queries concurrently
        async let branchResult = run(args: ["symbolic-ref", "--short", "HEAD"], in: path)
        async let trackingResult = run(args: ["rev-parse", "--abbrev-ref", "@{upstream}"], in: path)
        async let statusResult = run(args: ["status", "--porcelain"], in: path)
        // left-right gives "behind\tahead" in one call
        async let aheadBehindResult = run(args: ["rev-list", "--count", "--left-right", "@{upstream}...HEAD"], in: path)

        let branch = await branchResult.trimmingCharacters(in: .whitespacesAndNewlines)
        let tracking = await trackingResult.trimmingCharacters(in: .whitespacesAndNewlines)
        let porcelain = await statusResult
        let abRaw = await aheadBehindResult.trimmingCharacters(in: .whitespacesAndNewlines)

        // Parse branch — fallback to short SHA for detached HEAD
        var finalBranch = branch
        if finalBranch.isEmpty {
            let sha = await run(args: ["rev-parse", "--short", "HEAD"], in: path)
            finalBranch = sha.trimmingCharacters(in: .whitespacesAndNewlines)
        }

        // Parse tracking branch
        let finalTracking: String? = tracking.isEmpty ? nil : tracking

        // Parse ahead/behind from "behind\tahead" format
        var ahead = 0
        var behind = 0
        let abParts = abRaw.split(separator: "\t")
        if abParts.count == 2 {
            behind = Int(abParts[0]) ?? 0
            ahead = Int(abParts[1]) ?? 0
        }

        // Parse porcelain output
        let (staged, modified, untracked, deleted) = parsePorcelain(porcelain)

        // Detect default branch and compare
        let defaultBranch = await detectDefaultBranch(path: path)
        var aheadOfDefault = 0
        var behindDefault = 0
        if let defaultBranch, finalBranch != defaultBranch {
            let defaultAB = await run(
                args: ["rev-list", "--count", "--left-right", "\(defaultBranch)...HEAD"],
                in: path
            ).trimmingCharacters(in: .whitespacesAndNewlines)
            let parts = defaultAB.split(separator: "\t")
            if parts.count == 2 {
                behindDefault = Int(parts[0]) ?? 0
                aheadOfDefault = Int(parts[1]) ?? 0
            }
        }

        var info = GitStatusInfo()
        info.branch = finalBranch
        info.trackingBranch = finalTracking
        info.ahead = ahead
        info.behind = behind
        info.defaultBranch = defaultBranch
        info.aheadOfDefault = aheadOfDefault
        info.behindDefault = behindDefault
        info.stagedFiles = staged
        info.modifiedFiles = modified
        info.untrackedFiles = untracked
        info.deletedFiles = deleted
        info.isGitRepo = true
        return info
    }

    /// Detect the default branch (main, master, or whatever origin/HEAD points to).
    private static func detectDefaultBranch(path: String) async -> String? {
        // Try origin/HEAD first (most reliable)
        let originHead = await run(args: ["symbolic-ref", "--short", "refs/remotes/origin/HEAD"], in: path)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        if !originHead.isEmpty {
            // Returns "origin/main" → extract "main"
            return originHead.split(separator: "/").last.map(String.init)
        }

        // Fallback: check if main or master exists
        let mainCheck = await run(args: ["rev-parse", "--verify", "main"], in: path)
        if !mainCheck.isEmpty { return "main" }

        let masterCheck = await run(args: ["rev-parse", "--verify", "master"], in: path)
        if !masterCheck.isEmpty { return "master" }

        return nil
    }

    /// Parse `git status --porcelain` output into categorized file changes.
    /// Format: XY filename (X = index/staged status, Y = working tree status)
    static func parsePorcelain(_ output: String) -> (
        staged: [GitStatusInfo.FileChange],
        modified: [GitStatusInfo.FileChange],
        untracked: [GitStatusInfo.FileChange],
        deleted: [GitStatusInfo.FileChange]
    ) {
        var staged: [GitStatusInfo.FileChange] = []
        var modified: [GitStatusInfo.FileChange] = []
        var untracked: [GitStatusInfo.FileChange] = []
        var deleted: [GitStatusInfo.FileChange] = []

        for line in output.split(separator: "\n", omittingEmptySubsequences: true) {
            guard line.count >= 3 else { continue }
            let chars = Array(line)
            let x = chars[0]  // index (staged) status
            let y = chars[1]  // working tree status
            var path = String(line.dropFirst(3))

            // Handle renames: "R  old -> new"
            if let arrowRange = path.range(of: " -> ") {
                path = String(path[arrowRange.upperBound...])
            }

            // Untracked files
            if x == "?" && y == "?" {
                untracked.append(.init(path: path, status: .untracked))
                continue
            }

            // Staged changes (index column)
            switch x {
            case "M":
                staged.append(.init(path: path, status: .modified))
            case "A":
                staged.append(.init(path: path, status: .added))
            case "D":
                staged.append(.init(path: path, status: .deleted))
            case "R":
                staged.append(.init(path: path, status: .renamed))
            default:
                break
            }

            // Working tree changes
            switch y {
            case "M":
                modified.append(.init(path: path, status: .modified))
            case "D":
                deleted.append(.init(path: path, status: .deleted))
            default:
                break
            }
        }

        return (staged, modified, untracked, deleted)
    }
}

import XCTest
@testable import ccmux

final class GitServiceTests: XCTestCase {
    private func parse(_ lines: [String]) -> GitStatusInfo {
        GitService.parseStatusV2(lines.joined(separator: "\n"))
    }

    // MARK: - Branch header

    func testCleanRepoNoUpstream() {
        let info = parse([
            "# branch.oid 43bdeb4a1ac716d7474439e08ec4e7f83a4c7305",
            "# branch.head master",
        ])
        XCTAssertEqual(info.branch, "master")
        XCTAssertNil(info.trackingBranch)
        XCTAssertEqual(info.ahead, 0)
        XCTAssertEqual(info.behind, 0)
        XCTAssertTrue(info.isClean)
    }

    func testUpstreamAndAheadBehind() {
        let info = parse([
            "# branch.oid db45204caa80cd229171d32292ab98c9c57d87a7",
            "# branch.head feature",
            "# branch.upstream origin/feature",
            "# branch.ab +2 -3",
        ])
        XCTAssertEqual(info.branch, "feature")
        XCTAssertEqual(info.trackingBranch, "origin/feature")
        XCTAssertEqual(info.ahead, 2)
        XCTAssertEqual(info.behind, 3)
    }

    func testDetachedHeadUsesShortSha() {
        let info = parse([
            "# branch.oid 1234567890abcdef1234567890abcdef12345678",
            "# branch.head (detached)",
        ])
        XCTAssertEqual(info.branch, "1234567")
    }

    func testNoCommitsYetKeepsBranchName() {
        let info = parse([
            "# branch.oid (initial)",
            "# branch.head main",
        ])
        XCTAssertEqual(info.branch, "main")
    }

    // MARK: - File categorization

    func testWorktreeModified() {
        let info = parse([
            "# branch.head main",
            "1 .M N... 100644 100644 100644 aaa bbb file.txt",
        ])
        XCTAssertEqual(info.modifiedFiles.map(\.path), ["file.txt"])
        XCTAssertTrue(info.stagedFiles.isEmpty)
        XCTAssertFalse(info.isClean)
    }

    func testStagedModified() {
        let info = parse([
            "# branch.head main",
            "1 M. N... 100644 100644 100644 aaa bbb file.txt",
        ])
        XCTAssertEqual(info.stagedFiles.map(\.path), ["file.txt"])
        XCTAssertTrue(info.modifiedFiles.isEmpty)
    }

    func testStagedAndWorktreeModified() {
        let info = parse([
            "# branch.head main",
            "1 MM N... 100644 100644 100644 aaa bbb both.txt",
        ])
        XCTAssertEqual(info.stagedFiles.map(\.path), ["both.txt"])
        XCTAssertEqual(info.modifiedFiles.map(\.path), ["both.txt"])
    }

    func testStagedAdded() {
        let info = parse([
            "# branch.head main",
            "1 A. N... 000000 100644 100644 zzz bbb new.txt",
        ])
        XCTAssertEqual(info.stagedFiles.count, 1)
        XCTAssertEqual(info.stagedFiles.first?.status, .added)
    }

    func testWorktreeDeleted() {
        let info = parse([
            "# branch.head main",
            "1 .D N... 100644 100644 000000 aaa bbb gone.txt",
        ])
        XCTAssertEqual(info.deletedFiles.map(\.path), ["gone.txt"])
        XCTAssertTrue(info.stagedFiles.isEmpty)
    }

    func testUntracked() {
        let info = parse([
            "# branch.head main",
            "? whatever.txt",
        ])
        XCTAssertEqual(info.untrackedFiles.map(\.path), ["whatever.txt"])
    }

    func testRenameUsesNewPath() {
        let info = parse([
            "# branch.head main",
            "2 R. N... 100644 100644 100644 aaa bbb R100 newname.txt\toldname.txt",
        ])
        XCTAssertEqual(info.stagedFiles.count, 1)
        XCTAssertEqual(info.stagedFiles.first?.path, "newname.txt")
        XCTAssertEqual(info.stagedFiles.first?.status, .renamed)
    }

    func testPathWithSpacesPreserved() {
        let info = parse([
            "# branch.head main",
            "1 .M N... 100644 100644 100644 aaa bbb my file with spaces.txt",
        ])
        XCTAssertEqual(info.modifiedFiles.map(\.path), ["my file with spaces.txt"])
    }

    // MARK: - End-to-end (real git spawn, exit-code repo detection)

    func testFullStatusEndToEnd() async throws {
        let tmp = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("ccmux-gittest-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: tmp) }

        // An empty directory must report not-a-repo via git's exit code (128).
        let notRepo = await GitService.fullStatus(path: tmp.path, cachedDefaultBranch: nil)
        XCTAssertEqual(notRepo?.isGitRepo, false)

        func git(_ args: [String]) async { _ = await GitService.run(args: args, in: tmp.path) }
        await git(["init", "-b", "main"])
        await git(["config", "user.email", "t@example.com"])
        await git(["config", "user.name", "t"])
        try "hello".write(to: tmp.appendingPathComponent("a.txt"), atomically: true, encoding: .utf8)
        await git(["add", "a.txt"])
        await git(["commit", "-m", "init"])

        let clean = await GitService.fullStatus(path: tmp.path, cachedDefaultBranch: nil)
        XCTAssertEqual(clean?.isGitRepo, true)
        XCTAssertEqual(clean?.branch, "main")
        XCTAssertEqual(clean?.isClean, true)

        try "changed".write(to: tmp.appendingPathComponent("a.txt"), atomically: true, encoding: .utf8)
        let dirty = await GitService.fullStatus(path: tmp.path, cachedDefaultBranch: nil)
        XCTAssertEqual(dirty?.modifiedFiles.map(\.path), ["a.txt"])
    }

    func testMixedChanges() {
        let info = parse([
            "# branch.oid db45204caa80cd229171d32292ab98c9c57d87a7",
            "# branch.head main",
            "# branch.upstream origin/main",
            "# branch.ab +1 -0",
            "1 M. N... 100644 100644 100644 aaa bbb staged.txt",
            "1 .M N... 100644 100644 100644 ccc ddd modified.txt",
            "1 .D N... 100644 100644 000000 eee fff deleted.txt",
            "? untracked.txt",
        ])
        XCTAssertEqual(info.ahead, 1)
        XCTAssertEqual(info.stagedFiles.map(\.path), ["staged.txt"])
        XCTAssertEqual(info.modifiedFiles.map(\.path), ["modified.txt"])
        XCTAssertEqual(info.deletedFiles.map(\.path), ["deleted.txt"])
        XCTAssertEqual(info.untrackedFiles.map(\.path), ["untracked.txt"])
        XCTAssertEqual(info.totalChanges, 4)
    }

    // MARK: - Concurrency / crash regression

    /// Regression for the random SIGABRT crashes: `GitService.run` used to drain the
    /// pipe via `NSFileHandle.availableData`, which raises an uncaught ObjC NSException
    /// (EBADF) under concurrent fd churn and aborts the process. Firing many `run`
    /// invocations at once reproduced that abort; the POSIX-read drain must survive it.
    func testConcurrentRunsDoNotCrash() async throws {
        let tmp = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("ccmux-gitconc-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: tmp) }

        func git(_ args: [String]) async { _ = await GitService.run(args: args, in: tmp.path) }
        await git(["init", "-b", "main"])
        await git(["config", "user.email", "t@example.com"])
        await git(["config", "user.name", "t"])
        try "hello".write(to: tmp.appendingPathComponent("a.txt"), atomically: true, encoding: .utf8)
        await git(["add", "a.txt"])
        await git(["commit", "-m", "init"])
        // Leave the worktree dirty + an untracked file so status output is non-trivial.
        try "changed".write(to: tmp.appendingPathComponent("a.txt"), atomically: true, encoding: .utf8)
        try "new".write(to: tmp.appendingPathComponent("b.txt"), atomically: true, encoding: .utf8)

        let okCount = await withTaskGroup(of: Bool.self) { group in
            for _ in 0..<200 {
                group.addTask {
                    let result = await GitService.run(
                        args: ["status", "--porcelain=v2", "--branch"], in: tmp.path
                    )
                    let info = GitService.parseStatusV2(result.stdout)
                    return result.exitCode == 0 && info.branch == "main" && info.totalChanges > 0
                }
            }
            var count = 0
            for await ok in group where ok { count += 1 }
            return count
        }
        XCTAssertEqual(okCount, 200, "every concurrent git status should succeed and parse")
    }

    /// `run` against a path that can't host git returns the -1 "couldn't exec/transient"
    /// sentinel that callers (GitStatusMonitor) rely on, without crashing.
    func testRunInNonexistentDirectoryReturnsSentinel() async {
        let result = await GitService.run(
            args: ["status"], in: "/nonexistent-ccmux-\(UUID().uuidString)"
        )
        XCTAssertEqual(result.exitCode, -1)
        XCTAssertEqual(result.stdout, "")
    }
}

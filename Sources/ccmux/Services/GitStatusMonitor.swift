import Foundation
import Combine

/// Watches a workspace's git repository for changes and publishes updated status.
///
/// File-system watching on `.git` runs always (cheap — one fd + dispatch source)
/// and catches local commits/branch switches/fetches/pulls within ~0.5s.
/// The periodic poll is opt-in via `resume()`; `WindowManager` resumes monitors
/// only for workspaces the user can actually see (active-Space window's displayed
/// workspace + its expanded sidebar rows). Monitors for invisible workspaces stay
/// paused, eliminating ~5–8 git invocations per workspace per 30s for everything
/// off-screen.
class GitStatusMonitor: ObservableObject {
    @Published var status: GitStatusInfo = GitStatusInfo()

    /// A no-op monitor for use as a fallback when no monitor exists.
    static let empty = GitStatusMonitor()

    private let repoPath: String
    private var fileSource: DispatchSourceFileSystemObject?
    private var refreshDebouncer: Debouncer
    private var pollTimer: DispatchSourceTimer?
    private var pollIntervalSec: TimeInterval = 30
    private var isActive = true
    /// Bumped on every `performRefresh` entry; only the latest in-flight refresh
    /// writes to `status`. Prevents a fast-failing later task from clobbering a
    /// slow-succeeding earlier task during the startup burst.
    private var refreshGeneration: UInt64 = 0

    /// No-op init for the static `empty` sentinel.
    private init() {
        self.repoPath = ""
        self.refreshDebouncer = Debouncer(delay: 1)
        self.isActive = false
    }

    init(repoPath: String) {
        self.repoPath = repoPath
        self.refreshDebouncer = Debouncer(delay: 0.5)

        // No initial fetch here. WindowManager.recomputeActiveSet() fires a
        // refresh via resume() for every visible workspace within ~100ms of
        // launch; firing a second one from init just doubles the startup git
        // burst and races last-writer-wins against the resume refresh, which
        // is how repos with valid `.git` ended up rendering "Not a git
        // repository". Off-screen workspaces stay at the default
        // GitStatusInfo() until their window becomes visible.
        setupFileWatcher()

        // Periodic poll starts paused; WindowManager resumes the visible ones.
    }

    deinit {
        stop()
    }

    /// Manually trigger a refresh.
    func refresh() {
        Task { await performRefresh() }
    }

    /// Stop watching and clean up resources. Permanent — `resume()` after
    /// `stop()` is a no-op because the monitor is no longer active.
    func stop() {
        isActive = false
        fileSource?.cancel()
        fileSource = nil
        pollTimer?.cancel()
        pollTimer = nil
        refreshDebouncer.cancel()
    }

    /// Stop the periodic poll. The file watcher keeps firing on local changes
    /// so a paused workspace stays approximately fresh while idle. Idempotent.
    func pause() {
        guard pollTimer != nil else { return }
        pollTimer?.cancel()
        pollTimer = nil
    }

    /// Start the periodic poll and refresh immediately so the user sees current
    /// state on Space-switch / sidebar-expand without waiting a full interval.
    /// Idempotent; no-op if already polling or stopped.
    func resume() {
        guard isActive, pollTimer == nil else { return }
        setupPollTimer()
        Task { await performRefresh() }
    }

    /// Change the poll cadence. If currently polling, re-arm with the new interval.
    func setPollInterval(_ seconds: TimeInterval) {
        guard pollIntervalSec != seconds else { return }
        pollIntervalSec = seconds
        if pollTimer != nil {
            setupPollTimer()
        }
    }

    // MARK: - Private

    @MainActor
    private func performRefresh() async {
        guard isActive else { return }
        refreshGeneration &+= 1
        let myGen = refreshGeneration
        let newStatus = await GitService.fullStatus(path: repoPath)
        guard isActive, myGen == refreshGeneration else { return }
        // A transient `isGitRepo` miss (process spawn race under burst) must
        // not clobber a previously-confirmed repo state. Real `.git` removal
        // is caught independently by the file watcher.
        if !newStatus.isGitRepo && self.status.isGitRepo { return }
        // An empty branch on a known-good repo means both `symbolic-ref` and
        // the short-SHA fallback returned nothing — i.e. the concurrent git
        // invocations partially failed under burst. Don't paint the gaps
        // (empty file lists, zero ahead/behind) over the cached good data.
        if newStatus.branch.isEmpty && !self.status.branch.isEmpty { return }
        self.status = newStatus
    }

    private func setupFileWatcher() {
        let gitDir = (repoPath as NSString).appendingPathComponent(".git")

        // .git might be a file (for worktrees) — resolve it
        var watchPath = gitDir
        var isDir: ObjCBool = false
        if FileManager.default.fileExists(atPath: gitDir, isDirectory: &isDir), !isDir.boolValue {
            // .git is a file pointing to the real git dir — just watch the file
            watchPath = gitDir
        }

        let fd = open(watchPath, O_EVTONLY)
        guard fd >= 0 else { return }

        let source = DispatchSource.makeFileSystemObjectSource(
            fileDescriptor: fd,
            eventMask: [.write, .rename, .delete, .attrib],
            queue: .global(qos: .utility)
        )

        source.setEventHandler { [weak self] in
            self?.refreshDebouncer.call {
                Task { [weak self] in
                    await self?.performRefresh()
                }
            }
        }

        source.setCancelHandler {
            close(fd)
        }

        source.resume()
        self.fileSource = source
    }

    private func setupPollTimer() {
        // Cancel any prior timer so this is safe to call from setPollInterval too.
        pollTimer?.cancel()

        let timer = DispatchSource.makeTimerSource(queue: .global(qos: .utility))
        timer.schedule(deadline: .now() + pollIntervalSec, repeating: pollIntervalSec)

        timer.setEventHandler { [weak self] in
            self?.refreshDebouncer.call {
                Task { [weak self] in
                    await self?.performRefresh()
                }
            }
        }

        timer.resume()
        self.pollTimer = timer
    }
}

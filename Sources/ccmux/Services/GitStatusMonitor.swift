import Foundation
import Combine

/// Watches a workspace's git repository for changes and publishes updated status.
///
/// One recursive FSEvent stream per workspace, started in `init` and torn down in
/// `stop()` (workspace close). FSEvents catches working-tree edits AND `.git`
/// changes (commits, branch switches, fetches) — i.e. everything that can shift
/// the answer of `git status`. The stream is free at idle: the kernel only
/// delivers a callback when something actually changes on disk, so running one
/// per workspace regardless of window focus costs nothing until real activity.
class GitStatusMonitor: ObservableObject {
    @Published var status: GitStatusInfo = GitStatusInfo()

    /// A no-op monitor for use as a fallback when no monitor exists.
    static let empty = GitStatusMonitor()

    /// A remote-fed monitor for a hosted workspace: the repo lives on the
    /// daemon's host, so the daemon computes status and `apply(_:)` publishes
    /// it here. No FSEvents watcher; `refresh()` is a no-op.
    static func remoteFed() -> GitStatusMonitor { GitStatusMonitor() }

    /// Publish daemon-computed status (main thread — called from reconcile).
    func apply(_ new: GitStatusInfo) { status = new }

    private let repoPath: String
    private var watcher: RepoWatcher?
    private var isActive = true
    /// Bumped on every `performRefresh` entry; only the latest in-flight refresh
    /// writes to `status`. Without this, a slow earlier refresh can overwrite a
    /// fresh later one when an FS-event burst stacks up.
    private var refreshGeneration: UInt64 = 0
    /// Default branch (main/master/origin-HEAD), resolved once on first confirmed
    /// repo and reused on every later refresh. `resolved` distinguishes "not looked
    /// up yet" from a genuine nil (repo with no main/master/remote).
    private var defaultBranch: String?
    private var defaultBranchResolved = false

    /// No-op init for the static `empty` sentinel.
    private init() {
        self.repoPath = ""
        self.isActive = false
    }

    init(repoPath: String) {
        self.repoPath = repoPath
        let w = RepoWatcher(repoPath: repoPath) { [weak self] in
            self?.refresh()
        }
        w.start()
        watcher = w
        refresh()
    }

    deinit {
        stop()
    }

    /// Manually trigger a refresh. Used by the window-focus handler.
    func refresh() {
        Task { await performRefresh() }
    }

    /// Stop watching and clean up resources. Permanent — the monitor is no longer
    /// active afterward. Called on workspace close.
    func stop() {
        isActive = false
        watcher?.stop()
        watcher = nil
    }

    // MARK: - Private

    @MainActor
    private func performRefresh() async {
        guard isActive else { return }
        refreshGeneration &+= 1
        let myGen = refreshGeneration
        // nil = couldn't exec git (transient); keep prior status. exit 128 = genuine
        // non-repo, which comes back as a cleared GitStatusInfo (isGitRepo == false).
        guard let newStatus = await GitService.fullStatus(path: repoPath, cachedDefaultBranch: defaultBranch) else { return }
        guard isActive, myGen == refreshGeneration else { return }
        self.status = newStatus

        // Once we know it's a repo, resolve the default branch a single time and
        // refresh again so the "vs default" row populates. Cached thereafter.
        if newStatus.isGitRepo && !defaultBranchResolved {
            defaultBranchResolved = true
            defaultBranch = await GitService.detectDefaultBranch(path: repoPath)
            refresh()
        }
    }
}

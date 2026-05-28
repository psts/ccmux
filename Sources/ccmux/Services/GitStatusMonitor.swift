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

    private let repoPath: String
    private var watcher: RepoWatcher?
    private var isActive = true
    /// Bumped on every `performRefresh` entry; only the latest in-flight refresh
    /// writes to `status`. Without this, a slow earlier refresh can overwrite a
    /// fresh later one when an FS-event burst stacks up.
    private var refreshGeneration: UInt64 = 0

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
        // nil = transient failure (e.g. fork pressure / index.lock); keep prior status.
        guard let newStatus = await GitService.fullStatus(path: repoPath) else { return }
        guard isActive, myGen == refreshGeneration else { return }
        self.status = newStatus
    }
}

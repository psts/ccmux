import Foundation
import Combine

/// Watches a workspace's git repository for changes and publishes updated status.
/// Uses DispatchSource file system watching on .git directory + periodic polling.
class GitStatusMonitor: ObservableObject {
    @Published var status: GitStatusInfo = GitStatusInfo()

    /// A no-op monitor for use as a fallback when no monitor exists.
    static let empty = GitStatusMonitor()

    private let repoPath: String
    private var fileSource: DispatchSourceFileSystemObject?
    private var refreshDebouncer: Debouncer
    private var pollTimer: DispatchSourceTimer?
    private var isActive = true

    /// No-op init for the static `empty` sentinel.
    private init() {
        self.repoPath = ""
        self.refreshDebouncer = Debouncer(delay: 1)
        self.isActive = false
    }

    init(repoPath: String) {
        self.repoPath = repoPath
        self.refreshDebouncer = Debouncer(delay: 0.5)

        // Initial fetch
        Task { await performRefresh() }

        // Set up file system watching on .git directory
        setupFileWatcher()

        // Poll every 30s for remote changes
        setupPollTimer()
    }

    deinit {
        stop()
    }

    /// Manually trigger a refresh.
    func refresh() {
        Task { await performRefresh() }
    }

    /// Stop watching and clean up resources.
    func stop() {
        isActive = false
        fileSource?.cancel()
        fileSource = nil
        pollTimer?.cancel()
        pollTimer = nil
        refreshDebouncer.cancel()
    }

    // MARK: - Private

    @MainActor
    private func performRefresh() async {
        guard isActive else { return }
        let newStatus = await GitService.fullStatus(path: repoPath)
        guard isActive else { return }
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
        let timer = DispatchSource.makeTimerSource(queue: .global(qos: .utility))
        timer.schedule(deadline: .now() + 30, repeating: 30)

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

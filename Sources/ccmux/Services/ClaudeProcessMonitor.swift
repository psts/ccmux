import Foundation
import Combine
import Darwin

/// Monitors whether a "claude" process is running with its working directory
/// matching a workspace's repo path. Uses native libproc API — no process spawning.
class ClaudeProcessMonitor: ObservableObject {
    @Published var isRunning: Bool = false

    static let empty = ClaudeProcessMonitor()

    private let repoPath: String
    private var pollTimer: DispatchSourceTimer?
    private var isActive = true

    /// No-op init for the static `empty` sentinel.
    private init() {
        self.repoPath = ""
        self.isActive = false
    }

    init(repoPath: String) {
        self.repoPath = repoPath
        Task { await performCheck() }
        setupPollTimer()
    }

    deinit { stop() }

    func stop() {
        isActive = false
        pollTimer?.cancel()
        pollTimer = nil
    }

    // MARK: - Private

    @MainActor
    private func performCheck() async {
        guard isActive, !repoPath.isEmpty else { return }
        let running = checkClaudeRunning()
        guard isActive else { return }
        self.isRunning = running
    }

    private func setupPollTimer() {
        let timer = DispatchSource.makeTimerSource(queue: .global(qos: .utility))
        timer.schedule(deadline: .now() + 10, repeating: 10)
        timer.setEventHandler { [weak self] in
            Task { [weak self] in await self?.performCheck() }
        }
        timer.resume()
        self.pollTimer = timer
    }

    private func checkClaudeRunning() -> Bool {
        // Step 1: Get all PIDs
        let pidCount = proc_listpids(UInt32(PROC_ALL_PIDS), 0, nil, 0)
        guard pidCount > 0 else { return false }

        var pids = [pid_t](repeating: 0, count: Int(pidCount))
        let actualSize = proc_listpids(UInt32(PROC_ALL_PIDS), 0, &pids, pidCount)
        guard actualSize > 0 else { return false }

        let count = Int(actualSize) / MemoryLayout<pid_t>.size

        // Step 2: Find PIDs whose executable path indicates Claude Code
        // proc_name() returns the actual binary filename (e.g. "2.1.81" for
        // .local/share/claude/versions/2.1.81), so we use proc_pidpath instead.
        var claudePids: [pid_t] = []
        for i in 0..<count {
            let pid = pids[i]
            guard pid > 0 else { continue }

            var pathBuffer = [CChar](repeating: 0, count: Int(MAXPATHLEN))
            let pathLen = proc_pidpath(pid, &pathBuffer, UInt32(MAXPATHLEN))
            guard pathLen > 0 else { continue }
            let path = String(cString: pathBuffer)

            let lastComponent = (path as NSString).lastPathComponent
            if lastComponent == "claude" || path.contains("/claude/versions/") {
                claudePids.append(pid)
            }
        }

        guard !claudePids.isEmpty else { return false }

        // Step 3: Check cwd of claude processes
        for pid in claudePids {
            var pathInfo = proc_vnodepathinfo()
            let size = Int32(MemoryLayout<proc_vnodepathinfo>.size)
            let ret = proc_pidinfo(pid, PROC_PIDVNODEPATHINFO, 0, &pathInfo, size)
            guard ret > 0 else { continue }

            let cwd = withUnsafePointer(to: &pathInfo.pvi_cdir.vip_path) { ptr in
                ptr.withMemoryRebound(to: CChar.self, capacity: Int(MAXPATHLEN)) {
                    String(cString: $0)
                }
            }

            if cwd == repoPath || cwd.hasPrefix(repoPath + "/") {
                return true
            }
        }

        return false
    }
}

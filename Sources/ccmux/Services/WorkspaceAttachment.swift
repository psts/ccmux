import Foundation

/// Runtime state for one attached hosted workspace: its single WS attach connection
/// (multiplexing all panes), one `RemoteTermController` per pane, and the routing
/// that feeds daemon frames into them + drives the sidebar attention flash.
///
/// Controllers are pre-created for every known pane *before* connecting, so each pane
/// receives the daemon's initial `capture-pane` snapshot even while it's off-screen —
/// mirroring how `TerminalStore` pre-warms local terminals so switching is instant.
final class WorkspaceAttachment {
    let workspaceId: UUID       // app-side UUID (== daemon workspace uuid)
    let daemonId: String
    let repoPath: String

    private let attach: DaemonAttachClient
    private let attention: ClaudeAttentionMonitor
    private var controllers: [String: RemoteTermController] = [:]  // daemon paneId → controller

    /// Called on the main thread when the connection state changes (drives the overlay).
    var onConnectionState: ((DaemonConnectionState) -> Void)?
    /// True while the user is actively looking at this workspace — suppresses the flash.
    var isWatched: () -> Bool = { false }
    /// Post a system notification for a needs-input/done transition on an unwatched pane.
    var onAttention: ((AttentionState) -> Void)?
    /// Route a clicked file link (absolute local path) to the app.
    var onFileLink: ((String) -> Void)?

    private(set) var connectionState: DaemonConnectionState = .connecting

    init(workspaceId: UUID, daemonId: String, repoPath: String, panes: [DaemonPane], attentionMonitor: ClaudeAttentionMonitor) {
        self.workspaceId = workspaceId
        self.daemonId = daemonId
        self.repoPath = repoPath
        self.attention = attentionMonitor
        self.attach = DaemonAttachClient(workspaceId: daemonId)
        for pane in panes { _ = makeController(paneId: pane.id, workingDirectory: pane.cwd) }
        attach.onEvent = { [weak self] in self?.handle($0) }
        attach.onStateChange = { [weak self] state in
            self?.connectionState = state
            self?.onConnectionState?(state)
        }
    }

    func connect() { attach.connect() }
    func disconnect() { attach.disconnect() }

    /// Get (or lazily create) the controller backing a pane, for the hosting view.
    func controller(forPane paneId: String, workingDirectory: String) -> RemoteTermController {
        controllers[paneId] ?? makeController(paneId: paneId, workingDirectory: workingDirectory)
    }

    /// Whether this workspace owns the given daemon pane (used for the reverse lookup
    /// a hosted pane view does to find its controller/connection state).
    func hasPane(_ paneId: String) -> Bool { controllers[paneId] != nil }

    private func makeController(paneId: String, workingDirectory: String) -> RemoteTermController {
        let cwd = workingDirectory.isEmpty ? repoPath : workingDirectory
        let c = RemoteTermController(paneId: paneId, workingDirectory: cwd, attach: attach)
        c.onFileLinkClicked = { [weak self] rel in self?.onFileLink?(rel) }
        controllers[paneId] = c
        return c
    }

    // MARK: - Frame routing

    private func handle(_ event: DaemonEvent) {
        switch event {
        case .snapshot(let pane, let bytes):
            controller(forPane: pane, workingDirectory: repoPath).seedSnapshot(bytes)
        case .output(let pane, let bytes):
            controller(forPane: pane, workingDirectory: repoPath).feedOutput(bytes)
        case .attention(_, let state):
            applyAttention(state)
        case .hello, .presence, .paneAdded, .paneClosed, .unknown:
            break
        }
    }

    /// Mirror `ClaudeHookListener.handle`: a `.none` mapping clears; an actionable
    /// state either clears (already watching) or flashes + notifies.
    private func applyAttention(_ state: DaemonAttention) {
        let appState = state.appAttentionState
        guard appState != .none else { attention.clear(); return }
        if isWatched() {
            attention.clear()
            return
        }
        attention.set(appState)
        onAttention?(appState)
    }
}

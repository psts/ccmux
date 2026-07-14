import Foundation

/// Runtime state for one attached hosted workspace: its single WS attach connection
/// (multiplexing all panes) and one `RemoteTermController` per pane, feeding daemon
/// output/snapshot frames into them.
///
/// Attention is *not* handled here — it rides the global `/v1/events` firehose
/// (`DaemonEventsClient` → `RemoteSessionService`), so a sidebar row flashes whether
/// or not its workspace is attached. This connection carries pane bytes only.
///
/// Controllers are pre-created for every known pane *before* connecting, so each pane
/// receives the daemon's initial `capture-pane` snapshot even while it's off-screen —
/// mirroring how `TerminalStore` pre-warms local terminals so switching is instant.
final class WorkspaceAttachment {
    let workspaceId: UUID       // app-side UUID (== daemon workspace uuid)
    let daemonId: String
    let repoPath: String

    private let attach: DaemonAttachClient
    private var controllers: [String: RemoteTermController] = [:]  // daemon paneId → controller

    /// Called on the main thread when the connection state changes (drives the overlay).
    var onConnectionState: ((DaemonConnectionState) -> Void)?
    /// Route a clicked file link (absolute local path) to the app.
    var onFileLink: ((String) -> Void)?

    private(set) var connectionState: DaemonConnectionState = .connecting

    init(workspaceId: UUID, daemonId: String, repoPath: String, panes: [DaemonPane]) {
        self.workspaceId = workspaceId
        self.daemonId = daemonId
        self.repoPath = repoPath
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
        // Attention now rides the global firehose; the attach still carries the
        // per-workspace `attention` frame but it is authoritative on /v1/events.
        case .attention, .hello, .presence, .paneAdded, .paneClosed, .unknown:
            break
        }
    }
}

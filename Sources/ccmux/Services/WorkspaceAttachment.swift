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
    /// Fired when a pane's "another lens drove the size" staleness changes, so the
    /// service can publish it and the hosting view can show a "take over" control.
    var onPaneStale: ((String, Bool) -> Void)?

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

    /// Last focus frame sent ("" = explicitly unfocused; nil = never reported).
    private(set) var lastReportedFocus: String?

    /// Any pane id of this workspace — the focus frame needs one; nil when empty.
    var anyPaneId: String? { controllers.keys.first }

    /// Report this lens's focus to the daemon's presence hub (drives phone-push
    /// suppression): a non-empty pane id means "the user is watching this
    /// workspace at this screen"; "" clears it. Deduped; re-sent after every
    /// reconnect because presence is per-connection.
    func reportFocus(paneId: String) {
        guard paneId != lastReportedFocus else { return }
        lastReportedFocus = paneId
        attach.send(.focus(pane: paneId))
    }

    /// Reconcile per-pane controllers after a daemon-side pane change, keeping the
    /// WS connection up: pre-create controllers for new panes (same warm-start as
    /// init — the attach streams every pane of the workspace, so a new pane's first
    /// frames arrive on this very socket) and drop dead ones. Returns the removed
    /// pane ids so the service can clear derived per-pane state.
    func syncPanes(_ panes: [DaemonPane]) -> [String] {
        for pane in panes where controllers[pane.id] == nil {
            _ = makeController(paneId: pane.id, workingDirectory: pane.cwd)
        }
        let live = Set(panes.map { $0.id })
        let dead = controllers.keys.filter { !live.contains($0) }
        for id in dead { controllers.removeValue(forKey: id) }
        return dead
    }

    private func makeController(paneId: String, workingDirectory: String) -> RemoteTermController {
        let cwd = workingDirectory.isEmpty ? repoPath : workingDirectory
        let c = RemoteTermController(paneId: paneId, workingDirectory: cwd, attach: attach)
        c.onFileLinkClicked = { [weak self] rel in self?.onFileLink?(rel) }
        c.onStaleChanged = { [weak self] stale in self?.onPaneStale?(paneId, stale) }
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
        case .hello(let panes):
            for p in panes where p.cols > 0 {
                controller(forPane: p.id, workingDirectory: p.cwd).setAuthoritativeSize(cols: p.cols, rows: p.rows)
            }
            // A hello means a (re)connect: the daemon's presence entry for this
            // connection is fresh, so replay our focus state.
            if let focus = lastReportedFocus, !focus.isEmpty {
                attach.send(.focus(pane: focus))
            }
        case .paneSize(let pane, let cols, let rows):
            controller(forPane: pane, workingDirectory: repoPath).setAuthoritativeSize(cols: cols, rows: rows)
        // Attention now rides the global firehose; the attach still carries the
        // per-workspace `attention` frame but it is authoritative on /v1/events.
        case .attention, .presence, .paneAdded, .paneClosed, .unknown:
            break
        }
    }
}

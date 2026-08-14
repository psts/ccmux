import AppKit
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

    init(workspaceId: UUID, daemonId: String, repoPath: String, panes: [DaemonPane], wsOrigin: String? = nil) {
        self.workspaceId = workspaceId
        self.daemonId = daemonId
        self.repoPath = repoPath
        self.attach = DaemonAttachClient(workspaceId: daemonId, wsOrigin: wsOrigin)
        for pane in panes { _ = makeController(paneId: pane.id, workingDirectory: pane.cwd) }
        attach.onEvent = { [weak self] in self?.handle($0) }
        attach.onStateChange = { [weak self] state in
            self?.connectionState = state
            self?.onConnectionState?(state)
        }
    }

    func connect() { attach.connect() }
    func disconnect() { attach.disconnect() }

    /// Called when this attachment — the last owner of every controller, and so of
    /// every terminal view in the workspace — is about to be dropped. See
    /// `NSView.detachFromResponderChain`.
    func detachAllTerminals() {
        for controller in controllers.values {
            controller.terminalView.detachFromResponderChain()
        }
    }

    /// Re-dial now (wake). The socket keeps its controllers and their terminal
    /// buffers: the daemon re-sends hello plus a snapshot per pane, and `handle`
    /// replays our focus on that hello, so nothing on screen blinks.
    func forceReconnect() { attach.forceReconnect() }

    /// Get (or lazily create) the controller backing a pane, for the hosting view.
    func controller(forPane paneId: String, workingDirectory: String) -> RemoteTermController {
        controllers[paneId] ?? makeController(paneId: paneId, workingDirectory: workingDirectory)
    }

    /// Whether this workspace owns the given daemon pane (used for the reverse lookup
    /// a hosted pane view does to find its controller/connection state).
    func hasPane(_ paneId: String) -> Bool { controllers[paneId] != nil }

    /// Last focus frame sent ("" = explicitly unfocused; nil = never reported).
    private(set) var lastReportedFocus: String?
    /// Last presence reported alongside it (nil = never reported).
    private(set) var lastReportedPresent: Bool?

    /// Any pane id of this workspace — the focus frame needs one; nil when empty.
    var anyPaneId: String? { controllers.keys.first }

    /// Report this lens's focus AND this screen's presence to the daemon.
    ///
    /// The two are deliberately separate. Focus says which pane is on display and
    /// clears the flash for it; presence says this screen could show a notification
    /// at all. Collapsing them is what made alerts depend on whether a hosted
    /// workspace happened to be the one in front of you.
    ///
    /// Deduped on the pair, and re-sent after every reconnect because presence is
    /// per-connection on the daemon side.
    func reportFocus(paneId: String, present: Bool) {
        guard paneId != lastReportedFocus || present != lastReportedPresent else { return }
        lastReportedFocus = paneId
        lastReportedPresent = present
        attach.send(.focus(pane: paneId, present: present))
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
        for id in dead {
            // This dictionary owns the controller, and the controller owns the
            // terminal view — so the view dies here. A closed pane is usually the
            // one being typed in, and a window does not retain its first responder.
            controllers[id]?.terminalView.detachFromResponderChain()
            controllers.removeValue(forKey: id)
        }
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
            for p in panes {
                let c = controller(forPane: p.id, workingDirectory: p.cwd)
                if p.cols > 0 {
                    c.setAuthoritativeSize(cols: p.cols, rows: p.rows)
                }
                // Re-drive the pane to what THIS window shows. A reconnect is the
                // same claim as a window becoming key: the screen being looked at
                // owns the size. Without it the daemon keeps whatever it last
                // recorded — after its own restart that can be a phone's width, or
                // nothing at all — and the pane stays drawn at a width this window
                // does not have, with only the "Take over" pill to fix it by hand.
                // Panes that are not on screen are dropped by forwardResize's
                // superview guard, so a background tab never drives the size.
                c.sendCurrentSize()
            }
            // A hello means a (re)connect: the daemon's presence entry for this
            // connection is fresh, so replay what we told the old one.
            //
            // Presence replays even when it is false and even with no focused
            // pane, unlike focus. Staying silent would leave the new entry
            // unreported, which the daemon reads as a lens too old to know — and
            // that falls back to treating a focused pane as presence, the exact
            // behaviour this replaced.
            //
            // Only what we actually observed is replayed. An earlier version
            // invented `present: true` for the focus-without-presence case, which
            // claimed this screen was occupied on no evidence and would have
            // suppressed the owner's phone pushes fleet-wide. reportFocus always
            // sets both, so that case cannot arise; asserting it in a comment
            // beats fabricating a value if it ever does.
            if let present = lastReportedPresent {
                attach.send(.focus(pane: lastReportedFocus ?? "", present: present))
            }
        case .paneSize(let pane, let cols, let rows):
            controller(forPane: pane, workingDirectory: repoPath).setAuthoritativeSize(cols: cols, rows: rows)
        case .clipboard(let pane, let bytes):
            // tmux copy-mode copied in this workspace (selection = copy):
            // mirror it to the Mac clipboard so Cmd+V just works. Scoped by
            // the daemon to this workspace's lenses only. Lossy UTF-8 decode:
            // a copy containing stray invalid bytes should still copy (with
            // replacement chars), never silently vanish.
            let text = String(decoding: bytes, as: UTF8.self)
            guard !text.isEmpty else {
                NSLog("[ccmux clip] dropped empty/undecodable copy from pane \(pane) (\(bytes.count) bytes)")
                break
            }
            DispatchQueue.main.async {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(text, forType: .string)
            }
        // Attention now rides the global firehose; the attach still carries the
        // per-workspace `attention` frame but it is authoritative on /v1/events.
        case .attention, .presence, .paneAdded, .paneClosed, .unknown:
            break
        }
    }
}

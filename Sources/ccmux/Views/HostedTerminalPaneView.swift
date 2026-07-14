import AppKit
import SwiftUI
import SwiftTerm

/// A terminal pane backed by a ccmuxd/tmux session over WebSocket. Embeds the
/// `RemoteTermController`'s SwiftTerm view (owned by `RemoteSessionService`, reused
/// across splits/reconnects) and overlays a status banner while (re)connecting.
struct HostedTerminalPaneView: View {
    /// Daemon pane id (`@ccmux_pane_id`).
    let paneId: String
    let workingDirectory: String
    @ObservedObject private var service = RemoteSessionService.shared

    var body: some View {
        ZStack {
            HostedTerminalContainer(paneId: paneId, workingDirectory: workingDirectory)
            let state = service.hostedConnectionState(paneId: paneId)
            if state != .connected {
                ReconnectOverlay(state: state)
            }
        }
    }
}

/// Non-blocking banner shown over a hosted pane while the attach connection is not
/// live. Kept translucent + top-aligned so any last-rendered content stays visible.
private struct ReconnectOverlay: View {
    let state: DaemonConnectionState

    private var label: String {
        switch state {
        case .connecting: return "Connecting to daemon…"
        case .reconnecting: return "Reconnecting…"
        case .closed: return "Disconnected"
        case .connected: return ""
        }
    }

    var body: some View {
        VStack {
            HStack(spacing: 6) {
                if state == .connecting || state == .reconnecting {
                    ProgressView().controlSize(.small)
                } else {
                    Image(systemName: "bolt.horizontal.circle")
                }
                Text(label).font(.system(size: 11, weight: .medium))
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 5)
            .background(.black.opacity(0.65))
            .foregroundColor(.white)
            .clipShape(Capsule())
            .padding(.top, 8)
            Spacer()
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .allowsHitTesting(false)
    }
}

/// Reparents the pane's persistent SwiftTerm view into a disposable container —
/// the hosted analog of `TerminalContainerView`. No process to spawn; the daemon
/// owns lifecycle, so this only embeds and asserts the on-screen size.
private struct HostedTerminalContainer: NSViewRepresentable {
    let paneId: String
    let workingDirectory: String

    func makeNSView(context: Context) -> HostedTerminalContainerView {
        HostedTerminalContainerView(paneId: paneId, workingDirectory: workingDirectory)
    }

    func updateNSView(_ nsView: HostedTerminalContainerView, context: Context) {
        nsView.ensureEmbedded()
    }
}

final class HostedTerminalContainerView: NSView {
    let paneId: String
    let workingDirectory: String
    private var hasAssertedSize = false

    init(paneId: String, workingDirectory: String) {
        self.paneId = paneId
        self.workingDirectory = workingDirectory
        super.init(frame: .zero)
    }

    required init?(coder: NSCoder) { fatalError() }

    private var controller: RemoteTermController? {
        RemoteSessionService.shared.hostedController(paneId: paneId, workingDirectory: workingDirectory)
    }

    func ensureEmbedded() {
        guard let terminal = controller?.terminalView else { return }
        if terminal.superview !== self {
            terminal.removeFromSuperview()
            addSubview(terminal)
            terminal.translatesAutoresizingMaskIntoConstraints = false
            NSLayoutConstraint.activate([
                terminal.leadingAnchor.constraint(equalTo: leadingAnchor),
                terminal.trailingAnchor.constraint(equalTo: trailingAnchor),
                terminal.topAnchor.constraint(equalTo: topAnchor),
                terminal.bottomAnchor.constraint(equalTo: bottomAnchor),
            ])
            hasAssertedSize = false
        }
        assertSizeIfReady()
    }

    override func layout() {
        super.layout()
        assertSizeIfReady()
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        if window != nil { ensureEmbedded() }
    }

    /// Once embedded at a real size, push the grid dimensions to the daemon so tmux
    /// (window-size manual) matches what the user sees and re-seeds at the right size.
    /// The ongoing case (drag-resize) is handled by RemoteTermController.sizeChanged.
    private func assertSizeIfReady() {
        guard !hasAssertedSize, bounds.width > 0, bounds.height > 0,
              let controller, controller.terminalView.superview === self else { return }
        hasAssertedSize = true
        controller.sendCurrentSize()
    }
}

import AppKit
import SwiftUI
import SwiftTerm

/// A terminal pane backed by a ccmuxd/tmux session over WebSocket. Embeds the
/// `RemoteTermController`'s SwiftTerm view (owned by `RemoteSessionService`, reused
/// across splits/reconnects) and overlays a status banner while (re)connecting.
struct HostedTerminalPaneView: View {
    /// TerminalConfig.id of the tab this pane is shown in — what PaneFocusCoordinator
    /// addresses. Distinct from `paneId`, which the daemon owns.
    let tabId: UUID
    /// Daemon pane id (`@ccmux_pane_id`).
    let paneId: String
    let workingDirectory: String
    @ObservedObject private var service = RemoteSessionService.shared

    var body: some View {
        ZStack {
            HostedTerminalContainer(tabId: tabId, paneId: paneId, workingDirectory: workingDirectory)
            let state = service.hostedConnectionState(paneId: paneId)
            if state != .connected {
                ReconnectOverlay(state: state)
            }
        }
        // "Take over" when another lens drove the shared pane to a size this window
        // can't show 1:1. Sized to the button (top-trailing), so terminal clicks
        // elsewhere pass through. Tapping re-asserts this window's size.
        .overlay(alignment: .topTrailing) {
            if service.hostedConnectionState(paneId: paneId) == .connected,
               service.hostedIsStale(paneId: paneId) {
                TakeoverButton {
                    let c = service.hostedController(paneId: paneId, workingDirectory: workingDirectory)
                    c?.sendCurrentSize()
                    c?.requestRepaint()
                }
                .padding(8)
            }
        }
    }
}

/// Amber pill that reclaims the shared pane's size for this window.
private struct TakeoverButton: View {
    let action: () -> Void
    var body: some View {
        Button(action: action) {
            HStack(spacing: 4) {
                Image(systemName: "arrow.left.and.right")
                Text("Take over").font(.system(size: 11, weight: .semibold))
            }
            .padding(.horizontal, 10)
            .padding(.vertical, 5)
            .background(Color(red: 0.88, green: 0.63, blue: 0.29))
            .foregroundColor(Color(red: 0.13, green: 0.09, blue: 0.01))
            .clipShape(Capsule())
        }
        .buttonStyle(.plain)
        .help("Resize this session to fit this window")
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
    let tabId: UUID
    let paneId: String
    let workingDirectory: String

    func makeNSView(context: Context) -> HostedTerminalContainerView {
        HostedTerminalContainerView(tabId: tabId, paneId: paneId, workingDirectory: workingDirectory)
    }

    func updateNSView(_ nsView: HostedTerminalContainerView, context: Context) {
        nsView.ensureEmbedded()
    }
}

final class HostedTerminalContainerView: NSView {
    let tabId: UUID
    let paneId: String
    let workingDirectory: String
    private var hasAssertedSize = false
    private var keyObserver: NSObjectProtocol?
    private var focusClaim: PaneFocusClaim?

    init(tabId: UUID, paneId: String, workingDirectory: String) {
        self.tabId = tabId
        self.paneId = paneId
        self.workingDirectory = workingDirectory
        super.init(frame: .zero)
        focusClaim = PaneFocusClaim(tabId: tabId, host: self) { [weak self] in
            self?.controller?.terminalView
        }
    }

    required init?(coder: NSCoder) { fatalError() }

    deinit {
        if let keyObserver { NotificationCenter.default.removeObserver(keyObserver) }
    }

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
        // A tab switch rebuilds this container, so the click that asked for focus
        // landed before the terminal existed here. Claim it now that it does.
        focusClaim?.claimIfRequested()
    }

    override func layout() {
        super.layout()
        assertSizeIfReady()
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        if let keyObserver { NotificationCenter.default.removeObserver(keyObserver); self.keyObserver = nil }
        guard let window else { return }
        ensureEmbedded()
        // Re-assert this pane's size when its window becomes key. Another lens (a
        // phone) may have driven the shared tmux pane narrow while this window was
        // in the background; returning to it must un-crush the pane to what this
        // window shows. sendCurrentSize() forces the resend even though the Mac
        // view's own grid never changed. (The live drag-resize case stays with
        // RemoteTermController.sizeChanged.)
        keyObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.didBecomeKeyNotification, object: window, queue: .main
        ) { [weak self] _ in
            self?.reassertSizeOnFocus()
        }
    }

    /// Force-push this on-screen pane's size to the daemon (window became key).
    /// The repaint rides along because the size usually did NOT change: without
    /// it the daemon sends nothing and the pane keeps whatever this emulator
    /// drifted to while the window was in the background.
    private func reassertSizeOnFocus() {
        guard bounds.width > 0, bounds.height > 0,
              let controller, controller.terminalView.superview === self else { return }
        controller.sendCurrentSize()
        controller.requestRepaint()
    }

    /// Once embedded at a real size, push the grid dimensions to the daemon so tmux
    /// (window-size manual) matches what the user sees and re-seeds at the right size.
    /// The ongoing case (drag-resize) is handled by RemoteTermController.sizeChanged.
    /// The repaint covers the size-unchanged case, which is the common one: a tab
    /// or workspace switch rebuilds this container around a reused emulator whose
    /// grid matches the pane exactly, so the size assert alone repaints nothing.
    private func assertSizeIfReady() {
        guard !hasAssertedSize, bounds.width > 0, bounds.height > 0,
              let controller, controller.terminalView.superview === self else { return }
        hasAssertedSize = true
        controller.sendCurrentSize()
        controller.requestRepaint()
    }
}

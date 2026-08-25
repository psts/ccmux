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
    /// Transient, like the web lens's bar dismiss: a rebuilt view offers again.
    @State private var harnessBarDismissed = false

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
                    service.hostedController(paneId: paneId, workingDirectory: workingDirectory)?
                        .reassertOnActivation()
                }
                .padding(8)
            }
        }
        // The harness bar: a live shell pane (fresh or dormant, never the dev
        // server) offers what to start in place — the Mac analog of the web
        // lens's bar. Sized to its pill, so terminal clicks elsewhere pass.
        .overlay(alignment: .bottom) {
            if !harnessBarDismissed,
               service.hostedConnectionState(paneId: paneId) == .connected,
               let offer = service.harnessOffer(forPane: paneId) {
                HarnessBar(
                    harnesses: offer.harnesses, suggested: offer.suggested, restart: offer.restart,
                    onStart: { name in
                        Task { _ = await service.startHarness(paneId: paneId, name: name) }
                    },
                    onDismiss: { harnessBarDismissed = true })
                .padding(.bottom, 8)
            }
        }
    }
}

/// "Start here:" pill over a bare-shell pane: one button per harness, the
/// suggested one (folder rule, or the dormant pane's own harness) highlighted.
private struct HarnessBar: View {
    let harnesses: [DaemonHarness]
    let suggested: String
    let restart: Bool
    let onStart: (String) -> Void
    let onDismiss: () -> Void

    var body: some View {
        HStack(spacing: 6) {
            Text(restart ? "Restart:" : "Start here:")
                .font(.system(size: 11, weight: .medium))
                .foregroundColor(.white.opacity(0.75))
            ForEach(harnesses) { h in
                harnessButton(h)
            }
            Button(action: onDismiss) {
                Image(systemName: "xmark")
                    .font(.system(size: 9, weight: .bold))
                    .foregroundColor(.white.opacity(0.6))
            }
            .buttonStyle(.plain)
            .help("Keep the shell")
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 5)
        .background(.black.opacity(0.65))
        .clipShape(Capsule())
    }

    private func harnessButton(_ h: DaemonHarness) -> some View {
        let isSuggested = h.name == suggested
        return Button {
            onStart(h.name)
        } label: {
            Text("\([h.icon ?? "", h.name].filter { !$0.isEmpty }.joined(separator: " "))")
                .font(.system(size: 11, weight: isSuggested ? .semibold : .regular))
                .padding(.horizontal, 8)
                .padding(.vertical, 3)
                .background(Color.white.opacity(isSuggested ? 0.22 : 0.10))
                .foregroundColor(.white)
                .clipShape(Capsule())
                .overlay(Capsule().strokeBorder(
                    isSuggested ? Color.accentColor : .clear, lineWidth: 1))
        }
        .buttonStyle(.plain)
        .help(h.command ?? "")
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

    /// Window became key: activate the on-screen pane (size re-assert + repaint,
    /// see RemoteTermController.reassertOnActivation).
    private func reassertSizeOnFocus() {
        guard bounds.width > 0, bounds.height > 0,
              let controller, controller.terminalView.superview === self else { return }
        controller.reassertOnActivation()
    }

    /// Once embedded at a real size, activate the pane: push the grid to the daemon
    /// so tmux (window-size manual) matches what the user sees, and repaint for the
    /// common case where the size is unchanged (a tab or workspace switch rebuilds
    /// this container around a reused emulator whose grid already matches). The
    /// ongoing case (drag-resize) stays with RemoteTermController.sizeChanged.
    private func assertSizeIfReady() {
        guard !hasAssertedSize, bounds.width > 0, bounds.height > 0,
              let controller, controller.terminalView.superview === self else { return }
        hasAssertedSize = true
        controller.reassertOnActivation()
    }
}

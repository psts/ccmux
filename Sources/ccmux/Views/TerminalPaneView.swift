import AppKit
import SwiftUI
import SwiftTerm

/// Embeds a terminal in a disposable container NSView.
/// When SwiftUI recreates this view (e.g. on split), the terminal is just reparented
/// into the new container — the shell process keeps running.
struct TerminalPaneView: NSViewRepresentable {
    let terminalId: UUID
    let workingDirectory: String
    var startupCommand: String?

    func makeNSView(context: Context) -> TerminalContainerView {
        TerminalContainerView(terminalId: terminalId, workingDirectory: workingDirectory, startupCommand: startupCommand)
    }

    func updateNSView(_ nsView: TerminalContainerView, context: Context) {
        nsView.ensureTerminalEmbedded()
    }

    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    class Coordinator: NSObject {}
}

/// An NSView that hosts a terminal and re-embeds it on layout.
class TerminalContainerView: NSView {
    let terminalId: UUID
    let workingDirectory: String
    let startupCommand: String?
    private var hasLaidOut = false
    private var focusObserver: NSObjectProtocol?

    init(terminalId: UUID, workingDirectory: String, startupCommand: String? = nil) {
        self.terminalId = terminalId
        self.workingDirectory = workingDirectory
        self.startupCommand = startupCommand
        super.init(frame: .zero)
        // A tab click on an already-embedded terminal never re-enters embed/layout,
        // so the broadcast is the only way that case learns it should take focus.
        focusObserver = NotificationCenter.default.addObserver(
            forName: PaneFocusCoordinator.didRequestFocus, object: nil, queue: .main
        ) { [weak self] note in
            guard let self, note.object as? UUID == self.terminalId else { return }
            self.takeKeyboardFocusIfRequested()
        }
    }

    required init?(coder: NSCoder) { fatalError() }

    deinit {
        if let focusObserver { NotificationCenter.default.removeObserver(focusObserver) }
    }

    func ensureTerminalEmbedded() {
        // Always pass startupCommand: terminal(for:) only queues it on first creation,
        // so whichever call (this, viewDidMoveToWindow, or layout) creates the terminal
        // must carry it — otherwise the idempotent guard drops the command for a pane
        // added to an already-laid-out workspace (e.g. a spawned teammate pane).
        let terminal = TerminalStore.shared.terminal(for: terminalId, workingDirectory: workingDirectory, startupCommand: startupCommand)
        if terminal.superview !== self {
            terminal.removeFromSuperview()
            addSubview(terminal)
            // Use constraints so the terminal fills the container regardless of initial bounds
            terminal.translatesAutoresizingMaskIntoConstraints = false
            NSLayoutConstraint.activate([
                terminal.leadingAnchor.constraint(equalTo: leadingAnchor),
                terminal.trailingAnchor.constraint(equalTo: trailingAnchor),
                terminal.topAnchor.constraint(equalTo: topAnchor),
                terminal.bottomAnchor.constraint(equalTo: bottomAnchor),
            ])
            // Mark that we need to send SIGWINCH once we get a real size
            hasLaidOut = false
        }
        // Embedding may complete after the container's first layout() pass, so try
        // firing here too — whichever of embed/layout finishes last wins.
        fireStartupIfReady()
        // A tab switch rebuilds this container, so the click that asked for focus
        // landed before the terminal existed here. Claim it now that it does.
        takeKeyboardFocusIfRequested()
    }

    /// Make the terminal first responder if a tab click asked for it, so selecting a
    /// tab is enough to type. Claiming comes last and is one-shot: a re-embed that
    /// nobody asked for must not pull focus out of whatever the user is using.
    private func takeKeyboardFocusIfRequested() {
        guard let window else { return }
        let terminal = TerminalStore.shared.terminal(for: terminalId, workingDirectory: workingDirectory, startupCommand: startupCommand)
        guard terminal.superview === self else { return }
        guard PaneFocusCoordinator.shared.claim(tabId: terminalId) else { return }
        window.makeFirstResponder(terminal)
    }

    override func layout() {
        super.layout()
        fireStartupIfReady()
    }

    /// Send the queued startup command once the terminal is BOTH embedded and has a
    /// real size — order-independent across layout()/embed. `hasLaidOut` is only set
    /// when we actually fire, so an early layout() (before embedding) can't consume
    /// the one-shot and leave the command stranded.
    private func fireStartupIfReady() {
        guard !hasLaidOut, bounds.width > 0, bounds.height > 0 else { return }
        let terminal = TerminalStore.shared.terminal(for: terminalId, workingDirectory: workingDirectory, startupCommand: startupCommand)
        guard terminal.superview === self else { return }
        hasLaidOut = true
        // Send SIGWINCH to tell the shell the window size changed — forces full redraw.
        let pid = terminal.process?.shellPid ?? 0
        if pid > 0 {
            kill(pid, SIGWINCH)
        }
        // PTY now has correct cols/rows; safe to launch the user's TUI so it never
        // observes the 800x480 fallback frame.
        TerminalStore.shared.runStartupCommandIfPending(paneId: terminalId)
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        // Embed when we're actually in a window
        if window != nil {
            ensureTerminalEmbedded()
        }
    }
}

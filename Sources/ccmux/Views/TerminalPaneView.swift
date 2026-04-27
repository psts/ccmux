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

    init(terminalId: UUID, workingDirectory: String, startupCommand: String? = nil) {
        self.terminalId = terminalId
        self.workingDirectory = workingDirectory
        self.startupCommand = startupCommand
        super.init(frame: .zero)
    }

    required init?(coder: NSCoder) { fatalError() }

    func ensureTerminalEmbedded() {
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
    }

    override func layout() {
        super.layout()
        // After we get a real layout, force the terminal to redraw by sending SIGWINCH
        if !hasLaidOut && bounds.width > 0 && bounds.height > 0 {
            hasLaidOut = true
            let terminal = TerminalStore.shared.terminal(for: terminalId, workingDirectory: workingDirectory)
            if terminal.superview === self {
                // Send SIGWINCH to tell the shell the window size changed — forces full redraw
                let pid = terminal.process?.shellPid ?? 0
                if pid > 0 {
                    kill(pid, SIGWINCH)
                }
                // PTY now has correct cols/rows; safe to launch the user's TUI so
                // it never observes the 800x480 fallback frame.
                TerminalStore.shared.runStartupCommandIfPending(paneId: terminalId)
            }
        }
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        // Embed when we're actually in a window
        if window != nil {
            ensureTerminalEmbedded()
        }
    }
}

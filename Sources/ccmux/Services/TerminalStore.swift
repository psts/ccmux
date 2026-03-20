import AppKit
import SwiftTerm

/// Manages terminal view instances outside of SwiftUI's lifecycle.
/// Terminals survive view tree changes (e.g. splits) by being keyed to pane IDs.
class TerminalStore {
    static let shared = TerminalStore()

    private var terminals: [UUID: LocalProcessTerminalView] = [:]
    private var keyMonitors: [UUID: Any] = [:]

    private init() {}

    /// Get or create a terminal for the given pane ID.
    func terminal(for paneId: UUID, workingDirectory: String) -> LocalProcessTerminalView {
        if let existing = terminals[paneId] {
            return existing
        }

        let terminal = LocalProcessTerminalView(frame: .zero)

        // Appearance
        terminal.font = NSFont.monospacedSystemFont(ofSize: 13, weight: .regular)
        terminal.nativeForegroundColor = NSColor(white: 0.85, alpha: 1.0)
        terminal.nativeBackgroundColor = NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)

        // Determine shell
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let shellName = "-" + (shell as NSString).lastPathComponent

        // Build environment
        var env = ProcessInfo.processInfo.environment
        env["TERM"] = "xterm-256color"
        env["COLORTERM"] = "truecolor"
        env["TERM_PROGRAM"] = "Apple_Terminal"
        env["TERM_PROGRAM_VERSION"] = "450"
        env["LANG"] = env["LANG"] ?? "en_US.UTF-8"
        let envArray = env.map { "\($0.key)=\($0.value)" }

        terminal.startProcess(
            executable: shell,
            args: [],
            environment: envArray,
            execName: shellName,
            currentDirectory: workingDirectory
        )

        // Install Shift+Enter key monitor
        let monitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak terminal] event in
            guard let tv = terminal else { return event }
            if event.keyCode == 36 && event.modifierFlags.contains(.shift) {
                if let firstResponder = tv.window?.firstResponder as? NSView,
                   firstResponder === tv || firstResponder.isDescendant(of: tv) {
                    tv.getTerminal().sendResponse(text: "\u{1b}[13;2u")
                    return nil
                }
            }
            return event
        }

        terminals[paneId] = terminal
        if let monitor { keyMonitors[paneId] = monitor }

        return terminal
    }

    /// Remove a terminal when its pane is closed.
    func remove(paneId: UUID) {
        if let monitor = keyMonitors.removeValue(forKey: paneId) {
            NSEvent.removeMonitor(monitor)
        }
        if let terminal = terminals.removeValue(forKey: paneId) {
            terminal.terminate()
        }
    }
}

import AppKit
import SwiftTerm

/// Manages terminal view instances outside of SwiftUI's lifecycle.
/// Terminals survive view tree changes (e.g. splits) by being keyed to pane IDs.
class TerminalStore {
    static let shared = TerminalStore()

    private var terminals: [UUID: LocalProcessTerminalView] = [:]
    private var keyMonitors: [UUID: Any] = [:]
    private var linkDelegates: [UUID: TerminalLinkInterceptor] = [:]
    fileprivate var workingDirs: [UUID: String] = [:]

    /// Callback for when a file link is clicked in a terminal.
    /// Set by WorkspaceManager to route file opens to the right controller.
    var onFileLinkClicked: ((UUID, String) -> Void)?

    private init() {}

    /// Get or create a terminal for the given pane ID.
    func terminal(for paneId: UUID, workingDirectory: String) -> LocalProcessTerminalView {
        if let existing = terminals[paneId] {
            return existing
        }

        let terminal = LocalProcessTerminalView(frame: .zero)

        // Appearance
        terminal.font = NSFont(name: "Monaco", size: 12) ?? NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        terminal.nativeForegroundColor = NSColor(white: 0.85, alpha: 1.0)
        terminal.nativeBackgroundColor = NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)

        // Use Option for character input (e.g., @ { } [ ] on international keyboards)
        // instead of treating it as Meta key
        terminal.optionAsMetaKey = false

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

        // Wrap the terminal's self-delegate to intercept link clicks
        // Must happen AFTER startProcess so the terminal is fully set up
        let interceptor = TerminalLinkInterceptor(
            original: terminal,  // LocalProcessTerminalView is its own delegate
            paneId: paneId,
            workingDirectory: workingDirectory
        )
        terminal.terminalDelegate = interceptor
        linkDelegates[paneId] = interceptor

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
        workingDirs[paneId] = workingDirectory
        if let monitor { keyMonitors[paneId] = monitor }

        return terminal
    }

    /// Remove a terminal when its pane is closed.
    func remove(paneId: UUID) {
        if let monitor = keyMonitors.removeValue(forKey: paneId) {
            NSEvent.removeMonitor(monitor)
        }
        workingDirs.removeValue(forKey: paneId)
        linkDelegates.removeValue(forKey: paneId)
        if let terminal = terminals.removeValue(forKey: paneId) {
            terminal.terminalDelegate = nil
            terminal.terminate()
        }
    }

}

// MARK: - Link Interceptor Delegate

/// Wraps LocalProcessTerminalView's self-delegation to intercept requestOpenLink
/// while forwarding all other delegate calls to the original (the terminal itself).
class TerminalLinkInterceptor: NSObject, TerminalViewDelegate {
    /// The original delegate (LocalProcessTerminalView, which is its own delegate)
    weak var original: LocalProcessTerminalView?
    let paneId: UUID
    let workingDirectory: String

    init(original: LocalProcessTerminalView, paneId: UUID, workingDirectory: String) {
        self.original = original
        self.paneId = paneId
        self.workingDirectory = workingDirectory
    }

    // MARK: - Intercepted method

    func requestOpenLink(source: TerminalView, link: String, params: [String: String]) {
        // URLs → open in browser (default behavior)
        if link.hasPrefix("http://") || link.hasPrefix("https://") {
            if let url = URL(string: link) {
                NSWorkspace.shared.open(url)
            }
            return
        }

        var filePath = link
        if filePath.hasPrefix("file://") {
            filePath = String(filePath.dropFirst(7))
            filePath = filePath.removingPercentEncoding ?? filePath
        }

        // Strip line:column suffix
        let components = filePath.split(separator: ":", maxSplits: 2)
        let cleanPath = String(components[0])

        // Resolve path
        let absolutePath = cleanPath.hasPrefix("/") ? cleanPath :
            (workingDirectory as NSString).appendingPathComponent(cleanPath)

        // If file exists, open in File Explorer
        if FileManager.default.fileExists(atPath: absolutePath) {
            let relativePath: String
            if cleanPath.hasPrefix(workingDirectory + "/") {
                relativePath = String(cleanPath.dropFirst(workingDirectory.count + 1))
            } else if cleanPath.hasPrefix("/") {
                relativePath = cleanPath
            } else {
                relativePath = cleanPath
            }
            TerminalStore.shared.onFileLinkClicked?(paneId, relativePath)
        } else {
            // Fall back to system open
            if let url = URL(string: link) {
                NSWorkspace.shared.open(url)
            }
        }
    }

    // MARK: - Forwarded methods (critical: send must reach the process)

    func send(source: TerminalView, data: ArraySlice<UInt8>) {
        original?.send(source: source, data: data)
    }

    func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) {
        original?.sizeChanged(source: source, newCols: newCols, newRows: newRows)
    }

    func setTerminalTitle(source: TerminalView, title: String) {
        original?.setTerminalTitle(source: source, title: title)
    }

    func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {
        original?.hostCurrentDirectoryUpdate(source: source, directory: directory)
    }

    func scrolled(source: TerminalView, position: Double) {
        original?.scrolled(source: source, position: position)
    }

    func bell(source: TerminalView) {
        original?.bell(source: source)
    }

    func clipboardCopy(source: TerminalView, content: Data) {
        original?.clipboardCopy(source: source, content: content)
    }

    func iTermContent(source: TerminalView, content: ArraySlice<UInt8>) {
        original?.iTermContent(source: source, content: content)
    }

    func rangeChanged(source: TerminalView, startY: Int, endY: Int) {
        original?.rangeChanged(source: source, startY: startY, endY: endY)
    }
}

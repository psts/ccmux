import AppKit
import SwiftTerm
import Darwin

/// Manages terminal view instances outside of SwiftUI's lifecycle.
/// Terminals survive view tree changes (e.g. splits) by being keyed to pane IDs.
class TerminalStore {
    static let shared = TerminalStore()

    private var terminals: [UUID: LocalProcessTerminalView] = [:]
    private var linkDelegates: [UUID: TerminalLinkInterceptor] = [:]
    fileprivate var workingDirs: [UUID: String] = [:]
    private var zdotdirPaths: [UUID: String] = [:]

    /// Callback for when a file link is clicked in a terminal.
    /// Set by WorkspaceManager to route file opens to the right controller.
    var onFileLinkClicked: ((UUID, String) -> Void)?

    private init() {}

    private func cmdFilePath(for paneId: UUID) -> String {
        "/tmp/ccmux-cmd-\(paneId.uuidString)"
    }

    // MARK: - Command Detection (native APIs — no process spawning)

    /// Detect the command currently running in a terminal's shell.
    /// Returns nil if the shell has no child processes (idle prompt).
    /// Prefers the preexec-captured command (preserves alias names) over sysctl fallback.
    func detectRunningCommand(for paneId: UUID) -> String? {
        guard let terminal = terminals[paneId],
              let shellPid = terminal.process?.shellPid,
              shellPid > 0 else { return nil }

        // Check for child processes using native libproc (no pgrep spawning)
        var childBuf = [pid_t](repeating: 0, count: 64)
        let actual = proc_listpids(6 /* PROC_PPID_ONLY */, UInt32(shellPid), &childBuf,
                                   Int32(childBuf.count * MemoryLayout<pid_t>.size))
        let childCount = Int(actual) / MemoryLayout<pid_t>.size
        guard childCount > 0, childBuf[0] > 0 else { return nil }

        // Prefer preexec-captured command (preserves aliases/functions as typed)
        let cmdFile = cmdFilePath(for: paneId)
        if let content = try? String(contentsOfFile: cmdFile, encoding: .utf8) {
            let command = content.trimmingCharacters(in: .whitespacesAndNewlines)
            if !command.isEmpty { return command }
        }

        // Fallback: get child process command line via sysctl (no ps spawning)
        return getProcessCommandLine(pid: childBuf[0])
    }

    /// Get the full command line (argv joined) of a process using sysctl KERN_PROCARGS2.
    private func getProcessCommandLine(pid: pid_t) -> String? {
        var mib: [Int32] = [CTL_KERN, 49 /* KERN_PROCARGS2 */, Int32(pid)]
        var size: Int = 0
        guard sysctl(&mib, 3, nil, &size, nil, 0) == 0, size > 0 else { return nil }

        var buffer = [UInt8](repeating: 0, count: size)
        guard sysctl(&mib, 3, &buffer, &size, nil, 0) == 0 else { return nil }
        guard size >= MemoryLayout<Int32>.size else { return nil }

        let argc = buffer.withUnsafeBytes { $0.load(as: Int32.self) }
        var offset = MemoryLayout<Int32>.size

        // Skip exec_path (null-terminated) + padding nulls
        while offset < size && buffer[offset] != 0 { offset += 1 }
        while offset < size && buffer[offset] == 0 { offset += 1 }

        // Read argv strings
        var args: [String] = []
        for _ in 0..<argc {
            guard offset < size else { break }
            var end = offset
            while end < size && buffer[end] != 0 { end += 1 }
            if end > offset, let arg = String(bytes: buffer[offset..<end], encoding: .utf8) {
                args.append(arg)
            }
            offset = end + 1
        }

        return args.isEmpty ? nil : args.joined(separator: " ")
    }

    // MARK: - ZDOTDIR Setup (invisible preexec hook injection)

    /// Create a temporary ZDOTDIR with proxy startup files that source the user's
    /// config then inject a preexec hook to capture typed commands (including aliases).
    private func setupZdotdir(at path: String) {
        let fm = FileManager.default
        try? fm.createDirectory(atPath: path, withIntermediateDirectories: true)

        try? "[[ -f \"$HOME/.zshenv\" ]] && source \"$HOME/.zshenv\"\n"
            .write(toFile: "\(path)/.zshenv", atomically: true, encoding: .utf8)

        try? "[[ -f \"$HOME/.zprofile\" ]] && source \"$HOME/.zprofile\"\n"
            .write(toFile: "\(path)/.zprofile", atomically: true, encoding: .utf8)

        let zshrc = """
        ZDOTDIR="$HOME"
        [[ -f "$HOME/.zshrc" ]] && source "$HOME/.zshrc"
        if [[ -n "$CCMUX_CMD_FILE" ]]; then
            __ccmux_preexec() { print -r -- "$1" > "$CCMUX_CMD_FILE" }
            preexec_functions+=(__ccmux_preexec)
        fi
        __ccmux_reset_keyboard_protocol() { printf '\\e[<99u' }
        precmd_functions+=(__ccmux_reset_keyboard_protocol)

        """
        try? zshrc.write(toFile: "\(path)/.zshrc", atomically: true, encoding: .utf8)

        try? "[[ -f \"$HOME/.zlogin\" ]] && source \"$HOME/.zlogin\"\n"
            .write(toFile: "\(path)/.zlogin", atomically: true, encoding: .utf8)
    }

    // MARK: - Terminal Lifecycle

    /// Get or create a terminal for the given pane ID.
    func terminal(for paneId: UUID, workingDirectory: String, startupCommand: String? = nil) -> LocalProcessTerminalView {
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
        env["TERM_PROGRAM"] = "iTerm.app"
        env["LANG"] = env["LANG"] ?? "en_US.UTF-8"
        env["CCMUX_CMD_FILE"] = cmdFilePath(for: paneId)

        // Set up ZDOTDIR for invisible preexec hook injection (zsh only)
        if shell.hasSuffix("/zsh") {
            let zdotdir = "/tmp/ccmux-zsh-\(paneId.uuidString)"
            setupZdotdir(at: zdotdir)
            env["ZDOTDIR"] = zdotdir
            zdotdirPaths[paneId] = zdotdir
        }

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

        terminals[paneId] = terminal
        workingDirs[paneId] = workingDirectory

        // Replay startup command after shell initializes
        if let command = startupCommand, !command.isEmpty {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
                terminal.send(Array((command + "\r").utf8))
            }
        }

        return terminal
    }

    /// Remove a terminal when its pane is closed.
    func remove(paneId: UUID) {
        workingDirs.removeValue(forKey: paneId)
        linkDelegates.removeValue(forKey: paneId)
        // Clean up temp files
        try? FileManager.default.removeItem(atPath: cmdFilePath(for: paneId))
        if let zdotdir = zdotdirPaths.removeValue(forKey: paneId) {
            try? FileManager.default.removeItem(atPath: zdotdir)
        }
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

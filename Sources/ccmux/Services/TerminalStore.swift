import AppKit
import SwiftTerm
import Darwin

/// Terminal view that accepts first-mouse clicks so panes activate without
/// requiring a separate click to make the window key first.
class ClickThroughTerminalView: LocalProcessTerminalView {
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}

/// Manages terminal view instances outside of SwiftUI's lifecycle.
/// Terminals survive view tree changes (e.g. splits) by being keyed to pane IDs.
class TerminalStore {
    static let shared = TerminalStore()

    private static let claudeScrollbackLines = 5000

    private var terminals: [UUID: LocalProcessTerminalView] = [:]
    private var linkDelegates: [UUID: TerminalLinkInterceptor] = [:]
    fileprivate var workingDirs: [UUID: String] = [:]
    private var zdotdirPaths: [UUID: String] = [:]
    private var pendingStartupCommands: [UUID: String] = [:]
    private var scrollbackBumpedPanes: Set<UUID> = []
    /// Panes whose currently-running `claude --dangerously-load-development-channels` has
    /// already had its startup prompts auto-confirmed. Cleared by the scan once that claude
    /// exits, so a later manual re-launch in the same pane is auto-confirmed again.
    private var handledDevChannelPanes: Set<UUID> = []
    private var claudeDetectionTimer: DispatchSourceTimer?

    /// Callback for when a file link is clicked in a terminal.
    /// Set by WorkspaceManager to route file opens to the right controller.
    var onFileLinkClicked: ((UUID, String) -> Void)?

    private init() {
        sweepOrphanedTempFiles()
    }

    private func cmdFilePath(for paneId: UUID) -> String {
        "/tmp/ccmux-cmd-\(paneId.uuidString)"
    }

    /// Remove leftover per-pane ZDOTDIR dirs and cmd files from previous runs
    /// that crashed or were killed before `terminateAll` could clean them up.
    /// Safe before any panes exist this session — every path is keyed by a
    /// per-pane UUID, so anything present belongs to a process that's gone.
    private func sweepOrphanedTempFiles() {
        let fm = FileManager.default
        guard let entries = try? fm.contentsOfDirectory(atPath: "/tmp") else { return }
        for entry in entries where entry.hasPrefix("ccmux-zsh-") || entry.hasPrefix("ccmux-cmd-") {
            try? fm.removeItem(atPath: "/tmp/\(entry)")
        }
    }

    // MARK: - Command Detection (native APIs — no process spawning)

    /// Detect the command currently running in a terminal's shell.
    /// Returns nil if the shell has no child processes (idle prompt).
    /// Prefers the preexec-captured command (preserves alias names) over sysctl fallback.
    func detectRunningCommand(for paneId: UUID) -> String? {
        guard let childPid = firstChildPid(for: paneId) else { return nil }

        // Prefer preexec-captured command (preserves aliases/functions as typed)
        let cmdFile = cmdFilePath(for: paneId)
        if let content = try? String(contentsOfFile: cmdFile, encoding: .utf8) {
            let command = content.trimmingCharacters(in: .whitespacesAndNewlines)
            if !command.isEmpty { return command }
        }

        // Fallback: get child process command line via sysctl (no ps spawning)
        return getProcessCommandLine(pid: childPid)
    }

    /// The actual argv (sysctl KERN_PROCARGS2) of the shell's foreground child, ignoring the
    /// preexec-captured typed line. Returns nil when the shell is idle. Needed for flag
    /// detection that must see THROUGH shell aliases/functions: `claude_channels` execs
    /// `claude --dangerously-load-development-channels …`, but the typed alias name preexec
    /// records doesn't contain the flag — the kernel argv always does.
    func runningChildArgv(for paneId: UUID) -> String? {
        guard let childPid = firstChildPid(for: paneId) else { return nil }
        return getProcessCommandLine(pid: childPid)
    }

    /// First direct child PID of the pane's shell (native libproc, no process spawning),
    /// or nil if the shell is idle (sitting at the prompt).
    private func firstChildPid(for paneId: UUID) -> pid_t? {
        guard let terminal = terminals[paneId],
              let shellPid = terminal.process?.shellPid,
              shellPid > 0 else { return nil }
        var childBuf = [pid_t](repeating: 0, count: 64)
        let actual = proc_listpids(6 /* PROC_PPID_ONLY */, UInt32(shellPid), &childBuf,
                                   Int32(childBuf.count * MemoryLayout<pid_t>.size))
        let childCount = Int(actual) / MemoryLayout<pid_t>.size
        guard childCount > 0, childBuf[0] > 0 else { return nil }
        return childBuf[0]
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

        // Start at a plausible 80x24 size so SwiftTerm's buffer and the kernel PTY
        // are initialized at sensible dimensions even for panes that are pre-created
        // off-screen. Without this, bounds.size is .zero, cols floors at MINIMUM_COLS=2,
        // and any output that arrives before the first real layout is hard-wrapped to
        // 2 columns — Buffer.resize doesn't reflow already-wrapped lines.
        let terminal = ClickThroughTerminalView(frame: NSRect(x: 0, y: 0, width: 800, height: 480))

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
        env["TERM_PROGRAM"] = "WezTerm"
        env["TERM_PROGRAM_VERSION"] = "20240203-110809-5046fc22"
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

        if let command = startupCommand,
           let firstToken = command.split(separator: " ").first,
           firstToken.hasPrefix("claude") {
            terminal.getTerminal().changeScrollback(Self.claudeScrollbackLines)
            scrollbackBumpedPanes.insert(paneId)
        }

        // Queue startup command for the first real layout. Sending it now would
        // launch the user's TUI (claude, vim, etc.) into the 800x480 fallback
        // buffer; whatever it draws there gets hard-wrapped at ~100 cols and
        // SwiftTerm's Buffer.resize doesn't reflow already-wrapped content.
        if let command = startupCommand, !command.isEmpty {
            pendingStartupCommands[paneId] = command
        }

        startClaudeDetectionTimerIfNeeded()

        return terminal
    }

    // MARK: - Live Claude Detection (per-pane scrollback bump)

    /// One shared timer that walks all live panes every 5s and bumps scrollback
    /// to 5000 for any pane currently hosting `claude`. Once a pane is bumped
    /// it stays bumped for the pane's lifetime — claude renders into the main
    /// buffer (no alt-screen) so users want full history even after `/exit`.
    private func startClaudeDetectionTimerIfNeeded() {
        guard claudeDetectionTimer == nil else { return }
        let timer = DispatchSource.makeTimerSource(queue: .global(qos: .utility))
        timer.schedule(deadline: .now() + 3, repeating: 5)
        timer.setEventHandler { [weak self] in
            DispatchQueue.main.async { self?.scanPanesForClaude() }
        }
        timer.resume()
        claudeDetectionTimer = timer
    }

    private func scanPanesForClaude() {
        // libproc/sysctl only (no process spawning), every 5s. The scrollback bump keys off the
        // display command (preexec-captured, so it reads naturally); the dev-channels auto-confirm
        // keys off the REAL child argv so it fires through aliases/functions like `claude_channels`.
        for paneId in terminals.keys {
            bumpScrollbackIfClaudeRunning(paneId: paneId, command: detectRunningCommand(for: paneId))
            autoConfirmDevChannelsIfRunning(paneId: paneId, argv: runningChildArgv(for: paneId))
        }
    }

    private func bumpScrollbackIfClaudeRunning(paneId: UUID, command: String?) {
        guard !scrollbackBumpedPanes.contains(paneId) else { return }
        guard let terminal = terminals[paneId] else { return }
        guard let command, let firstToken = command.split(separator: " ").first,
              firstToken.hasPrefix("claude") else { return }
        terminal.getTerminal().changeScrollback(Self.claudeScrollbackLines)
        scrollbackBumpedPanes.insert(paneId)
    }

    /// Auto-confirm claude's startup prompts for ANY pane running
    /// `claude --dangerously-load-development-channels` — whether ccmux spawned it, the user
    /// typed it in full, or it came from a shell alias/function (e.g. `claude_channels`). Keys
    /// off the real child-process argv, not the preexec-captured typed line, so the flag is seen
    /// regardless of how claude was invoked. Idempotent per running claude (see
    /// `handledDevChannelPanes`); re-arms once that claude exits.
    private func autoConfirmDevChannelsIfRunning(paneId: UUID, argv: String?) {
        if Self.isDevChannelsCommand(argv) {
            autoConfirmStartupPrompts(paneId: paneId)  // guarded; no-op if already handled
        } else {
            handledDevChannelPanes.remove(paneId)      // claude gone → re-arm for a future launch
        }
    }

    /// True if `argv` is a claude launched with `--dangerously-load-development-channels` — the
    /// flag that triggers claude's blocking "local development" startup prompt we auto-confirm.
    static func isDevChannelsCommand(_ argv: String?) -> Bool {
        argv?.contains("--dangerously-load-development-channels") ?? false
    }

    /// Sends a previously queued startup command, if any, into the PTY.
    /// Called by TerminalContainerView after the pane's first real layout so
    /// the TUI launches at the actual visible size, not the fallback frame.
    /// One-shot: each queued command runs at most once per pane lifetime.
    func runStartupCommandIfPending(paneId: UUID) {
        guard let command = pendingStartupCommands.removeValue(forKey: paneId),
              let terminal = terminals[paneId] else { return }
        terminal.send(Array((command + "\r").utf8))
    }

    /// Resize a pre-created off-screen terminal to `targetSize` (drives the kernel PTY winsize
    /// via setFrameSize → processSizeChange → setWinSize), notify the shell with SIGWINCH, then
    /// drain its queued startup command. Lets non-displayed workspaces replay their startup
    /// command on relaunch without waiting to be shown — the view path (fireStartupIfReady)
    /// only fires once a pane is embedded with real bounds. No-op if the pane has no terminal
    /// or nothing is queued. targetSize nil = fire at the current 800x480 fallback size.
    func fireStartupEagerly(paneId: UUID, targetSize: CGSize?) {
        guard let terminal = terminals[paneId] else { return }
        if let size = targetSize, size.width > 0, size.height > 0 {
            terminal.setFrameSize(size)
        }
        let pid = terminal.process?.shellPid ?? 0
        if pid > 0 {
            kill(pid, SIGWINCH)
        }
        runStartupCommandIfPending(paneId: paneId)
    }

    /// Send a command line into a live terminal immediately (as if typed + Enter).
    /// Used to launch a teammate claude in an already-open, idle designated pane.
    func sendCommand(_ command: String, to paneId: UUID) {
        guard let terminal = terminals[paneId] else { return }
        terminal.send(Array((command + "\r").utf8))
    }

    /// A freshly-spawned teammate (launched with `--dangerously-load-development-channels`)
    /// can show up to two blocking confirmation prompts before its session starts:
    ///  1. "Is this a project you trust? … 1. Yes, I trust this folder" (untrusted dirs only)
    ///  2. "Loading development channels … 1. I am using this for local development"
    /// Both pre-select option 1 and confirm on Enter. Watch the pane and press Enter for
    /// each as it appears, so a spawn is fully hands-free.
    ///
    /// Robustness notes (learned the hard way):
    ///  - claude can take tens of seconds to render the first prompt, so we watch for ~120s.
    ///  - We match each prompt's SPECIFIC text (not a generic "Enter to confirm") so the
    ///    watcher never auto-confirms a later tool-permission prompt.
    ///  - Plain CR (0x0D) confirms correctly even though claude enables the kitty keyboard
    ///    protocol; legacy Enter still works.
    ///  - Safe to call before the pane's terminal exists (it just keeps polling).
    func autoConfirmStartupPrompts(paneId: UUID) {
        // Idempotent per running claude: the first caller (spawn or the 5s scan) wins; the
        // pane stays "handled" until that claude exits and the scan re-arms it.
        guard handledDevChannelPanes.insert(paneId).inserted else { return }
        confirmStartupPrompts(paneId: paneId, attemptsRemaining: 480, entersRemaining: 3)
    }

    private func confirmStartupPrompts(paneId: UUID, attemptsRemaining: Int, entersRemaining: Int) {
        guard attemptsRemaining > 0, entersRemaining > 0 else { return }
        if let terminal = terminals[paneId],
           Self.isStartupConfirmPrompt(Self.visibleText(of: terminal.getTerminal())) {
            terminal.send([0x0D])  // Enter → confirms the pre-selected option
            // Give the screen ~2s to advance, then keep watching for a following prompt.
            DispatchQueue.main.asyncAfter(deadline: .now() + 2.0) { [weak self] in
                self?.confirmStartupPrompts(paneId: paneId, attemptsRemaining: attemptsRemaining - 8, entersRemaining: entersRemaining - 1)
            }
            return
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.25) { [weak self] in
            self?.confirmStartupPrompts(paneId: paneId, attemptsRemaining: attemptsRemaining - 1, entersRemaining: entersRemaining)
        }
    }

    /// True if the visible text is one of claude's startup confirmation prompts (trust-folder
    /// or load-development-channels). claude's TUI positions words with cursor moves, leaving
    /// the cells between them as NUL (`getCharacter()` for an empty cell returns "\0", NOT a
    /// space) — so we compact out NUL/space/newline and match the jammed phrase. Matching a
    /// spaced phrase silently never fires.
    static func isStartupConfirmPrompt(_ text: String) -> Bool {
        let compact = text
            .replacingOccurrences(of: "\0", with: "")
            .replacingOccurrences(of: " ", with: "")
            .replacingOccurrences(of: "\n", with: "")
        return compact.contains("localdevelopment") || compact.contains("trustthisfolder")
    }

    /// Currently-visible terminal text (viewport rows joined), for prompt detection.
    static func visibleText(of term: Terminal) -> String {
        var text = ""
        for row in 0..<term.rows {
            if let line = term.getLine(row: row) {
                text += line.translateToString(trimRight: true) + "\n"
            }
        }
        return text
    }

    /// Remove a terminal when its pane is closed.
    func remove(paneId: UUID) {
        workingDirs.removeValue(forKey: paneId)
        linkDelegates.removeValue(forKey: paneId)
        pendingStartupCommands.removeValue(forKey: paneId)
        scrollbackBumpedPanes.remove(paneId)
        handledDevChannelPanes.remove(paneId)
        // Clean up temp files
        try? FileManager.default.removeItem(atPath: cmdFilePath(for: paneId))
        if let zdotdir = zdotdirPaths.removeValue(forKey: paneId) {
            try? FileManager.default.removeItem(atPath: zdotdir)
        }
        if let terminal = terminals.removeValue(forKey: paneId) {
            terminal.terminalDelegate = nil
            // This dictionary is the terminal's last owner, so the view dies here.
            terminal.detachFromResponderChain()
            terminal.terminate()
        }
    }

    /// Kill every tracked terminal. Called from app-quit so child process groups
    /// (zsh + claude / dev servers / etc.) get reaped instead of leaking to launchd.
    func terminateAll() {
        for paneId in Array(terminals.keys) {
            remove(paneId: paneId)
        }
        claudeDetectionTimer?.cancel()
        claudeDetectionTimer = nil
        // Defensive: catches any zdotdir/cmd file whose paneId fell out of the
        // tracking maps before `remove` ran.
        sweepOrphanedTempFiles()
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
        var cleanPath = String(components[0])

        // Strip trailing sentence punctuation the implicit-link regex can capture
        // (e.g. "Full plan at /path/to/file.md." — the period is not part of the filename).
        let trailingPunctuation: Set<Character> = [".", ",", ";", "!", "?", ")", "]"]
        while let last = cleanPath.last, trailingPunctuation.contains(last) {
            cleanPath.removeLast()
        }

        // Resolve path
        let absolutePath = cleanPath.hasPrefix("/") ? cleanPath :
            (workingDirectory as NSString).appendingPathComponent(cleanPath)

        // If file exists, open in File Explorer
        if FileManager.default.fileExists(atPath: absolutePath) {
            let relativePath: String
            if cleanPath.hasPrefix(workingDirectory + "/") {
                relativePath = String(cleanPath.dropFirst(workingDirectory.count + 1))
            } else {
                relativePath = cleanPath
            }
            TerminalStore.shared.onFileLinkClicked?(paneId, relativePath)
        } else {
            // URL(string:) on a bare filesystem path produces a schemeless URL that
            // NSWorkspace rejects with OSStatus -50. Use fileURLWithPath for paths,
            // and only fall back to URL(string:) when the link has a real scheme.
            if let url = URL(string: link), url.scheme != nil {
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

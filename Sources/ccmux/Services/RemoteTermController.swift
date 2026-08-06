import AppKit
import SwiftTerm

/// Base `TerminalView` (no local process) that accepts first-mouse clicks, so a
/// hosted pane activates on the same first click as a local one.
final class ClickThroughRemoteTerminalView: TerminalView {
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}

/// Bridges one hosted pane to a SwiftTerm view: it IS the view's delegate, so
/// keystrokes (`send`) become WS `input` frames and view resizes (`sizeChanged`)
/// become WS `resize` frames; incoming `output`/`snapshot` bytes are `feed()` back
/// into the emulator. No child process — the process lives in tmux on the daemon.
///
/// The lens emulator sees Claude's *raw* bytes (the daemon un-escapes `%output` and
/// sends raw hex back via `send-keys -H`), so this is a thin byte pump — the same
/// role `LocalProcessTerminalView` plays for local panes, minus the PTY.
final class RemoteTermController: NSObject, TerminalViewDelegate {
    let paneId: String
    let workingDirectory: String
    let terminalView: TerminalView
    weak var attach: DaemonAttachClient?

    /// Routes a clicked file link (absolute local path) to the app. Hosted panes
    /// resolve links against the *local* clone at `workingDirectory` — the v1 scope
    /// cut (file/git panes are local, terminals are hosted).
    var onFileLinkClicked: ((String) -> Void)?

    private var lastSentCols = -1
    private var lastSentRows = -1

    /// The daemon's authoritative width for this pane (0 until known). When it
    /// diverges from what this view shows 1:1, another lens drove the shared pane
    /// and `isStale` becomes true — the hosting view surfaces a "take over" control.
    private var paneCols = 0
    private(set) var isStale = false
    /// Fired (main thread) when isStale flips, so the service can publish it.
    var onStaleChanged: ((Bool) -> Void)?

    /// Record the daemon's authoritative pane width (from a hello or pane-size frame)
    /// and re-evaluate staleness against this view's current grid.
    func setAuthoritativeSize(cols: Int, rows: Int) {
        paneCols = cols
        recomputeStale()
    }

    private func recomputeStale() {
        let gridCols = terminalView.getTerminal().cols
        let next = paneCols > 0 && gridCols > 0 && paneCols != gridCols
        if next != isStale {
            isStale = next
            onStaleChanged?(next)
        }
    }

    init(paneId: String, workingDirectory: String, attach: DaemonAttachClient?) {
        self.paneId = paneId
        self.workingDirectory = workingDirectory
        self.attach = attach
        // Match the local terminal's look so hosted and local panes are visually
        // identical (Monaco 12 on the ccmux dark background).
        self.terminalView = ClickThroughRemoteTerminalView(frame: NSRect(x: 0, y: 0, width: 800, height: 480))
        super.init()
        terminalView.font = NSFont(name: "Monaco", size: 12) ?? NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        terminalView.nativeForegroundColor = NSColor(white: 0.85, alpha: 1.0)
        terminalView.nativeBackgroundColor = NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)
        terminalView.optionAsMetaKey = false
        terminalView.terminalDelegate = self
    }

    // MARK: - Daemon → view

    /// Append live output bytes.
    func feedOutput(_ bytes: [UInt8]) {
        terminalView.feed(byteArray: bytes[...])
    }

    /// Re-seed from a fresh `capture-pane` snapshot. Unlike the web lens (which uses
    /// a throwaway xterm per attach), this view is reused across reconnects, so a
    /// re-seed must home+clear first or successive snapshots would stack down-screen.
    func seedSnapshot(_ bytes: [UInt8]) {
        terminalView.feed(byteArray: ArraySlice(Array("\u{1b}[H\u{1b}[2J".utf8)))
        terminalView.feed(byteArray: bytes[...])
    }

    /// Push the current grid size to the daemon, FORCING a re-send even when the
    /// grid size is unchanged (it resets the de-dupe first). Used after (re)attach
    /// and when the app window becomes key: another lens (e.g. a phone) may have
    /// resized the shared tmux pane while this window was in the background, so
    /// returning to it must re-assert what this window actually shows — otherwise
    /// the Mac keeps rendering the crushed, phone-driven width.
    func sendCurrentSize() {
        let t = terminalView.getTerminal()
        lastSentCols = -1
        lastSentRows = -1
        forwardResize(cols: t.cols, rows: t.rows)
    }

    // MARK: - TerminalViewDelegate (view → daemon)

    func send(source: TerminalView, data: ArraySlice<UInt8>) {
        attach?.send(.input(pane: paneId, bytes: data))
    }

    // Fired by SwiftTerm's processSizeChange whenever a view resize changes the grid
    // (AppleTerminalView.swift:180) — the hook that keeps tmux's window-size manual
    // matched to what the user sees.
    func sizeChanged(source: TerminalView, newCols: Int, newRows: Int) {
        forwardResize(cols: newCols, rows: newRows)
    }

    private func forwardResize(cols: Int, rows: Int) {
        guard cols > 0, rows > 0 else { return }
        // Only the *embedded* (on-screen) pane drives tmux's size (window-size manual =
        // driver wins); pre-warmed off-screen panes have no superview and must not crunch
        // the displayed pane's dimensions. Gating on `window` instead would wrongly drop
        // the resize during SwiftUI's initial layout pass, where the view is embedded but
        // not yet attached to a window — leaving tmux stuck at its 80x24 default.
        guard terminalView.superview != nil else { return }
        guard cols != lastSentCols || rows != lastSentRows else { return }
        lastSentCols = cols
        lastSentRows = rows
        // We are driving the pane to our grid — reflect it immediately (the daemon's
        // pane-size broadcast confirms) so "take over" clears without a round-trip.
        paneCols = cols
        recomputeStale()
        attach?.send(.resize(pane: paneId, cols: cols, rows: rows))
    }

    func requestOpenLink(source: TerminalView, link: String, params: [String: String]) {
        RemoteTermController.handleOpenLink(link, workingDirectory: workingDirectory, onFile: onFileLinkClicked)
    }

    // Unused delegate calls — the daemon owns process lifecycle, so these are inert.
    func setTerminalTitle(source: TerminalView, title: String) {}
    func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
    func scrolled(source: TerminalView, position: Double) {}
    func bell(source: TerminalView) {}
    func clipboardCopy(source: TerminalView, content: Data) {
        if let s = String(data: content, encoding: .utf8) {
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(s, forType: .string)
        }
    }
    func iTermContent(source: TerminalView, content: ArraySlice<UInt8>) {}
    func rangeChanged(source: TerminalView, startY: Int, endY: Int) {}

    // MARK: - Link handling (mirrors TerminalLinkInterceptor, daemon-side resolution)

    enum LinkAction: Equatable {
        case openExternal(URL)
        case openFile(String)
    }

    /// Pure classification of a clicked link, split from the side effects for
    /// testability. The file lives on the daemon's host, so existence can't be
    /// checked here — file-looking links always classify as `.openFile`; a bad
    /// path simply fails the daemon read downstream and opens nothing.
    static func linkAction(_ link: String, workingDirectory: String) -> LinkAction {
        // Non-file URL schemes (http, mailto, vscode, …) open externally. The
        // "://" probe avoids URL(string:)'s scheme parsing, which would read
        // "file.md:3" as scheme "file.md".
        if !link.hasPrefix("file://"), link.contains("://") || link.hasPrefix("mailto:"),
           let url = URL(string: link) {
            return .openExternal(url)
        }
        var filePath = link
        if filePath.hasPrefix("file://") {
            filePath = String(filePath.dropFirst(7)).removingPercentEncoding ?? String(filePath.dropFirst(7))
        }
        let components = filePath.split(separator: ":", maxSplits: 2)
        var cleanPath = String(components[0])
        let trailingPunctuation: Set<Character> = [".", ",", ";", "!", "?", ")", "]"]
        while let last = cleanPath.last, trailingPunctuation.contains(last) { cleanPath.removeLast() }

        let absolutePath = cleanPath.hasPrefix("/") ? cleanPath
            : (workingDirectory as NSString).appendingPathComponent(cleanPath)
        return .openFile(absolutePath)
    }

    static func handleOpenLink(_ link: String, workingDirectory: String, onFile: ((String) -> Void)?) {
        switch linkAction(link, workingDirectory: workingDirectory) {
        case .openExternal(let url): NSWorkspace.shared.open(url)
        case .openFile(let path): onFile?(path)
        }
    }
}

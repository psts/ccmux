import AppKit
import SwiftUI
import UserNotifications

class AppDelegate: NSObject, NSApplicationDelegate {
    private var windowManager: WindowManager?
    private let workspaceManager = WorkspaceManager()
    private var quitConfirmationController: QuitConfirmationController?
    private var settingsWindow: NSWindow?

    /// Listens for Claude Code hook events and drives the sidebar attention flash.
    private var hookListener: ClaudeHookListener?
    private let attentionNotifier = AttentionNotifier()

    /// ccmux://spawn requests that arrived before the window manager was ready
    /// (e.g. when a spawn URL cold-starts the app). Flushed once launch completes.
    private var pendingSpawnRequests: [SpawnRequest] = []
    private var isReadyForSpawns = false

    /// Exposed for AppleScript command handlers.
    var windowManagerForScripting: WindowManager? { windowManager }

    func applicationWillFinishLaunching(_ notification: Notification) {
        setAppIcon()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        registerScriptingIfNeeded()
        buildMainMenu()

        // Try to restore saved state
        let windowDescriptors = workspaceManager.loadState()

        // If no saved workspaces, create one from cwd
        if workspaceManager.workspaces.isEmpty {
            let cwd = FileManager.default.currentDirectoryPath
            let name = (cwd as NSString).lastPathComponent
            workspaceManager.addWorkspace(name: name, repoPath: cwd)
        }

        let wm = WindowManager(workspaceManager: workspaceManager)
        self.windowManager = wm

        // Tell WorkspaceManager how to get window descriptors for saving
        workspaceManager.windowDescriptorProvider = { [weak wm] in
            wm?.windowDescriptors() ?? []
        }

        let qc = QuitConfirmationController()
        qc.install()
        self.quitConfirmationController = qc

        NSApp.activate(ignoringOtherApps: true)

        // Defer window/terminal creation — and therefore the first Monaco load — to the
        // next run-loop pass. Launched via LaunchServices the fresh session's CoreText
        // font connection isn't ready yet during didFinishLaunching, so resolving Monaco
        // synchronously here comes back degraded ("System LastResort not available") and
        // every glyph renders as a .notdef "tofu" box — and the bad resolution is cached
        // for the process's lifetime. (Run bare from a terminal it works because the
        // connection is already up.) Creating windows one tick later lets it establish.
        DispatchQueue.main.async { [weak self, weak wm] in
            guard let self, let wm else { return }
            if !windowDescriptors.isEmpty {
                wm.restoreWindows(from: windowDescriptors)
            } else {
                wm.createWindow(displayingWorkspace: self.workspaceManager.activeWorkspaceId ?? self.workspaceManager.workspaces.first?.id)
            }
            // Pre-create terminals and replay startup commands for ALL workspaces — non-displayed
            // ones fire eagerly here (the view layer only fires the displayed workspace), sized to
            // their owning window's content area.
            self.workspaceManager.preCreateTerminals(
                displayedWorkspaceIds: Set(wm.windowControllers.compactMap { $0.windowContext.displayedWorkspaceId }),
                contentSizeProvider: { [weak wm] wsId in wm?.contentSize(forWorkspace: wsId) }
            )

            // The window manager is now live — service any spawn URLs that cold-started us.
            self.isReadyForSpawns = true
            self.flushPendingSpawns()

            // Start listening for Claude Code attention events (sidebar flash + notifications).
            self.startAttentionSignals(windowManager: wm)

            // Start polling the ccmuxd daemon for hosted (lens) workspaces.
            self.startRemoteSessions()
        }
    }

    /// Wire and start the hosted-session service: it polls ccmuxd, renders hosted
    /// workspaces through the same sidebar/SplitTree machinery, and feeds the same
    /// attention flash + notifications the local hook path uses.
    private func startRemoteSessions() {
        let service = RemoteSessionService.shared
        service.isWatched = { [weak self] id in self?.isWatchingWorkspace(id) ?? false }
        service.onAttention = { [weak self] workspace, state in
            self?.attentionNotifier.post(for: workspace, state: state)
        }
        // Hosted workspaces sit in the same sidebar window groups as local ones:
        // adopt newly appeared ones (created by another lens) into a window, and
        // drop removed ones from every window.
        service.onWorkspacesChanged = { [weak self] in
            self?.windowManager?.adoptOrphanHostedWorkspaces()
        }
        service.onWorkspaceRemoved = { [weak self] id in
            self?.windowManager?.hostedWorkspaceRemoved(id: id)
        }
        service.onFileLink = { wsId, absolutePath in
            // Hosted panes are terminal-only in v1; reveal a clicked file in the local
            // clone rather than mutating the daemon-driven layout.
            guard FileManager.default.fileExists(atPath: absolutePath) else { return }
            let parent = (absolutePath as NSString).deletingLastPathComponent
            NSWorkspace.shared.selectFile(absolutePath, inFileViewerRootedAtPath: parent)
        }
        service.start()
    }

    /// True when ccmux is frontmost AND the key window displays this (hosted)
    /// workspace — mirrors `ClaudeHookListener.isCurrentlyWatched` for local ones, so
    /// a workspace you're already looking at doesn't flash or notify.
    private func isWatchingWorkspace(_ id: UUID) -> Bool {
        guard NSApp.isActive,
              let keyWindow = NSApp.keyWindow,
              let wc = windowManager?.windowControllers.first(where: { $0.window === keyWindow })
        else { return false }
        return wc.windowContext.displayedWorkspaceId == id
    }

    /// Wire up the macOS-notification delegate and the hook-event socket listener.
    /// Both depend on the window manager being live (for click navigation and the
    /// "currently watched" suppression check).
    private func startAttentionSignals(windowManager wm: WindowManager) {
        if Bundle.main.bundleIdentifier != nil {
            UNUserNotificationCenter.current().delegate = self
            attentionNotifier.requestAuthorization()
        }
        let listener = ClaudeHookListener(
            workspaceManager: workspaceManager,
            windowManager: wm,
            notifier: attentionNotifier
        )
        listener.start()
        hookListener = listener
    }

    // MARK: - ccmux:// URL scheme (teammate spawning from claude-peers)

    /// Handle `ccmux://spawn?repo=…&prompt=…&requester=…` deep links. macOS delivers
    /// these via LaunchServices, cold-starting ccmux if it isn't already running.
    func application(_ application: NSApplication, open urls: [URL]) {
        let requests = urls.compactMap { SpawnRequest.parse(from: $0) }
        guard !requests.isEmpty else { return }
        if isReadyForSpawns {
            requests.forEach { handleSpawn($0) }
        } else {
            pendingSpawnRequests.append(contentsOf: requests)
        }
    }

    private func flushPendingSpawns() {
        let pending = pendingSpawnRequests
        pendingSpawnRequests.removeAll()
        pending.forEach { handleSpawn($0) }
    }

    private func handleSpawn(_ request: SpawnRequest) {
        let workspaceId = workspaceManager.spawnTeammate(request)
        showWorkspace(workspaceId)
    }

    /// Bring the given workspace on screen and to the front so a spawned teammate is visible.
    private func showWorkspace(_ id: UUID) {
        guard let wm = windowManager else { return }
        NSApp.activate(ignoringOtherApps: true)
        if let wc = wm.windowDisplaying(workspaceId: id) ?? wm.windowOwning(workspaceId: id) {
            wc.windowContext.displayedWorkspaceId = id
            wc.updateWindowTitle()
            wc.window?.makeKeyAndOrderFront(nil)
        } else if let wc = wm.windowControllers.first {
            wm.selectWorkspace(id: id, from: wc)
            wc.window?.makeKeyAndOrderFront(nil)
        } else {
            wm.createWindow(displayingWorkspace: id)
        }
        workspaceManager.activeWorkspaceId = id
    }

    func applicationWillTerminate(_ notification: Notification) {
        hookListener?.stop()
        RemoteSessionService.shared.stop()
        quitConfirmationController?.teardown()
        // Prevent windowWillClose from moving workspaces to closedWorkspaces during quit
        windowManager?.isTerminating = true
        // Capture running commands before terminals are destroyed (uses libproc on each shellPid)
        workspaceManager.detectAndSaveCommands()
        // Persist workspaces + window descriptors so the next launch can replay startupCommands
        workspaceManager.saveState()
        // Reap every shell + its process group. Without this, ccmux's spawned zsh shells
        // (and anything they were running — claude, dev servers, …) survive app quit
        // because nothing else closes the PTYs.
        TerminalStore.shared.terminateAll()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag {
            // Create a window — if there are open workspaces show the first one, otherwise show welcome
            windowManager?.createWindow(displayingWorkspace: workspaceManager.workspaces.first?.id)
        }
        return true
    }

    private var activeController: SplitTreeController? {
        guard let keyWindow = NSApp.keyWindow,
              let wc = windowManager?.windowControllers.first(where: { $0.window === keyWindow })
        else { return nil }
        return wc.activeController
    }

    @objc private func splitHorizontal() {
        guard let ctrl = activeController, let id = ctrl.focusedPaneId else { return }
        ctrl.splitPane(id: id, direction: .horizontal)
    }

    @objc private func splitVertical() {
        guard let ctrl = activeController, let id = ctrl.focusedPaneId else { return }
        ctrl.splitPane(id: id, direction: .vertical)
    }

    @objc private func togglePeerMessages() {
        guard let keyWindow = NSApp.keyWindow,
              let wc = windowManager?.windowControllers.first(where: { $0.window === keyWindow })
        else { return }
        wc.togglePeerMessages()
    }

    /// Settings… (⌘,) — edits the daemon-wide settings (startup command +
    /// per-folder rules); one lazily-created window, reused across opens.
    @objc private func openDaemonSettings() {
        if settingsWindow == nil {
            let window = NSWindow(contentViewController: NSHostingController(rootView: DaemonSettingsView()))
            window.title = "Settings"
            window.styleMask = [.titled, .closable]
            window.isReleasedWhenClosed = false
            window.center()
            settingsWindow = window
        }
        settingsWindow?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc private func closeFocusedPane() {
        guard let ctrl = activeController, let id = ctrl.focusedPaneId else { return }
        ctrl.closePane(id: id)
    }

    @objc private func openWorkspace() {
        // Show panel on the key window, or create a new window
        if let keyWindow = NSApp.keyWindow,
           let wc = windowManager?.windowControllers.first(where: { $0.window === keyWindow }) {
            wc.showAddWorkspacePanel()
        } else {
            windowManager?.windowControllers.first?.showAddWorkspacePanel()
        }
    }

    @objc private func newWindow() {
        windowManager?.createWindow(displayingWorkspace: workspaceManager.workspaces.first?.id)
    }

    private func setAppIcon() {
        // Try loading from app bundle first (release .app build)
        if let bundleIcon = NSImage(named: "AppIcon") {
            NSApp.applicationIconImage = bundleIcon
            return
        }

        // Fallback: load from source tree (debug build via swift build)
        // Walk up from the executable to find the project root
        let executablePath = ProcessInfo.processInfo.arguments[0]
        let execDir = (executablePath as NSString).deletingLastPathComponent

        // Check common locations relative to the executable
        let candidates = [
            (execDir as NSString).appendingPathComponent("../../AppIcon.icns"),          // .build/debug/ → project root
            (execDir as NSString).appendingPathComponent("../../../AppIcon.icns"),        // deeper build paths
            (FileManager.default.currentDirectoryPath as NSString).appendingPathComponent("AppIcon.icns"), // cwd
        ]

        for path in candidates {
            let resolved = (path as NSString).standardizingPath
            if FileManager.default.fileExists(atPath: resolved),
               let icon = NSImage(contentsOfFile: resolved) {
                NSApp.applicationIconImage = icon
                return
            }
        }
    }

    private func registerScriptingIfNeeded() {
        // Cocoa Scripting auto-loads from Info.plist + sdef in Resources for .app bundles.
        // For debug builds (plain executable), AppleScript is not available.
        if Bundle.main.path(forResource: "ccmux", ofType: "sdef") == nil {
            print("[AppleScript] Scripting not available — run from .app bundle (./build-app.sh)")
        }
    }

    private func buildMainMenu() {
        let mainMenu = NSMenu()

        // App menu
        let appMenuItem = NSMenuItem()
        mainMenu.addItem(appMenuItem)
        let appMenu = NSMenu()
        appMenuItem.submenu = appMenu
        appMenu.addItem(withTitle: "About ccmux", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Settings…", action: #selector(openDaemonSettings), keyEquivalent: ",")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Quit ccmux", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "")

        // File menu
        let fileMenuItem = NSMenuItem()
        mainMenu.addItem(fileMenuItem)
        let fileMenu = NSMenu(title: "File")
        fileMenuItem.submenu = fileMenu
        fileMenu.addItem(withTitle: "Open Workspace…", action: #selector(openWorkspace), keyEquivalent: "o")

        let newWindowItem = NSMenuItem(title: "New Window", action: #selector(newWindow), keyEquivalent: "n")
        newWindowItem.keyEquivalentModifierMask = [.command, .shift]
        fileMenu.addItem(newWindowItem)

        fileMenu.addItem(.separator())
        fileMenu.addItem(withTitle: "Close Pane", action: #selector(closeFocusedPane), keyEquivalent: "w")
        fileMenu.addItem(withTitle: "Close Window", action: #selector(NSWindow.close), keyEquivalent: "W")

        // View menu
        let viewMenuItem = NSMenuItem()
        mainMenu.addItem(viewMenuItem)
        let viewMenu = NSMenu(title: "View")
        viewMenuItem.submenu = viewMenu

        let splitH = NSMenuItem(title: "Split Horizontal", action: #selector(splitHorizontal), keyEquivalent: "d")
        viewMenu.addItem(splitH)

        let splitV = NSMenuItem(title: "Split Vertical", action: #selector(splitVertical), keyEquivalent: "d")
        splitV.keyEquivalentModifierMask = [.command, .shift]
        viewMenu.addItem(splitV)

        viewMenu.addItem(.separator())
        let peerItem = NSMenuItem(title: "Peer Messages", action: #selector(togglePeerMessages), keyEquivalent: "p")
        peerItem.keyEquivalentModifierMask = [.command, .shift]
        viewMenu.addItem(peerItem)

        // Edit menu
        let editMenuItem = NSMenuItem()
        mainMenu.addItem(editMenuItem)
        let editMenu = NSMenu(title: "Edit")
        editMenuItem.submenu = editMenu

        editMenu.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
        let redoItem = NSMenuItem(title: "Redo", action: Selector(("redo:")), keyEquivalent: "z")
        redoItem.keyEquivalentModifierMask = [.command, .shift]
        editMenu.addItem(redoItem)
        editMenu.addItem(.separator())

        editMenu.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editMenu.addItem(.separator())

        let findItem = NSMenuItem(title: "Find…", action: #selector(NSTextView.performFindPanelAction(_:)), keyEquivalent: "f")
        findItem.tag = Int(NSTextFinder.Action.showFindInterface.rawValue)
        editMenu.addItem(findItem)

        let findReplaceItem = NSMenuItem(title: "Find and Replace…", action: #selector(NSTextView.performFindPanelAction(_:)), keyEquivalent: "f")
        findReplaceItem.keyEquivalentModifierMask = [.command, .option]
        findReplaceItem.tag = Int(NSTextFinder.Action.showReplaceInterface.rawValue)
        editMenu.addItem(findReplaceItem)

        let findNextItem = NSMenuItem(title: "Find Next", action: #selector(NSTextView.performFindPanelAction(_:)), keyEquivalent: "g")
        findNextItem.tag = Int(NSTextFinder.Action.nextMatch.rawValue)
        editMenu.addItem(findNextItem)

        let findPrevItem = NSMenuItem(title: "Find Previous", action: #selector(NSTextView.performFindPanelAction(_:)), keyEquivalent: "g")
        findPrevItem.keyEquivalentModifierMask = [.command, .shift]
        findPrevItem.tag = Int(NSTextFinder.Action.previousMatch.rawValue)
        editMenu.addItem(findPrevItem)

        // Window menu
        let windowMenuItem = NSMenuItem()
        mainMenu.addItem(windowMenuItem)
        let windowMenu = NSMenu(title: "Window")
        windowMenuItem.submenu = windowMenu
        windowMenu.addItem(withTitle: "Minimize", action: #selector(NSWindow.miniaturize(_:)), keyEquivalent: "m")
        windowMenu.addItem(withTitle: "Zoom", action: #selector(NSWindow.zoom(_:)), keyEquivalent: "")

        NSApp.mainMenu = mainMenu
        NSApp.windowsMenu = windowMenu
    }
}

// MARK: - Attention Notifications

extension AppDelegate: UNUserNotificationCenterDelegate {
    /// Show the banner even when ccmux is frontmost (we only post for background
    /// workspaces, so the alert always concerns a workspace you're not viewing).
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner, .sound])
    }

    /// Clicking the notification navigates to the workspace that needed attention.
    func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        if let idString = response.notification.request.content.userInfo[AttentionNotifier.workspaceIdKey] as? String,
           let id = UUID(uuidString: idString) {
            showWorkspace(id)
        }
        completionHandler()
    }
}

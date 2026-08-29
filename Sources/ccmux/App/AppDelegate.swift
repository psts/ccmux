import AppKit
import SwiftUI
import UserNotifications

class AppDelegate: NSObject, NSApplicationDelegate, NSMenuItemValidation {
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

        // First launch only: seed a workspace from the directory we were started in.
        if let seed = Self.launchSeedRepoPath(
            savedWindows: windowDescriptors,
            localWorkspaces: workspaceManager.workspaces,
            cwd: FileManager.default.currentDirectoryPath
        ) {
            workspaceManager.addWorkspace(name: (seed as NSString).lastPathComponent, repoPath: seed)
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

            // Only now, with windows on screen and the app active, is a modal safe:
            // run from didFinishLaunching it would block launch on an alert owned by
            // an inactive app with nothing behind it.
            self.reportStateLoadWarning()

            // The window manager is now live — service any spawn URLs that cold-started us.
            self.isReadyForSpawns = true
            self.flushPendingSpawns()

            // Start listening for Claude Code attention events (sidebar flash + notifications).
            self.startAttentionSignals(windowManager: wm)

            // Discover the federation hub (via the local daemon) BEFORE the
            // daemon services connect, so they target the hub from their first
            // request. Falls through to the local daemon when there is no hub
            // or an unreachable one — and keeps retrying in the background for
            // the cold-boot case where the daemon hasn't found the hub yet,
            // rewiring the live services if it lands late.
            Task { @MainActor in
                await HubDiscovery.adoptHub(onLateAdopt: {
                    RemoteSessionService.shared.hubAdopted()
                })
                // Start polling the ccmuxd daemon for hosted (lens) workspaces.
                self.startRemoteSessions()
                // Keep the daemons on the same release as this app: local via
                // its binary, the fleet via the hub. AFTER adoptHub — the
                // fleet pass reads DaemonConfig.baseURL, and firing before the
                // hub URL lands would ask the local daemon for /v1/hosts,
                // which only exists on the hub.
                UpdaterService.shared.syncLocalDaemon()
                // Daemon child-process census, feeding the sidebar's warning
                // strip. In here because start() is MainActor-isolated and the
                // enclosing DispatchQueue.main.async closure is not one to the
                // compiler. Independent of adoptHub: this reads localURL, not
                // the hub's baseURL.
                DaemonHealthService.shared.start()
            }

            // Quiet update checks (launch + every 4h) — prompts only when a
            // newer release exists; the menu item stays the loud path.
            UpdaterService.shared.startAutomaticChecks()
        }
    }

    /// Tell the user when state.json did not come back whole. Losing saved workspaces
    /// silently is how a corrupt file becomes "ccmux forgot everything" with no trace
    /// the user can follow — the alert names the backup so recovery is possible.
    private func reportStateLoadWarning() {
        guard let warning = PersistenceService.lastLoadWarning else { return }
        let alert = NSAlert()
        alert.messageText = "Some saved state could not be restored"
        alert.informativeText = warning
        alert.alertStyle = .warning
        alert.addButton(withTitle: "OK")
        alert.runModal()
    }

    /// The directory to seed a first-run workspace from, or nil when we must not seed one.
    ///
    /// Two things make the naive "no workspaces yet → make one from cwd" wrong:
    ///
    /// 1. Hosted (lens) workspaces live in ccmuxd and arrive *after* launch, so an
    ///    empty `localWorkspaces` says nothing about whether the user has sessions.
    ///    Saved window descriptors are the real "this is a returning session" signal.
    /// 2. Launched from the Dock/Finder, LaunchServices gives the app cwd `/`. Seeding
    ///    from that mints a junk workspace named "/" rooted at the filesystem root —
    ///    the phantom "empty session with one terminal, Not a git repository" that used
    ///    to appear in the first window's sidebar on every reopen.
    static func launchSeedRepoPath(
        savedWindows: [WindowDescriptor], localWorkspaces: [Workspace], cwd: String
    ) -> String? {
        guard savedWindows.isEmpty, localWorkspaces.isEmpty else { return nil }
        guard cwd != "/" else { return nil }
        return cwd
    }

    /// Wire and start the hosted-session service: it polls ccmuxd, renders hosted
    /// workspaces through the same sidebar/SplitTree machinery, and feeds the same
    /// attention flash + notifications the local hook path uses.
    private func startRemoteSessions() {
        let service = RemoteSessionService.shared
        service.isWatched = { [weak self] id in self?.windowManager?.isWatching(id) ?? false }
        // Displayed hosted workspaces get a daemon focus frame while the user is
        // at this Mac — that's what keeps phone pushes quiet at the desk.
        service.displayedHostedWorkspaceIds = { [weak self] in
            guard let wm = self?.windowManager else { return [] }
            return Set(wm.windowControllers
                .compactMap { $0.windowContext.displayedWorkspaceId }
                .filter { RemoteSessionService.shared.isHosted($0) })
        }
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
            // The file lives on the daemon's host: open it in the workspace's
            // own file explorer (backed by the daemon's file routes), never in
            // Finder — the local Mac may not even have a clone.
            RemoteSessionService.shared.revealFile(wsId, path: absolutePath)
        }
        service.start()
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

    /// Builds the View menu.
    private func viewMenuItem() -> NSMenuItem {
        let viewMenuItem = NSMenuItem()
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
        return viewMenuItem
    }

    /// Builds the Edit menu. Separate from `buildMainMenu` because it is the one
    /// menu with behaviour rather than just items — its first three entries have to
    /// carry an explicit target — and because a test can then address it directly.
    func editMenuItem() -> NSMenuItem {
        let editMenuItem = NSMenuItem()
        let editMenu = NSMenu(title: "Edit")
        editMenuItem.submenu = editMenu

        // Undo, Redo and Cut are aimed at this delegate, NOT sent as bare actions
        // into the responder chain. ccmux crashed on Cmd+Z (SIGSEGV, with AppKit
        // reporting "Performing @selector(undo:) from sender NSMenuItem"): with no
        // target, AppKit walks the chain from window.firstResponder, and right
        // after a pane closes that walk can reach something already gone.
        //
        // Which actions walk the WHOLE chain is what decides the exposure, and it
        // comes down to what SwiftTerm exposes to the Objective-C runtime:
        // `paste(_:)` and `copy(_:)` are @objc and `selectAll(_:)` overrides
        // NSResponder, so those three stop at the terminal. `cut(sender:)` is
        // neither @objc nor named `cut:`, so Cut answered nobody and walked exactly
        // as far as Undo did. Undo and Redo have no implementation anywhere.
        //
        // An explicit target means AppKit dispatches straight here and never walks
        // the chain. That removes the many stale links a full walk could reach, but
        // NOT the head of it: the handlers below still read
        // `NSApp.keyWindow?.firstResponder` themselves, and that one reference is
        // where the original SIGSEGV was. What makes those reads safe is
        // `detachFromResponderChain`, called wherever a terminal's last owner drops
        // it — see ResponderDetachTests and DetachOnPaneCloseTests. Removing one of
        // those calls is not made harmless by this targeting.
        let undoItem = NSMenuItem(title: "Undo", action: #selector(performUndo(_:)), keyEquivalent: "z")
        undoItem.target = self
        editMenu.addItem(undoItem)
        let redoItem = NSMenuItem(title: "Redo", action: #selector(performRedo(_:)), keyEquivalent: "z")
        redoItem.keyEquivalentModifierMask = [.command, .shift]
        redoItem.target = self
        editMenu.addItem(redoItem)
        editMenu.addItem(.separator())

        let cutItem = NSMenuItem(title: "Cut", action: #selector(performCut(_:)), keyEquivalent: "x")
        cutItem.target = self
        editMenu.addItem(cutItem)
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
        return editMenuItem
    }

    /// The undo manager for whatever currently has the keyboard, or nil when that is
    /// something with no undo stack — a terminal, most of the time.
    ///
    /// `firstResponder?.undoManager` is read through AppKit's own accessor rather
    /// than by walking the chain by hand, and only ever on a live key window.
    private var activeUndoManager: UndoManager? {
        NSApp.keyWindow?.firstResponder?.undoManager
    }

    @objc private func performUndo(_ sender: Any?) {
        guard let manager = activeUndoManager, manager.canUndo else { return }
        manager.undo()
    }

    @objc private func performRedo(_ sender: Any?) {
        guard let manager = activeUndoManager, manager.canRedo else { return }
        manager.redo()
    }

    /// Cut, delivered to the focused view directly rather than walked to.
    ///
    /// One hop, not a chain search: the thing being cut from is the thing with the
    /// keyboard, and stopping there is what keeps a stale link further up the chain
    /// out of reach. A terminal does not answer `cut:` at all, so Cmd+X there is a
    /// no-op, which is what it already was.
    @objc private func performCut(_ sender: Any?) {
        let cut = #selector(NSText.cut(_:))
        guard let responder = NSApp.keyWindow?.firstResponder, responder.responds(to: cut) else { return }
        _ = responder.perform(cut, with: sender)
    }

    /// Greys Undo, Redo and Cut out when the focused thing cannot answer them,
    /// instead of offering an action that would silently do nothing.
    func validateMenuItem(_ menuItem: NSMenuItem) -> Bool {
        switch menuItem.action {
        case #selector(performUndo(_:)): return activeUndoManager?.canUndo ?? false
        case #selector(performRedo(_:)): return activeUndoManager?.canRedo ?? false
        case #selector(performCut(_:)):
            return NSApp.keyWindow?.firstResponder?.responds(to: #selector(NSText.cut(_:))) ?? false
        default: return true
        }
    }

    @objc private func togglePeerMessages() {
        guard let keyWindow = NSApp.keyWindow,
              let wc = windowManager?.windowControllers.first(where: { $0.window === keyWindow })
        else { return }
        wc.togglePeerMessages()
    }

    /// Check for Updates… — the app-side `ccmuxd upgrade` (GitHub releases,
    /// download + verify + swap + relaunch). See UpdaterService.
    @objc private func checkForUpdates() {
        MainActor.assumeIsolated { UpdaterService.shared.checkForUpdates() }
    }

    /// Settings… (⌘,) — edits the daemon-wide settings (startup command +
    /// per-folder rules); one lazily-created window, reused across opens.
    @objc private func openDaemonSettings() {
        if settingsWindow == nil {
            let settings = DaemonSettingsView(onDone: { [weak self] in self?.settingsWindow?.close() })
            let window = NSWindow(contentViewController: NSHostingController(rootView: settings))
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

    /// File → New Window (Cmd+N) and the sidebar's New Window button (the
    /// macwindow.badge.plus at its top) share one meaning: an EMPTY window
    /// (welcome screen). Opening with a workspace pre-displayed belongs to
    /// "Open in New Window" on the workspace itself.
    @objc private func newWindow() {
        windowManager?.createWindow(displayingWorkspace: nil)
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

    /// Internal rather than private so a test can assert Undo/Redo keep an explicit
    /// target — a nil target is the crash, not a style choice.
    func buildMainMenu() {
        let mainMenu = NSMenu()

        // App menu
        let appMenuItem = NSMenuItem()
        mainMenu.addItem(appMenuItem)
        let appMenu = NSMenu()
        appMenuItem.submenu = appMenu
        appMenu.addItem(withTitle: "About ccmux", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(withTitle: "Check for Updates…", action: #selector(checkForUpdates), keyEquivalent: "")
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

        mainMenu.addItem(viewMenuItem())

        mainMenu.addItem(editMenuItem())

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

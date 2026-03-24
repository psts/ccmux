import AppKit

class AppDelegate: NSObject, NSApplicationDelegate {
    private var windowManager: WindowManager?
    private let workspaceManager = WorkspaceManager()

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

        // Restore windows from saved state, or create one
        if !windowDescriptors.isEmpty {
            wm.restoreWindows(from: windowDescriptors)
        } else {
            wm.createWindow(displayingWorkspace: workspaceManager.activeWorkspaceId ?? workspaceManager.workspaces.first?.id)
        }

        NSApp.activate(ignoringOtherApps: true)
    }

    func applicationWillTerminate(_ notification: Notification) {
        // Prevent windowWillClose from moving workspaces to closedWorkspaces during quit
        windowManager?.isTerminating = true
        // Save current state including all window descriptors
        workspaceManager.saveState()
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
        appMenu.addItem(withTitle: "Quit ccmux", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")

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

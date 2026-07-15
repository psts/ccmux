import AppKit
import SwiftUI
import Combine

class WorkspaceWindowController: NSWindowController, NSWindowDelegate {
    let workspaceManager: WorkspaceManager
    weak var windowManager: WindowManager?
    let windowContext: WindowContext
    let windowId: UUID
    let peerMessagesController = PeerMessagesController()

    init(
        workspaceManager: WorkspaceManager,
        windowManager: WindowManager,
        displayedWorkspaceId: UUID?,
        windowId: UUID = UUID()
    ) {
        self.workspaceManager = workspaceManager
        self.windowManager = windowManager
        self.windowId = windowId
        self.windowContext = WindowContext(
            workspaceId: displayedWorkspaceId,
            workspaceManager: workspaceManager
        )

        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1200, height: 800),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.titlebarAppearsTransparent = true
        window.titleVisibility = .hidden
        window.minSize = NSSize(width: 600, height: 400)
        window.appearance = NSAppearance(named: .darkAqua)

        super.init(window: window)
        window.delegate = self

        setupSplitView()
        updateWindowTitle()
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("init(coder:) not implemented")
    }

    func updateWindowTitle() {
        if let wsId = windowContext.displayedWorkspaceId,
           let ws = workspaceManager.workspaces.first(where: { $0.id == wsId })
            ?? RemoteSessionService.shared.workspaces.first(where: { $0.id == wsId }) {
            window?.title = ws.name
        } else {
            window?.title = "ccmux"
        }
    }

    /// Pick a project folder from the daemon's projects root and create a hosted
    /// session there. No local file panel: the folders live on the daemon's
    /// filesystem, which may be a remote server. The new workspace joins THIS
    /// window's sidebar group and is displayed, like a locally added one.
    func showAddHostedWorkspacePanel() {
        guard let window else { return }
        var sheet: NSWindow?
        let picker = HostedProjectPickerView(
            onPick: { [weak self] project in
                if let sheet { window.endSheet(sheet) }
                Task { @MainActor in
                    guard let newId = await RemoteSessionService.shared.createWorkspace(
                        name: project.name, repoPath: project.path, startupCommand: "claude"),
                        let self else { return }
                    self.windowContext.ownedWorkspaceIds.insert(newId)
                    self.windowContext.displayedWorkspaceId = newId
                    self.updateWindowTitle()
                    self.windowManager?.refreshOtherWindowIds()
                }
            },
            onCancel: { if let sheet { window.endSheet(sheet) } }
        )
        let sheetWindow = NSWindow(contentViewController: NSHostingController(rootView: picker))
        sheet = sheetWindow
        window.beginSheet(sheetWindow)
    }

    private func setupSplitView() {
        let splitVC = NSSplitViewController()

        // Sidebar — passes per-window context for selection
        let sidebarView = SidebarView(
            manager: workspaceManager,
            windowContext: windowContext,
            remoteService: RemoteSessionService.shared,
            onAddWorkspace: { [weak self] in
                self?.showAddWorkspacePanel()
            },
            onDetachWorkspace: { [weak self] id in
                self?.windowManager?.detachWorkspace(id: id, sourceWindow: self?.window)
            },
            onSelectWorkspace: { [weak self] id in
                guard let self else { return }
                self.windowManager?.selectWorkspace(id: id, from: self)
            },
            onReopenWorkspace: { [weak self] id in
                self?.windowManager?.reopenWorkspace(id: id)
            },
            onMoveToThisWindow: { [weak self] id in
                guard let self else { return }
                self.windowManager?.moveWorkspaceToWindow(id: id, targetController: self)
            },
            currentWindowId: windowId,
            onRenameWindow: { [weak self] windowId, currentName in
                self?.showRenameWindowAlert(windowId: windowId, currentName: currentName)
            },
            onRestoreWindow: { [weak self] windowId in
                self?.windowManager?.restoreClosedWindow(id: windowId)
            },
            onNewHostedSession: { [weak self] in
                self?.showAddHostedWorkspacePanel()
            }
        )
        let sidebarHosting = NSHostingController(rootView: sidebarView)
        // Disable vibrancy for a solid dark sidebar
        let sidebarWrapper = NSViewController()
        sidebarWrapper.view = NSView()
        sidebarWrapper.view.wantsLayer = true
        sidebarWrapper.view.layer?.backgroundColor = NSColor(red: 0.15, green: 0.16, blue: 0.17, alpha: 1.0).cgColor
        sidebarHosting.view.translatesAutoresizingMaskIntoConstraints = false
        sidebarWrapper.view.addSubview(sidebarHosting.view)
        sidebarWrapper.addChild(sidebarHosting)
        NSLayoutConstraint.activate([
            sidebarHosting.view.topAnchor.constraint(equalTo: sidebarWrapper.view.topAnchor),
            sidebarHosting.view.bottomAnchor.constraint(equalTo: sidebarWrapper.view.bottomAnchor),
            sidebarHosting.view.leadingAnchor.constraint(equalTo: sidebarWrapper.view.leadingAnchor),
            sidebarHosting.view.trailingAnchor.constraint(equalTo: sidebarWrapper.view.trailingAnchor),
        ])
        let sidebarItem = NSSplitViewItem(sidebarWithViewController: sidebarWrapper)
        sidebarItem.minimumThickness = 180
        sidebarItem.maximumThickness = 300
        sidebarItem.canCollapse = true
        splitVC.addSplitViewItem(sidebarItem)

        // Main content — observes windowContext for which workspace to show.
        let mainView = MainContentView(
            manager: workspaceManager,
            windowContext: windowContext,
            remoteService: RemoteSessionService.shared
        )
        let mainHosting = NSHostingController(rootView: mainView)
        let mainItem = NSSplitViewItem(contentListWithViewController: mainHosting)
        mainItem.minimumThickness = 400
        splitVC.addSplitViewItem(mainItem)

        // Persist sidebar width across launches (shared across windows via UserDefaults).
        splitVC.splitView.autosaveName = "ccmux.mainSplit"

        window?.contentViewController = splitVC

        // Add antenna button in the titlebar (right side)
        let antennaButton = NSButton(frame: .zero)
        antennaButton.image = NSImage(systemSymbolName: "antenna.radiowaves.left.and.right", accessibilityDescription: "Peer Messages")
        antennaButton.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 11, weight: .regular)
        antennaButton.isBordered = false
        antennaButton.target = self
        antennaButton.action = #selector(togglePeerMessages)
        antennaButton.toolTip = "Peer Messages"
        antennaButton.contentTintColor = NSColor.white.withAlphaComponent(0.5)
        antennaButton.translatesAutoresizingMaskIntoConstraints = false

        let containerView = NSView(frame: NSRect(x: 0, y: 0, width: 36, height: 28))
        containerView.addSubview(antennaButton)
        NSLayoutConstraint.activate([
            antennaButton.centerXAnchor.constraint(equalTo: containerView.centerXAnchor),
            antennaButton.centerYAnchor.constraint(equalTo: containerView.centerYAnchor),
            antennaButton.widthAnchor.constraint(equalToConstant: 28),
            antennaButton.heightAnchor.constraint(equalToConstant: 28),
        ])

        let accessoryVC = NSTitlebarAccessoryViewController()
        accessoryVC.view = containerView
        accessoryVC.layoutAttribute = .trailing
        window?.addTitlebarAccessoryViewController(accessoryVC)
    }

    func showRenameWindowAlert(windowId: UUID, currentName: String) {
        let alert = NSAlert()
        alert.messageText = "Rename Window"
        alert.informativeText = "Enter a name for this window:"
        alert.addButton(withTitle: "Rename")
        alert.addButton(withTitle: "Cancel")

        let textField = NSTextField(frame: NSRect(x: 0, y: 0, width: 200, height: 24))
        textField.stringValue = currentName
        alert.accessoryView = textField

        alert.beginSheetModal(for: window!) { [weak self] response in
            if response == .alertFirstButtonReturn {
                let newName = textField.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
                self?.windowManager?.renameWindow(id: windowId, newName: newName.isEmpty ? nil : newName)
            }
        }
    }

    func showAddWorkspacePanel() {
        let panel = NSOpenPanel()
        panel.canChooseFiles = false
        panel.canChooseDirectories = true
        panel.allowsMultipleSelection = false
        panel.message = "Choose a project directory"

        panel.beginSheetModal(for: window!) { [weak self] result in
            guard result == .OK, let url = panel.url else { return }
            guard let self else { return }
            let name = url.lastPathComponent
            self.workspaceManager.addWorkspace(name: name, repoPath: url.path)

            // Show the new workspace in this window and claim ownership
            if let newId = self.workspaceManager.workspaces.last?.id {
                self.windowContext.displayedWorkspaceId = newId
                self.windowContext.ownedWorkspaceIds.insert(newId)
                self.updateWindowTitle()
                self.windowManager?.refreshOtherWindowIds()
            }
        }
    }

    // MARK: - NSWindowDelegate

    func windowDidResize(_ notification: Notification) {
        workspaceManager.scheduleSaveFromWindow()
    }

    func windowDidMove(_ notification: Notification) {
        workspaceManager.scheduleSaveFromWindow()
    }

    func windowWillClose(_ notification: Notification) {
        peerMessagesController.dismiss()
        windowManager?.windowWillClose(self)
    }

    func windowDidBecomeKey(_ notification: Notification) {
        // Update global activeWorkspaceId for menu actions
        if let wsId = windowContext.displayedWorkspaceId {
            workspaceManager.activeWorkspaceId = wsId
            // Returning focus to a window counts as "seeing" the workspace it shows —
            // clear any attention flash so a Cmd-Tab back stops the pulse.
            workspaceManager.attentionMonitors[wsId]?.clear()
            RemoteSessionService.shared.attentionMonitors[wsId]?.clear()
            // Cheap belt-and-suspenders for "user just Cmd-Tabbed back from an
            // external editor" — FSEvents would catch the change eventually,
            // but a focus-event refresh makes the sidebar feel instant and
            // covers any missed events on weird filesystems.
            workspaceManager.monitors[wsId]?.refresh()
        }
    }

    @objc func togglePeerMessages() {
        guard let wsId = windowContext.displayedWorkspaceId,
              let ws = workspaceManager.workspaces.first(where: { $0.id == wsId })
        else { return }
        let project = (ws.repoPath as NSString).deletingLastPathComponent
        peerMessagesController.toggle(project: project, relativeTo: window)
    }

    var activeController: SplitTreeController? {
        windowContext.displayedController
    }
}

/// SwiftUI view that shows the active workspace's split tree, or a welcome screen.
struct MainContentView: View {
    @ObservedObject var manager: WorkspaceManager
    @ObservedObject var windowContext: WindowContext
    /// Observed so the main view re-renders when a hosted workspace's controller is
    /// (re)built by the daemon poll.
    @ObservedObject var remoteService: RemoteSessionService

    var body: some View {
        if let controller = windowContext.displayedController {
            SplitTreeView(controller: controller)
                .id(windowContext.displayedWorkspaceId)
        } else {
            VStack(spacing: 16) {
                Image(systemName: "rectangle.split.3x1")
                    .font(.system(size: 48))
                    .foregroundColor(.secondary)
                Text("No workspace open")
                    .font(.system(size: 16, weight: .medium))
                    .foregroundColor(.secondary)
                Text("Add a workspace from the sidebar or press Cmd+O")
                    .font(.system(size: 12))
                    .foregroundColor(.secondary.opacity(0.7))
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Color(nsColor: NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)))
        }
    }
}

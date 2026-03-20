import AppKit
import SwiftUI
import Combine

class WorkspaceWindowController: NSWindowController, NSWindowDelegate {
    let workspaceManager: WorkspaceManager
    weak var windowManager: WindowManager?
    let windowContext: WindowContext
    let windowId: UUID

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
            styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
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
           let ws = workspaceManager.workspaces.first(where: { $0.id == wsId }) {
            window?.title = ws.name
        } else {
            window?.title = "ccmux"
        }
    }

    private func setupSplitView() {
        let splitVC = NSSplitViewController()

        // Sidebar — passes per-window context for selection
        let sidebarView = SidebarView(
            manager: workspaceManager,
            windowContext: windowContext,
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

        // Main content — observes windowContext for which workspace to show
        let mainView = MainContentView(
            manager: workspaceManager,
            windowContext: windowContext
        )
        let mainHosting = NSHostingController(rootView: mainView)
        let mainItem = NSSplitViewItem(contentListWithViewController: mainHosting)
        mainItem.minimumThickness = 400
        splitVC.addSplitViewItem(mainItem)

        window?.contentViewController = splitVC
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

            // Show the new workspace in this window
            if let newId = self.workspaceManager.workspaces.last?.id {
                self.windowContext.displayedWorkspaceId = newId
                self.updateWindowTitle()
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
        windowManager?.windowWillClose(self)
    }

    func windowDidBecomeKey(_ notification: Notification) {
        // Update global activeWorkspaceId for menu actions
        if let wsId = windowContext.displayedWorkspaceId {
            workspaceManager.activeWorkspaceId = wsId
        }
    }

    var activeController: SplitTreeController? {
        windowContext.displayedController
    }
}

/// SwiftUI view that shows the active workspace's split tree, or a welcome screen.
struct MainContentView: View {
    @ObservedObject var manager: WorkspaceManager
    @ObservedObject var windowContext: WindowContext

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

import AppKit
import Combine

/// Per-window state binding — each window creates one and passes it to its views.
class WindowContext: ObservableObject {
    @Published var displayedWorkspaceId: UUID?
    /// IDs of workspaces displayed in OTHER windows (for sidebar badges)
    @Published var otherWindowWorkspaceIds: Set<UUID> = []
    weak var workspaceManager: WorkspaceManager?

    var displayedController: SplitTreeController? {
        guard let id = displayedWorkspaceId else { return nil }
        return workspaceManager?.controllers[id]
    }

    init(workspaceId: UUID?, workspaceManager: WorkspaceManager) {
        self.displayedWorkspaceId = workspaceId
        self.workspaceManager = workspaceManager
    }
}

/// Manages multiple windows, each displaying a workspace.
/// Owns window controller references (keeps them alive) and handles
/// detach, workspace selection across windows, and persistence.
class WindowManager {
    let workspaceManager: WorkspaceManager
    private(set) var windowControllers: [WorkspaceWindowController] = []

    init(workspaceManager: WorkspaceManager) {
        self.workspaceManager = workspaceManager

        // When a workspace is removed, clear any window that was showing it
        workspaceManager.onWorkspaceRemoved = { [weak self] removedId in
            self?.handleWorkspaceRemoved(id: removedId)
        }
    }

    // MARK: - Window Lifecycle

    /// Create a new window displaying the given workspace (or nil for welcome screen).
    @discardableResult
    func createWindow(displayingWorkspace workspaceId: UUID?, frame: WindowFrame? = nil) -> WorkspaceWindowController {
        let wc = WorkspaceWindowController(
            workspaceManager: workspaceManager,
            windowManager: self,
            displayedWorkspaceId: workspaceId
        )

        if let frame {
            wc.window?.setFrame(
                NSRect(x: frame.x, y: frame.y, width: frame.width, height: frame.height),
                display: true
            )
        } else {
            wc.window?.center()
        }

        windowControllers.append(wc)
        wc.showWindow(nil)
        refreshOtherWindowIds()
        return wc
    }

    /// Detach a workspace into its own window.
    /// If already displayed in a window, bring that window to front.
    /// Uses the source window's frame for the new window.
    func detachWorkspace(id: UUID, sourceWindow: NSWindow? = nil) {
        if let existing = windowDisplaying(workspaceId: id) {
            existing.window?.makeKeyAndOrderFront(nil)
            return
        }
        let frame: WindowFrame?
        if let f = sourceWindow?.frame {
            frame = WindowFrame(x: f.origin.x + 30, y: f.origin.y - 30, width: f.size.width, height: f.size.height)
        } else {
            frame = nil
        }
        createWindow(displayingWorkspace: id, frame: frame)
    }

    /// Handle workspace selection from sidebar.
    /// If the workspace is already open in another window, bring that window to front.
    /// Otherwise, switch the requesting window to show it.
    func selectWorkspace(id: UUID, from requestingController: WorkspaceWindowController) {
        // Check if another window is already showing this workspace
        if let existing = windowDisplaying(workspaceId: id), existing !== requestingController {
            existing.window?.makeKeyAndOrderFront(nil)
            return
        }
        // Switch the requesting window
        requestingController.windowContext.displayedWorkspaceId = id
        requestingController.updateWindowTitle()

        // Update global activeWorkspaceId for compatibility
        workspaceManager.activeWorkspaceId = id
        refreshOtherWindowIds()
    }

    /// Called when a window is about to close.
    func windowWillClose(_ controller: WorkspaceWindowController) {
        // If this window has a workspace that no other window is displaying, close it
        if let wsId = controller.windowContext.displayedWorkspaceId {
            let otherWindowsShowingIt = windowControllers.contains { wc in
                wc !== controller && wc.windowContext.displayedWorkspaceId == wsId
            }
            if !otherWindowsShowingIt {
                // Save window frame before closing
                if let f = controller.window?.frame {
                    workspaceManager.saveWindowFrame(
                        workspaceId: wsId,
                        frame: WindowFrame(x: f.origin.x, y: f.origin.y, width: f.size.width, height: f.size.height)
                    )
                }
                workspaceManager.closeWorkspace(id: wsId)
            }
        }

        windowControllers.removeAll { $0 === controller }
        refreshOtherWindowIds()

        // Don't quit — app stays running so user can reopen from dock
    }

    /// Reopen a previously closed workspace in a new window with its saved frame.
    func reopenWorkspace(id: UUID) {
        guard let workspace = workspaceManager.reopenWorkspace(id: id) else { return }
        createWindow(displayingWorkspace: workspace.id, frame: workspace.lastWindowFrame)
    }

    // MARK: - Queries

    /// Find which window (if any) is currently displaying a given workspace.
    func windowDisplaying(workspaceId: UUID) -> WorkspaceWindowController? {
        windowControllers.first { $0.windowContext.displayedWorkspaceId == workspaceId }
    }

    // MARK: - Persistence

    /// Get current window descriptors for saving.
    func windowDescriptors() -> [WindowDescriptor] {
        windowControllers.compactMap { wc in
            guard let window = wc.window else { return nil }
            let f = window.frame
            return WindowDescriptor(
                id: wc.windowId,
                workspaceId: wc.windowContext.displayedWorkspaceId,
                frame: WindowFrame(x: f.origin.x, y: f.origin.y, width: f.size.width, height: f.size.height)
            )
        }
    }

    /// Restore windows from saved descriptors.
    func restoreWindows(from descriptors: [WindowDescriptor]) {
        for desc in descriptors {
            // Only restore if the workspace still exists
            if let wsId = desc.workspaceId,
               workspaceManager.workspaces.contains(where: { $0.id == wsId }) {
                let wc = WorkspaceWindowController(
                    workspaceManager: workspaceManager,
                    windowManager: self,
                    displayedWorkspaceId: wsId,
                    windowId: desc.id
                )
                wc.window?.setFrame(
                    NSRect(x: desc.frame.x, y: desc.frame.y, width: desc.frame.width, height: desc.frame.height),
                    display: true
                )
                windowControllers.append(wc)
                wc.showWindow(nil)
            }
        }

        // If no windows were restored, create one showing the first workspace
        if windowControllers.isEmpty {
            createWindow(displayingWorkspace: workspaceManager.workspaces.first?.id)
        }
    }

    /// Update all window contexts with which workspaces are in other windows.
    func refreshOtherWindowIds() {
        // Build a map of all displayed workspace IDs
        let allDisplayed: [(controller: WorkspaceWindowController, wsId: UUID)] = windowControllers.compactMap { wc in
            guard let wsId = wc.windowContext.displayedWorkspaceId else { return nil }
            return (wc, wsId)
        }

        for wc in windowControllers {
            let othersIds = Set(allDisplayed.compactMap { entry -> UUID? in
                entry.controller !== wc ? entry.wsId : nil
            })
            wc.windowContext.otherWindowWorkspaceIds = othersIds
        }
    }

    // MARK: - Private

    private func handleWorkspaceRemoved(id: UUID) {
        for wc in windowControllers {
            if wc.windowContext.displayedWorkspaceId == id {
                // Switch to another workspace, or show welcome screen
                let nextId = workspaceManager.workspaces.first?.id
                wc.windowContext.displayedWorkspaceId = nextId
                wc.updateWindowTitle()
            }
        }
    }
}

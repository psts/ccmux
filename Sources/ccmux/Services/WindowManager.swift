import AppKit
import Combine

/// Per-window state binding — each window creates one and passes it to its views.
class WindowContext: ObservableObject {
    @Published var displayedWorkspaceId: UUID?
    /// IDs of workspaces that belong to OTHER windows
    @Published var otherWindowWorkspaceIds: Set<UUID> = []
    /// IDs of workspaces that belong to THIS window (displayed + previously selected here)
    @Published var ownedWorkspaceIds: Set<UUID> = []
    weak var workspaceManager: WorkspaceManager?

    var displayedController: SplitTreeController? {
        guard let id = displayedWorkspaceId else { return nil }
        return workspaceManager?.controllers[id]
    }

    init(workspaceId: UUID?, workspaceManager: WorkspaceManager) {
        self.displayedWorkspaceId = workspaceId
        self.workspaceManager = workspaceManager
        if let id = workspaceId {
            ownedWorkspaceIds = [id]
        }
    }
}

/// Manages multiple windows, each displaying a workspace.
/// Owns window controller references (keeps them alive) and handles
/// detach, workspace selection across windows, and persistence.
class WindowManager {
    let workspaceManager: WorkspaceManager
    private(set) var windowControllers: [WorkspaceWindowController] = []

    /// Set to true during app termination to prevent windowWillClose from closing workspaces.
    var isTerminating = false

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
        if let ownerWc = windowOwning(workspaceId: id) {
            if ownerWc.windowContext.displayedWorkspaceId == id {
                // Already displayed in a window, bring it to front
                ownerWc.window?.makeKeyAndOrderFront(nil)
                return
            }
            // Remove ownership from source window
            ownerWc.windowContext.ownedWorkspaceIds.remove(id)
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
        // Check if another window owns this workspace
        if let ownerWc = windowOwning(workspaceId: id), ownerWc !== requestingController {
            ownerWc.window?.makeKeyAndOrderFront(nil)
            return
        }

        // Switch the requesting window and claim ownership
        requestingController.windowContext.displayedWorkspaceId = id
        requestingController.windowContext.ownedWorkspaceIds.insert(id)
        requestingController.updateWindowTitle()

        // Update global activeWorkspaceId for compatibility
        workspaceManager.activeWorkspaceId = id
        refreshOtherWindowIds()
    }

    /// Find which window owns (not just displays) a workspace.
    func windowOwning(workspaceId: UUID) -> WorkspaceWindowController? {
        windowControllers.first { $0.windowContext.ownedWorkspaceIds.contains(workspaceId) }
    }

    /// Called when a window is about to close.
    func windowWillClose(_ controller: WorkspaceWindowController) {
        // During app termination, don't close workspaces — they'll be saved as-is
        if !isTerminating {
            // Close ALL owned workspaces that aren't owned by another window
            let ownedIds = controller.windowContext.ownedWorkspaceIds
            for wsId in ownedIds {
                let ownedElsewhere = windowControllers.contains { wc in
                    wc !== controller && wc.windowContext.ownedWorkspaceIds.contains(wsId)
                }
                if !ownedElsewhere {
                    // Save window frame for the displayed workspace
                    if wsId == controller.windowContext.displayedWorkspaceId,
                       let f = controller.window?.frame {
                        workspaceManager.saveWindowFrame(
                            workspaceId: wsId,
                            frame: WindowFrame(x: f.origin.x, y: f.origin.y, width: f.size.width, height: f.size.height)
                        )
                    }
                    workspaceManager.closeWorkspace(id: wsId)
                }
            }
        }

        windowControllers.removeAll { $0 === controller }
        refreshOtherWindowIds()

        // Don't quit — app stays running so user can reopen from dock
    }

    /// Move a workspace from its current window into the requesting window.
    /// Closes the source window if it only had that workspace.
    func moveWorkspaceToWindow(id: UUID, targetController: WorkspaceWindowController) {
        // Remove ownership from any other window
        for wc in windowControllers where wc !== targetController {
            wc.windowContext.ownedWorkspaceIds.remove(id)
            // If source window was displaying this workspace and has no other owned workspaces, close it
            if wc.windowContext.displayedWorkspaceId == id {
                if let otherOwned = wc.windowContext.ownedWorkspaceIds.first {
                    wc.windowContext.displayedWorkspaceId = otherOwned
                    wc.updateWindowTitle()
                } else {
                    wc.windowContext.displayedWorkspaceId = nil
                    windowControllers.removeAll { $0 === wc }
                    wc.window?.close()
                }
            }
        }

        // Switch target window to display and own this workspace
        targetController.windowContext.displayedWorkspaceId = id
        targetController.windowContext.ownedWorkspaceIds.insert(id)
        targetController.updateWindowTitle()
        workspaceManager.activeWorkspaceId = id
        refreshOtherWindowIds()
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

        // Assign unowned workspaces to the first window
        let allOwned = windowControllers.reduce(into: Set<UUID>()) { $0.formUnion($1.windowContext.ownedWorkspaceIds) }
        if let firstWc = windowControllers.first {
            for ws in workspaceManager.workspaces where !allOwned.contains(ws.id) {
                firstWc.windowContext.ownedWorkspaceIds.insert(ws.id)
            }
        }

        // Tell all windows about each other's workspaces
        refreshOtherWindowIds()
    }

    /// Update all window contexts with which workspaces belong to other windows.
    func refreshOtherWindowIds() {
        for wc in windowControllers {
            var otherIds = Set<UUID>()
            for otherWc in windowControllers where otherWc !== wc {
                otherIds.formUnion(otherWc.windowContext.ownedWorkspaceIds)
            }
            wc.windowContext.otherWindowWorkspaceIds = otherIds
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

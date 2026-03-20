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

    /// Windows assigned to other Spaces but not yet ordered. Keyed by target CGS Space ID.
    /// Ordered when the user navigates to the target Space.
    private var pendingSpaceWindows: [size_t: [WorkspaceWindowController]] = [:]
    private var spaceChangeObserver: NSObjectProtocol?

    init(workspaceManager: WorkspaceManager) {
        self.workspaceManager = workspaceManager
        setupSpaceChangeObserver()

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
            // Switch that window to display the clicked workspace
            ownerWc.windowContext.displayedWorkspaceId = id
            ownerWc.updateWindowTitle()
            // Bring it to front (switches Space if needed via FocusWindowCommand-style behavior)
            NSApp.activate(ignoringOtherApps: true)
            if let window = ownerWc.window {
                let originalBehavior = window.collectionBehavior
                window.collectionBehavior.insert(.canJoinAllSpaces)
                window.makeKeyAndOrderFront(nil)
                window.orderFrontRegardless()
                DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
                    window.collectionBehavior = originalBehavior
                }
            }
            workspaceManager.activeWorkspaceId = id
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

    /// Get current window descriptors for saving (including Space snapshot).
    func windowDescriptors() -> [WindowDescriptor] {
        windowControllers.compactMap { wc in
            guard let window = wc.window else { return nil }
            let f = window.frame
            let space = SpaceTracker.spaceSnapshot(for: window)
            return WindowDescriptor(
                id: wc.windowId,
                workspaceId: wc.windowContext.displayedWorkspaceId,
                frame: WindowFrame(x: f.origin.x, y: f.origin.y, width: f.size.width, height: f.size.height),
                space: space
            )
        }
    }

    /// Restore windows from saved descriptors, placing each on its saved Space.
    ///
    /// Critical sequence (from old cmux app analysis):
    /// 1. Create window (defer: false gives valid windowNumber immediately)
    /// 2. Resolve target Space from saved snapshot
    /// 3. If other Space: CGS pre-assign, do NOT order (no showWindow/orderFront)
    /// 4. If current Space: makeKeyAndOrderFront directly on NSWindow
    /// 5. THEN set frame with display: true (after ordering decision)
    ///
    /// Pending (other-Space) windows are ordered when the user navigates to that Space
    /// via NSWorkspace.activeSpaceDidChangeNotification.
    func restoreWindows(from descriptors: [WindowDescriptor]) {
        let currentSpaceID = SpaceTracker.currentSpaceID()

        for desc in descriptors {
            // Only restore if the workspace still exists
            if let wsId = desc.workspaceId,
               workspaceManager.workspaces.contains(where: { $0.id == wsId }) {
                // Step 1: Create window (defer: false → valid windowNumber)
                let wc = WorkspaceWindowController(
                    workspaceManager: workspaceManager,
                    windowManager: self,
                    displayedWorkspaceId: wsId,
                    windowId: desc.id
                )

                // Step 2: Resolve target Space
                let targetSpaceID: size_t? = {
                    guard let spaceSnap = desc.space else { return nil }
                    return SpaceTracker.resolveSpaceID(from: spaceSnap)
                }()
                let isOtherSpace = targetSpaceID != nil && targetSpaceID != currentSpaceID

                windowControllers.append(wc)

                // Step 3/4: Order or defer based on Space
                if isOtherSpace, let targetSpaceID, let window = wc.window {
                    // OTHER SPACE: Pre-assign to target Space BEFORE any ordering.
                    // The window has a valid windowNumber from defer:false but is not yet
                    // on any space. Assigning first means it won't auto-place on current space.
                    SpaceTracker.assignWindowToSpace(window, spaceID: targetSpaceID)
                    // Do NOT order — any ordering triggers macOS to switch Spaces.
                    pendingSpaceWindows[targetSpaceID, default: []].append(wc)
                } else if let window = wc.window {
                    // CURRENT SPACE (or no Space info): Order directly on NSWindow
                    // Use makeKeyAndOrderFront, not showWindow (which triggers extra WC machinery)
                    window.makeKeyAndOrderFront(nil)
                }

                // Step 5: Apply frame AFTER ordering decision
                let frame = NSRect(x: desc.frame.x, y: desc.frame.y,
                                   width: desc.frame.width, height: desc.frame.height)
                wc.window?.setFrame(frame, display: true)
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

        // Schedule fallback: if the user is already on a target Space,
        // no activeSpaceDidChangeNotification will fire. Force-order after a delay.
        schedulePendingWindowFallback()
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

    // MARK: - Space Management

    /// Observe Space changes to order pending windows when the user switches to their Space.
    private func setupSpaceChangeObserver() {
        spaceChangeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.activeSpaceDidChangeNotification,
            object: nil,
            queue: .main
        ) { [weak self] _ in
            self?.orderPendingWindowsForCurrentSpace()
        }
    }

    /// Schedule a fallback to order all pending windows after a delay.
    /// Handles the case where the user is already on the target Space when the app launches
    /// (no Space change notification will fire).
    func schedulePendingWindowFallback() {
        guard !pendingSpaceWindows.isEmpty else { return }
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { [weak self] in
            self?.forceOrderAllPendingWindows()
        }
    }

    /// When the user switches to a Space, order ALL pending windows.
    /// We order all of them regardless of Space ID matching because:
    /// - Space IDs can drift between save and restore
    /// - Windows already assigned to the wrong Space simply won't be visible
    /// - This ensures no window gets stuck as "pending" forever
    private func orderPendingWindowsForCurrentSpace() {
        forceOrderAllPendingWindows()
    }

    /// Force-order all pending windows immediately.
    private func forceOrderAllPendingWindows() {
        guard !pendingSpaceWindows.isEmpty else { return }
        let allPending = pendingSpaceWindows.values.flatMap { $0 }
        pendingSpaceWindows.removeAll()
        for wc in allPending {
            wc.window?.orderFront(nil)
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

    deinit {
        if let observer = spaceChangeObserver {
            NSWorkspace.shared.notificationCenter.removeObserver(observer)
        }
    }
}

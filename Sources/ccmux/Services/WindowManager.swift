import AppKit
import Combine

/// Per-window state binding — each window creates one and passes it to its views.
class WindowContext: ObservableObject {
    @Published var displayedWorkspaceId: UUID?
    /// IDs of workspaces that belong to OTHER windows, grouped by window
    @Published var otherWindowWorkspaceIds: Set<UUID> = []
    /// IDs of workspaces that belong to THIS window (displayed + previously selected here)
    @Published var ownedWorkspaceIds: Set<UUID> = []
    /// Custom window name, nil = auto-generated "Window N"
    @Published var windowName: String?
    /// Info about other windows for sidebar display
    @Published var otherWindowGroups: [WindowGroup] = []
    /// Workspace IDs whose sidebar disclosure is collapsed in this window.
    /// Persisted via WindowDescriptor; only consulted for the current window's
    /// own rows (other windows render collapsed regardless).
    @Published var collapsedWorkspaceIds: Set<UUID> = []
    weak var workspaceManager: WorkspaceManager?

    struct WindowGroup: Identifiable {
        let id: UUID  // window ID
        let name: String
        let workspaceIds: [UUID]
    }

    var displayedController: SplitTreeController? {
        guard let id = displayedWorkspaceId else { return nil }
        // Local workspaces resolve from WorkspaceManager; hosted workspaces (the lens
        // pivot) resolve from RemoteSessionService, which renders through the same
        // SplitTree machinery.
        return workspaceManager?.controllers[id] ?? RemoteSessionService.shared.controllers[id]
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

    // Local-pane→window map sync state (see syncLocalPaneGroups).
    private var localGroupsTimer: Timer?
    private var lastLocalGroups: [String: String] = [:]
    private var lastLocalGroupsPush: Date = .distantPast

    init(workspaceManager: WorkspaceManager) {
        self.workspaceManager = workspaceManager
        setupSpaceChangeObserver()

        // When a workspace is removed, clear any window that was showing it
        workspaceManager.onWorkspaceRemoved = { [weak self] removedId in
            self?.handleWorkspaceRemoved(id: removedId)
        }

        // Local-pane group sync also runs on a timer: pane splits/closes don't
        // pass through refreshOtherWindowIds, and a periodic push re-seeds a
        // restarted daemon (the map is daemon-memory only).
        localGroupsTimer = Timer.scheduledTimer(withTimeInterval: 15, repeats: true) { [weak self] _ in
            self?.syncLocalPaneGroups()
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
        // Switching to a workspace is the "I've seen it" acknowledgment — clear any
        // flash. Hosted workspaces flash via RemoteSessionService's monitors.
        workspaceManager.attentionMonitors[id]?.clear()
        RemoteSessionService.shared.attentionMonitors[id]?.clear()

        // Check if another window owns this workspace
        if let ownerWc = windowOwning(workspaceId: id), ownerWc !== requestingController {
            // Switch that window to display the clicked workspace
            ownerWc.windowContext.displayedWorkspaceId = id
            ownerWc.updateWindowTitle()
            // Bring that window to front — macOS switches to its Space automatically
            if let window = ownerWc.window {
                NSApp.activate(ignoringOtherApps: true)
                window.makeKeyAndOrderFront(nil)
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

    /// Measured content-area size of the window owning `workspaceId`, or nil if the window
    /// isn't laid out yet (e.g. pre-assigned to another Space). The main content split item
    /// is the terminal area shared by all workspaces owned by that window — used to size
    /// off-screen terminals before replaying their startup command on relaunch.
    func contentSize(forWorkspace workspaceId: UUID) -> CGSize? {
        guard let wc = windowOwning(workspaceId: workspaceId),
              let split = wc.window?.contentViewController as? NSSplitViewController,
              split.splitViewItems.count > 1 else { return nil }
        let size = split.splitViewItems[1].viewController.view.bounds.size
        guard size.width > 0, size.height > 0 else { return nil }
        return size
    }

    /// Called when a window is about to close.
    func windowWillClose(_ controller: WorkspaceWindowController) {
        // During app termination, don't close workspaces — they'll be saved as-is
        if !isTerminating {
            // Collect workspace IDs that will actually be closed (not owned by other windows).
            // Seed with the displayed workspace too — ownership tracking has historically had
            // gaps (detach/move paths can drop a workspace from ownedWorkspaceIds while it's
            // still on screen), and a leaked workspace there means leaked PTYs/child processes.
            var ownedIds = controller.windowContext.ownedWorkspaceIds
            if let displayed = controller.windowContext.displayedWorkspaceId {
                ownedIds.insert(displayed)
            }
            var closingIds: [UUID] = []

            for wsId in ownedIds {
                // Hosted workspaces live on the daemon, not in this window — closing
                // the window must not close (or archive) them. They become unowned
                // and are re-adopted into the first window on the next reconcile.
                if RemoteSessionService.shared.isHosted(wsId) {
                    continue
                }
                let ownedElsewhere = windowControllers.contains { wc in
                    wc !== controller && wc.windowContext.ownedWorkspaceIds.contains(wsId)
                }
                if !ownedElsewhere {
                    closingIds.append(wsId)
                }
            }

            // Save as a closed window group if it had workspaces
            if !closingIds.isEmpty {
                let frame = controller.window?.frame ?? NSRect(x: 100, y: 100, width: 1200, height: 800)
                let closedWindow = ClosedWindow(
                    id: UUID(),
                    windowName: controller.windowContext.windowName,
                    workspaceIds: closingIds,
                    displayedWorkspaceId: controller.windowContext.displayedWorkspaceId,
                    frame: WindowFrame(x: frame.origin.x, y: frame.origin.y,
                                       width: frame.size.width, height: frame.size.height)
                )
                workspaceManager.saveClosedWindow(closedWindow)
            }

            // Now close the individual workspaces
            for wsId in closingIds {
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
            wc.windowContext.collapsedWorkspaceIds.remove(id)
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

    /// Restore an entire closed window with all its workspaces.
    func restoreClosedWindow(id: UUID) {
        guard let closedWindow = workspaceManager.closedWindows.first(where: { $0.id == id }) else { return }

        // Reopen all workspaces that belong to this window
        var reopenedIds: [UUID] = []
        for wsId in closedWindow.workspaceIds {
            if let _ = workspaceManager.reopenWorkspace(id: wsId) {
                reopenedIds.append(wsId)
            }
        }

        guard !reopenedIds.isEmpty else { return }

        // Create a new window displaying the previously displayed workspace (or first)
        let displayId = closedWindow.displayedWorkspaceId ?? reopenedIds.first!
        let wc = createWindow(displayingWorkspace: displayId, frame: closedWindow.frame)

        // Assign all reopened workspaces to this window
        wc.windowContext.ownedWorkspaceIds = Set(reopenedIds)
        wc.windowContext.windowName = closedWindow.windowName

        // Remove the closed window record
        workspaceManager.closedWindows.removeAll { $0.id == id }

        refreshOtherWindowIds()
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
                ownedWorkspaceIds: Array(wc.windowContext.ownedWorkspaceIds),
                windowName: wc.windowContext.windowName,
                frame: WindowFrame(x: f.origin.x, y: f.origin.y, width: f.size.width, height: f.size.height),
                space: space,
                collapsedWorkspaceIds: Array(wc.windowContext.collapsedWorkspaceIds)
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
            // Restore if the displayed workspace exists locally — or if the window
            // owns anything at all: a hosted displayed workspace can't be checked
            // here (the daemon list arrives async after startup), so the window gets
            // the benefit of the doubt and shows the welcome screen until the hosted
            // workspace materializes.
            let displayedExistsLocally = desc.workspaceId.map { id in
                workspaceManager.workspaces.contains { $0.id == id }
            } ?? false
            if let wsId = desc.workspaceId,
               displayedExistsLocally || !desc.ownedWorkspaceIds.isEmpty {
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

                // Restore ownership from saved state
                if !desc.ownedWorkspaceIds.isEmpty {
                    wc.windowContext.ownedWorkspaceIds = Set(desc.ownedWorkspaceIds)
                }
                wc.windowContext.windowName = desc.windowName
                wc.windowContext.collapsedWorkspaceIds = Set(desc.collapsedWorkspaceIds)

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

    /// Give every hosted workspace a window: any id no window owns yet is adopted
    /// by the first window (mirroring the unowned-local assignment in
    /// `restoreWindows`). Called after each daemon reconcile, so sessions created
    /// by other lenses (web, phone, another Mac) always land in a sidebar group.
    func adoptOrphanHostedWorkspaces() {
        guard let firstWc = windowControllers.first else { return }
        var adopted = false
        for ws in RemoteSessionService.shared.workspaces where windowOwning(workspaceId: ws.id) == nil {
            firstWc.windowContext.ownedWorkspaceIds.insert(ws.id)
            adopted = true
        }
        if adopted { refreshOtherWindowIds() }
    }

    /// A hosted workspace is genuinely gone from the daemon (removed by some lens):
    /// drop it from every window; a window displaying it falls back to its next
    /// owned workspace (or the welcome screen).
    func hostedWorkspaceRemoved(id: UUID) {
        for wc in windowControllers {
            wc.windowContext.ownedWorkspaceIds.remove(id)
            wc.windowContext.collapsedWorkspaceIds.remove(id)
            if wc.windowContext.displayedWorkspaceId == id {
                wc.windowContext.displayedWorkspaceId = wc.windowContext.ownedWorkspaceIds.first
                wc.updateWindowTitle()
            }
        }
        refreshOtherWindowIds()
    }

    /// Update all window contexts with which workspaces belong to other windows.
    func refreshOtherWindowIds() {
        for wc in windowControllers {
            var otherIds = Set<UUID>()
            var otherGroups: [WindowContext.WindowGroup] = []
            for otherWc in windowControllers where otherWc !== wc {
                otherIds.formUnion(otherWc.windowContext.ownedWorkspaceIds)
                let name = otherWc.windowContext.windowName ?? autoWindowName(for: otherWc)
                otherGroups.append(WindowContext.WindowGroup(
                    id: otherWc.windowId,
                    name: name,
                    workspaceIds: Array(otherWc.windowContext.ownedWorkspaceIds)
                ))
            }
            wc.windowContext.otherWindowWorkspaceIds = otherIds
            wc.windowContext.otherWindowGroups = otherGroups
            // Auto-assign window name if not custom
            if wc.windowContext.windowName == nil {
                // Don't publish — just for display fallback
            }
        }
        syncHostedGroups()
        syncLocalPaneGroups()
    }

    /// Push each hosted workspace's owning-window name to the daemon as its
    /// shared `group`, so non-Mac lenses (web/phone) render the same sidebar
    /// grouping. This Mac is the source of truth; the diff against the daemon's
    /// last-known value keeps steady-state silent. Runs on every ownership/name
    /// change via refreshOtherWindowIds (move, detach, adopt, create, rename).
    private func syncHostedGroups() {
        let service = RemoteSessionService.shared
        for wc in windowControllers {
            let name = wc.windowContext.windowName ?? autoWindowName(for: wc)
            for wsId in wc.windowContext.ownedWorkspaceIds
            where service.isHosted(wsId) && service.groups[wsId] != name {
                Task { await service.setGroup(wsId, to: name) }
            }
        }
    }

    /// Push the complete local-pane→window-name map to the daemon's peers bus,
    /// so sessions in Mac-local (driver-mode) panes get window grouping too —
    /// the same source-of-truth pattern as syncHostedGroups, keyed by the pane
    /// UUID the session's thin client reads from CCMUX_CMD_FILE. Diff-gated,
    /// with a periodic unconditional push so a restarted daemon (in-memory map)
    /// re-seeds within a minute.
    func syncLocalPaneGroups() {
        var map: [String: String] = [:]
        for wc in windowControllers {
            let name = wc.windowContext.windowName ?? autoWindowName(for: wc)
            for wsId in wc.windowContext.ownedWorkspaceIds {
                guard let ws = workspaceManager.workspaces.first(where: { $0.id == wsId }),
                      ws.mode == .local else { continue }
                for leaf in ws.layout.allLeaves {
                    for tab in leaf.content.tabs {
                        if case .terminal(let cfg) = tab {
                            map[cfg.id.uuidString] = name
                        }
                    }
                }
            }
        }
        let stale = Date().timeIntervalSince(lastLocalGroupsPush) > 60
        guard map != lastLocalGroups || stale else { return }
        lastLocalGroups = map
        lastLocalGroupsPush = Date()
        Task { await PeerBrokerService.shared.pushLocalGroups(map) }
    }

    /// Rename a window.
    func renameWindow(id: UUID, newName: String?) {
        guard let wc = windowControllers.first(where: { $0.windowId == id }) else { return }
        wc.windowContext.windowName = newName
        refreshOtherWindowIds()
    }

    /// Get the auto-generated name for a window based on its index.
    func autoWindowName(for controller: WorkspaceWindowController) -> String {
        if let idx = windowControllers.firstIndex(where: { $0 === controller }) {
            return "Window \(idx + 1)"
        }
        return "Window"
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
            wc.windowContext.collapsedWorkspaceIds.remove(id)
        }
    }

    deinit {
        if let observer = spaceChangeObserver {
            NSWorkspace.shared.notificationCenter.removeObserver(observer)
        }
    }
}

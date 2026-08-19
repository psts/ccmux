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

    struct WindowGroup: Identifiable, Equatable {
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
    /// `name` is applied before the window joins `windowControllers`, because the
    /// `refreshOtherWindowIds` below pushes each window's name to the daemon as its
    /// hosted group. Naming afterwards means the auto "Window N" goes out first, and
    /// the corrective push races it from a separate unstructured Task.
    @discardableResult
    func createWindow(
        displayingWorkspace workspaceId: UUID?, frame: WindowFrame? = nil, name: String? = nil
    ) -> WorkspaceWindowController {
        let wc = WorkspaceWindowController(
            workspaceManager: workspaceManager,
            windowManager: self,
            displayedWorkspaceId: workspaceId
        )
        wc.windowContext.windowName = name

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
            let others = ownerWc.windowContext.ownedWorkspaceIds.subtracting([id])
            let decision = Self.detachDecision(
                askedByOwner: sourceWindow != nil && ownerWc.window === sourceWindow,
                ownerDisplaysIt: ownerWc.windowContext.displayedWorkspaceId == id,
                ownerHasOthers: !others.isEmpty)
            switch decision {
            case .revealOwner:
                ownerWc.window?.makeKeyAndOrderFront(nil)
                return
            case .detach(let repointOwner):
                ownerWc.windowContext.ownedWorkspaceIds.remove(id)
                if repointOwner {
                    ownerWc.windowContext.displayedWorkspaceId = others.first
                    ownerWc.updateWindowTitle()
                }
            }
        }
        let frame: WindowFrame?
        if let f = sourceWindow?.frame {
            frame = WindowFrame(x: f.origin.x + 30, y: f.origin.y - 30, width: f.size.width, height: f.size.height)
        } else {
            frame = nil
        }
        createWindow(displayingWorkspace: id, frame: frame)
        workspaceManager.scheduleSaveFromWindow()
    }

    /// What "Open in New Window" should do.
    enum DetachDecision: Equatable {
        /// Reveal the window that already shows it rather than making a second window
        /// for one session.
        case revealOwner
        /// Move it into a new window. `repointOwner` when the owner was showing it and
        /// has something else left to show.
        case detach(repointOwner: Bool)
    }

    /// Pure decision behind `detachWorkspace`.
    ///
    /// The case that used to read as a dead menu item: the owner IS the window asking,
    /// so fronting it changes nothing the user can see. The case that must NOT detach:
    /// the workspace is the owner's only one, so a new window would just move it
    /// sideways, discard the old window's name, and leave an empty descriptor behind.
    /// It is already alone in its own window — revealing is the honest answer.
    static func detachDecision(
        askedByOwner: Bool, ownerDisplaysIt: Bool, ownerHasOthers: Bool
    ) -> DetachDecision {
        guard askedByOwner else {
            return ownerDisplaysIt ? .revealOwner : .detach(repointOwner: false)
        }
        guard ownerHasOthers else { return .revealOwner }
        return .detach(repointOwner: ownerDisplaysIt)
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
            let plan = Self.closingPlan(
                owned: controller.windowContext.ownedWorkspaceIds,
                displayed: controller.windowContext.displayedWorkspaceId,
                isHosted: { RemoteSessionService.shared.isHosted($0) },
                isOwnedElsewhere: { wsId in
                    windowControllers.contains { wc in
                        wc !== controller && wc.windowContext.ownedWorkspaceIds.contains(wsId)
                    }
                })
            archiveAndRecord(controller: controller, plan: plan)

            // Now close the individual workspaces
            for wsId in plan.closeLocally {
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

    /// Put the window's hosted sessions to sleep and write down how to bring the whole
    /// window back.
    ///
    /// Archiving is what makes "closing a window closes what is in it" true: tmux dies,
    /// the recipe survives, and each session shows as a cold row until Restore Window
    /// revives it. A failure is logged rather than swallowed — the session simply stays
    /// live, and while another window is open the orphan sweep re-homes it, but with the
    /// last window gone there is nobody to adopt it and the log is the only trace.
    private func archiveAndRecord(
        controller: WorkspaceWindowController,
        plan: (closeLocally: [UUID], archiveHosted: [UUID], record: [UUID])
    ) {
        for wsId in plan.archiveHosted {
            Task { @MainActor in
                if let error = await RemoteSessionService.shared.archiveWorkspace(wsId) {
                    NSLog("[ccmux] closing window: archiving %@ failed: %@", wsId.uuidString, error)
                }
            }
        }

        // The window that grouped these (its name, frame, and membership) is ours to
        // remember; without the record there is nothing for Restore Window to rebuild.
        guard !plan.record.isEmpty else { return }
        let frame = controller.window?.frame ?? NSRect(x: 100, y: 100, width: 1200, height: 800)
        workspaceManager.saveClosedWindow(ClosedWindow(
            id: UUID(),
            windowName: controller.windowContext.windowName,
            workspaceIds: plan.record,
            displayedWorkspaceId: controller.windowContext.displayedWorkspaceId,
            frame: WindowFrame(x: frame.origin.x, y: frame.origin.y,
                               width: frame.size.width, height: frame.size.height)))
    }

    /// What a closing window does with its workspaces.
    ///
    /// Closing a window closes what is in it. `closeLocally` are local workspaces this
    /// window is the last owner of, whose panes it tears down; `archiveHosted` are its
    /// hosted sessions, which are archived on the daemon — tmux killed, recipe kept —
    /// so nothing is left running with nowhere to appear. `record` is what the
    /// closed-window entry remembers so "Restore Window" can rebuild the window and
    /// bring both kinds back.
    ///
    /// This is deliberately NOT what quitting does: `windowWillClose` skips the whole
    /// path when `isTerminating`, so ⌘Q leaves every hosted session running.
    ///
    /// The displayed workspace is folded in because ownership tracking has historically
    /// had gaps — detach/move paths can drop a workspace from `ownedWorkspaceIds` while
    /// it is still on screen, and a leaked one there means leaked PTYs.
    static func closingPlan(
        owned: Set<UUID>, displayed: UUID?,
        isHosted: (UUID) -> Bool, isOwnedElsewhere: (UUID) -> Bool
    ) -> (closeLocally: [UUID], archiveHosted: [UUID], record: [UUID]) {
        var ids = owned
        if let displayed { ids.insert(displayed) }
        var closeLocally: [UUID] = []
        var archiveHosted: [UUID] = []
        var record: [UUID] = []
        for id in ids where !isOwnedElsewhere(id) {
            if isHosted(id) { archiveHosted.append(id) } else { closeLocally.append(id) }
            record.append(id)
        }
        return (closeLocally, archiveHosted, record)
    }

    /// What to do with a closed-window record after trying to restore it.
    enum RecordFate: Equatable {
        /// Nothing resolved and the daemon has not answered — keep it and try later.
        case keep
        /// Everything resolved (or the daemon says the rest is genuinely gone).
        case forget
        /// Some resolved; the record shrinks to the ids that did not.
        case keepUnresolved([UUID])
    }

    /// Whether restoring consumes the record, shrinks it, or leaves it alone.
    ///
    /// Consuming it wholesale is the trap: a record holding both local and hosted ids,
    /// restored while the daemon list has not landed (a daemon restart — which a fleet
    /// upgrade causes), resolves the local ones and would delete the hosted ids with the
    /// record. The sessions survive as cold rows, but the grouping the window existed to
    /// remember is gone for good. So unresolved ids stay on record unless the daemon has
    /// answered and simply does not have them.
    static func recordFate(savedIds: [UUID], restored: [UUID], daemonAnswered: Bool) -> RecordFate {
        let unresolved = savedIds.filter { !restored.contains($0) }
        if unresolved.isEmpty { return .forget }
        if daemonAnswered { return .forget }   // it answered and does not know them: really gone
        return restored.isEmpty ? .keep : .keepUnresolved(unresolved)
    }

    /// Whether a workspace leaving the live list should be struck from closed-window
    /// records. Only when the daemon no longer knows it at all: archiving also drops it
    /// from that list, and a window close archives everything it held, so pruning on
    /// mere absence would delete the record that restores it seconds after writing it.
    static func shouldForgetRecordReferences(isKnownHosted: Bool) -> Bool { !isKnownHosted }

    /// Split a closed window's saved ids by how each one comes back: hosted sessions are
    /// re-claimed live from the daemon, local ones are reopened from `closedWorkspaces`.
    ///
    /// Ids matching neither are dropped. A hosted session deleted or archived while the
    /// window was closed would otherwise be claimed into `ownedWorkspaceIds`, persisted
    /// into the window descriptor, and never cleaned up — the orphan sweep only iterates
    /// ids the daemon still lists.
    static func restorePlan(
        ids: [UUID], isHosted: (UUID) -> Bool, isReopenable: (UUID) -> Bool
    ) -> (hosted: [UUID], local: [UUID]) {
        var hosted: [UUID] = []
        var local: [UUID] = []
        for id in ids {
            if isHosted(id) { hosted.append(id) } else if isReopenable(id) { local.append(id) }
        }
        return (hosted, local)
    }

    /// Move a workspace from its current window into the requesting window.
    /// Closes the source window if it only had that workspace.
    func moveWorkspaceToWindow(id: UUID, targetController: WorkspaceWindowController) {
        // Moving counts as "I've seen it" just like clicking does — the flash
        // must not follow the workspace into its new window (see selectWorkspace).
        workspaceManager.attentionMonitors[id]?.clear()
        RemoteSessionService.shared.attentionMonitors[id]?.clear()

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
        // Only mark it active when the target is where the user actually is: a
        // drag dropped on ANOTHER window's section must not point menu actions
        // at a workspace the key window doesn't display.
        if targetController.window?.isKeyWindow ?? false {
            workspaceManager.activeWorkspaceId = id
        }
        refreshOtherWindowIds()
        // Persist now — without this a move only survives a clean quit.
        workspaceManager.scheduleSaveFromWindow()
    }

    /// Drag-and-drop entry: move a workspace onto a sidebar window SECTION,
    /// identified by window id. Same-window drops are a silent no-op (sections
    /// are name-sorted, so there is no position to change); a drop on a window
    /// that vanished mid-drag logs — that failure would otherwise be
    /// indistinguishable from the intended no-op.
    func moveWorkspace(id: UUID, toWindowId windowId: UUID) {
        guard let target = windowControllers.first(where: { $0.windowId == windowId }) else {
            NSLog("[ccmux drag] move failed: no window \(windowId)")
            return
        }
        guard !target.windowContext.ownedWorkspaceIds.contains(id) else { return }
        moveWorkspaceToWindow(id: id, targetController: target)
    }

    /// Reopen a previously closed workspace in a new window with its saved frame.
    func reopenWorkspace(id: UUID) {
        guard let workspace = workspaceManager.reopenWorkspace(id: id) else { return }
        createWindow(displayingWorkspace: workspace.id, frame: workspace.lastWindowFrame)
    }

    /// Restore an entire closed window with all its workspaces.
    func restoreClosedWindow(id: UUID) {
        guard let closedWindow = workspaceManager.closedWindows.first(where: { $0.id == id }) else { return }

        let service = RemoteSessionService.shared
        let plan = Self.restorePlan(
            ids: closedWindow.workspaceIds,
            // Cold counts: closing the window archived these, so the ones being restored
            // are normally cold. They are revived below.
            isHosted: { service.isKnownHosted($0) },
            isReopenable: { wsId in workspaceManager.closedWorkspaces.contains { $0.id == wsId } })
        for wsId in plan.local { _ = workspaceManager.reopenWorkspace(id: wsId) }

        let restored = plan.hosted + plan.local
        switch Self.recordFate(
            savedIds: closedWindow.workspaceIds, restored: restored, daemonAnswered: service.reachable) {
        case .keep:
            return                                            // nothing resolved yet; try again later
        case .forget:
            workspaceManager.forgetClosedWindow(id: id)
        case .keepUnresolved(let remaining):
            workspaceManager.replaceClosedWindowMembers(id: id, with: remaining)
        }
        guard !restored.isEmpty else { return }

        // Display what was on screen when it closed, unless that one didn't come back.
        let displayId = closedWindow.displayedWorkspaceId.flatMap { restored.contains($0) ? $0 : nil }
            ?? restored[0]
        let wc = createWindow(
            displayingWorkspace: displayId, frame: closedWindow.frame, name: closedWindow.windowName)
        wc.windowContext.ownedWorkspaceIds = Set(restored)
        // Claim exclusively rather than inserting: a session that stayed live (an
        // archive that failed, or one revived by hand) may have been adopted elsewhere,
        // and two owners are deduped straight back out by the next orphan sweep.
        for wsId in plan.hosted { claimHostedWorkspace(wsId, into: wc) }

        reviveRestoredSessions(plan.hosted)
        refreshOtherWindowIds()
    }

    /// Wake the sessions a window close put to sleep. Ownership already points at them,
    /// so each appears in the restored window the moment the daemon reports it live.
    private func reviveRestoredSessions(_ ids: [UUID]) {
        let service = RemoteSessionService.shared
        let pending = ids.filter { !service.isHosted($0) }
            .compactMap { service.daemonId(forApp: $0) }
        guard !pending.isEmpty else { return }

        Task { @MainActor in
            var failures: [String] = []
            for daemonId in pending {
                if let error = await service.reviveWorkspace(daemonId: daemonId) {
                    NSLog("[ccmux] restoring window: reviving %@ failed: %@", daemonId, error)
                    failures.append(error)
                }
            }
            // Push this window's name as each session's group. syncHostedGroups skips
            // anything not yet attached, so the pass that ran before these revives
            // landed ignored them, leaving the daemon on the old group.
            self.refreshOtherWindowIds()
            // One alert for the batch: the record is consumed by now, so silence would
            // leave an empty window and no explanation — but N dead sessions must not
            // mean N modal alerts stacked in front of the user.
            if let first = failures.first { self.reportRestoreFailure(first, count: failures.count) }
        }
    }

    /// A restore that could not bring sessions back says so.
    private func reportRestoreFailure(_ error: String, count: Int) {
        let alert = NSAlert()
        alert.messageText = count == 1 ? "Could not restore a session" : "Could not restore \(count) sessions"
        alert.informativeText = "\(error)\n\nThey are still listed under Cold Sessions."
        alert.alertStyle = .warning
        alert.addButton(withTitle: "OK")
        alert.runModal()
    }

    // MARK: - Queries

    /// Find which window (if any) is currently displaying a given workspace.
    func windowDisplaying(workspaceId: UUID) -> WorkspaceWindowController? {
        windowControllers.first { $0.windowContext.displayedWorkspaceId == workspaceId }
    }

    /// True when this workspace is one you are demonstrably looking at *right now*:
    /// ccmux is frontmost, and its key window both displays the workspace and sits
    /// on the Space you are currently on.
    ///
    /// The Space term is not decoration. A key window stays key across a Space
    /// switch — macOS only re-points it when the Space you arrive at has another
    /// app's window to activate — so on an empty Space, or one holding nothing but
    /// ccmux's own windows, the app stays active with a key window you cannot see.
    /// Without the Space check every alert for that workspace was swallowed as
    /// "already watching", and the only way to learn Claude was blocked was to
    /// switch back and look.
    ///
    /// One copy on purpose. The local hook listener and the hosted firehose both
    /// ask this question, they had already drifted into two identical private
    /// copies, and the missing Space term would have had to be fixed in both.
    func isWatching(_ id: UUID) -> Bool {
        guard let keyWindow = NSApp.keyWindow,
              let wc = windowControllers.first(where: { $0.window === keyWindow })
        else { return false }
        return Self.watched(
            appActive: NSApp.isActive,
            keyWindowOnActiveSpace: keyWindow.isOnActiveSpace,
            displayedByKeyWindow: wc.windowContext.displayedWorkspaceId,
            target: id
        )
    }

    /// The rule itself, lifted clear of AppKit state so it can be tested.
    static func watched(
        appActive: Bool,
        keyWindowOnActiveSpace: Bool,
        displayedByKeyWindow: UUID?,
        target: UUID
    ) -> Bool {
        appActive && keyWindowOnActiveSpace && displayedByKeyWindow == target
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

    /// Windows with a hosted-session create in flight. While non-zero, the orphan
    /// sweep pauses so a racing reconcile (createWorkspace's own refresh, or the
    /// workspace-added firehose) can't adopt the new session into the FIRST window
    /// before the creating window claims it. Other-lens orphans wait one cycle.
    private var pendingHostedCreates = 0
    func beginHostedCreate() { pendingHostedCreates += 1 }
    func endHostedCreate() { pendingHostedCreates = max(0, pendingHostedCreates - 1) }

    /// Claim a hosted workspace for one window EXCLUSIVELY: every other window
    /// drops it. Used after creating a session, in case an adoption sweep grabbed
    /// it mid-create anyway, and when restoring a closed window takes its sessions
    /// back from whichever window adopted them meanwhile.
    ///
    /// A window that loses the claim must also stop *showing* it. Ownership and
    /// display are separate fields, and leaving a window displaying a workspace it no
    /// longer owns breaks the routing invariant (`selectWorkspace` sends a click to
    /// the owner) and leaves two windows attached to one daemon session — which, with
    /// one PTY per session, means they fight over its size.
    func claimHostedWorkspace(_ id: UUID, into controller: WorkspaceWindowController) {
        for wc in windowControllers where wc !== controller {
            wc.windowContext.ownedWorkspaceIds.remove(id)
            guard wc.windowContext.displayedWorkspaceId == id else { continue }
            wc.windowContext.displayedWorkspaceId = wc.windowContext.ownedWorkspaceIds.first
            wc.updateWindowTitle()
        }
        controller.windowContext.ownedWorkspaceIds.insert(id)
        refreshOtherWindowIds()
        workspaceManager.scheduleSaveFromWindow()
    }

    /// Keep hosted-workspace ownership consistent after each daemon reconcile:
    /// orphans (sessions created by other lenses, or left behind by a closed
    /// window) are adopted by the first window, and a workspace owned by SEVERAL
    /// windows — the create/adopt race, possibly persisted by older builds — is
    /// deduped to exactly one.
    func adoptOrphanHostedWorkspaces() {
        guard pendingHostedCreates == 0 else { return } // a local create is claiming; don't race it
        guard let resolved = Self.reconcileHostedOwnership(
            workspaceIds: RemoteSessionService.shared.workspaces.map(\.id),
            groups: RemoteSessionService.shared.groups,
            owned: windowControllers.map { $0.windowContext.ownedWorkspaceIds },
            displayed: windowControllers.map { $0.windowContext.displayedWorkspaceId },
            windowNames: windowControllers.map { $0.windowContext.windowName ?? autoWindowName(for: $0) })
        else { return }
        for (wc, ids) in zip(windowControllers, resolved) where wc.windowContext.ownedWorkspaceIds != ids {
            wc.windowContext.ownedWorkspaceIds = ids
        }
        refreshOtherWindowIds()
        // Ownership lives only in the window descriptor, and nothing here trips the
        // autosave: without this the saved state keeps insisting a session is
        // unowned while the running app has already re-homed it.
        workspaceManager.scheduleSaveFromWindow()
    }

    /// Pure ownership resolution (index = window order): every listed workspace
    /// ends up owned by exactly one window. Orphans go to the window whose name
    /// matches their shared group (so a web/phone-created session lands in the
    /// group the user picked), else the first window; a multiply-owned workspace
    /// keeps the window displaying it, else its first owner. Returns nil when
    /// nothing changes.
    ///
    /// Every LIVE hosted session must end up owned by exactly one open window, so it
    /// has somewhere to appear. A closed window's sessions do not need protecting here:
    /// closing archives them, so they are cold, and a cold session is not in this list
    /// at all. One revived by hand is adopted normally — the window record stays until
    /// "Restore Window" uses it.
    static func reconcileHostedOwnership(
        workspaceIds: [UUID], groups: [UUID: String], owned: [Set<UUID>], displayed: [UUID?],
        windowNames: [String]
    ) -> [Set<UUID>]? {
        guard !owned.isEmpty else { return nil }
        var result = owned
        var changed = false
        for id in workspaceIds {
            let owners = result.indices.filter { result[$0].contains(id) }
            if owners.isEmpty {
                let target = groups[id].flatMap { windowNames.firstIndex(of: $0) } ?? 0
                result[target].insert(id)
                changed = true
            } else if owners.count > 1 {
                let keeper = owners.first { displayed[$0] == id } ?? owners[0]
                for i in owners where i != keeper {
                    result[i].remove(id)
                    changed = true
                }
            }
        }
        return changed ? result : nil
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
        // Closed windows remember hosted sessions too, so they need the same pruning
        // the local delete path gets — otherwise a record slowly fills with dead ids.
        if Self.shouldForgetRecordReferences(
            isKnownHosted: RemoteSessionService.shared.isKnownHosted(id)) {
            workspaceManager.forgetClosedWindowReferences(to: id)
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
            // Diff-gate the @Published writes: an unchanged reassign still
            // invalidates every sidebar's List (NSTableView reload), and a
            // reload mid-press kills an in-flight drag gesture.
            if wc.windowContext.otherWindowWorkspaceIds != otherIds {
                wc.windowContext.otherWindowWorkspaceIds = otherIds
            }
            if wc.windowContext.otherWindowGroups != otherGroups {
                wc.windowContext.otherWindowGroups = otherGroups
            }
            // Auto-assign window name if not custom
            if wc.windowContext.windowName == nil {
                // Don't publish — just for display fallback
            }
        }
        syncHostedGroups()
        syncLocalPaneGroups()
        // Displayed workspaces may have changed — keep the daemon focus frames
        // (phone-push suppression) truthful.
        RemoteSessionService.shared.syncFocusFrames()
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

    /// Rename a window. Persisted immediately: the name lives only in the window
    /// descriptor, and nothing else about a rename touches the autosave triggers, so
    /// without this the new name survived only if you happened to move, resize, or
    /// change a workspace before quitting.
    func renameWindow(id: UUID, newName: String?) {
        guard let wc = windowControllers.first(where: { $0.windowId == id }) else { return }
        wc.windowContext.windowName = newName
        refreshOtherWindowIds()
        workspaceManager.scheduleSaveFromWindow()
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

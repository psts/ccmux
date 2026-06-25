import Foundation
import Combine
import CoreGraphics

class WorkspaceManager: ObservableObject {
    @Published var workspaces: [Workspace] = []
    @Published var closedWorkspaces: [Workspace] = []
    @Published var closedWindows: [ClosedWindow] = []
    @Published var activeWorkspaceId: UUID?

    /// Map of workspace ID → its SplitTreeController (runtime only, not persisted)
    private(set) var controllers: [UUID: SplitTreeController] = [:]

    /// Map of workspace ID → its GitStatusMonitor (runtime only)
    private(set) var monitors: [UUID: GitStatusMonitor] = [:]

    /// Map of workspace ID → its ClaudeProcessMonitor (runtime only)
    private(set) var claudeMonitors: [UUID: ClaudeProcessMonitor] = [:]

    /// Terminal config IDs of ephemeral "teammate" panes spawned via `ccmux://spawn`
    /// (runtime only). These are stripped from every persisted layout so their seed
    /// prompt fires exactly once and never replays on restart. See `spawnTeammate`.
    private var ephemeralTerminalIds: Set<UUID> = []

    /// Terminal config IDs of designated Claude panes that received a teammate seed this
    /// session (runtime only). Their persisted startup command is forced to a plain
    /// `claude` so a restart resumes normally instead of re-firing the birth prompt.
    private var teammateSeededTerminalIds: Set<UUID> = []

    /// Called by WindowManager when a workspace is removed
    var onWorkspaceRemoved: ((UUID) -> Void)?

    /// Provides window descriptors at save time (set by AppDelegate after WindowManager is created)
    var windowDescriptorProvider: (() -> [WindowDescriptor])?

    private let saveDebouncer = Debouncer(delay: 0.3)
    /// Long-lived subscriptions on `self` (e.g. `$workspaces`).
    private var cancellables = Set<AnyCancellable>()
    /// Per-workspace controller subscriptions. Cleared in `closeWorkspace`/`removeWorkspace`
    /// so the upstream `SplitTreeController` is no longer retained after close.
    private var controllerCancellables: [UUID: Set<AnyCancellable>] = [:]

    var activeWorkspace: Workspace? {
        workspaces.first { $0.id == activeWorkspaceId }
    }

    var activeController: SplitTreeController? {
        guard let id = activeWorkspaceId else { return nil }
        return controllers[id]
    }

    init() {
        // Auto-save whenever workspaces or active ID changes
        $workspaces
            .combineLatest($activeWorkspaceId)
            .dropFirst()
            .sink { [weak self] _ in
                self?.scheduleSave()
            }
            .store(in: &cancellables)

        // Handle file link clicks from terminals — route to the right workspace's File Explorer
        TerminalStore.shared.onFileLinkClicked = { [weak self] terminalId, relativePath in
            guard let self else { return }
            // Find which workspace owns this terminal tab (matched by terminal config UUID)
            for (_, ctrl) in self.controllers {
                let ownsTerminal = ctrl.tree.allLeaves.contains { leaf in
                    leaf.content.tabs.contains { tab in
                        if case .terminal(let config) = tab { return config.id == terminalId }
                        return false
                    }
                }
                if ownsTerminal {
                    _ = ctrl.openFileInExplorer(relativePath: relativePath)
                    break
                }
            }
        }
    }

    // MARK: - Load / Save

    /// Load state and return window descriptors for WindowManager to restore.
    func loadState() -> [WindowDescriptor] {
        guard let state = PersistenceService.load() else { return [] }

        for workspace in state.workspaces {
            workspaces.append(workspace)
            let controller = SplitTreeController(workingDirectory: workspace.repoPath)
            // Restore the saved layout
            controller.tree = workspace.layout
            controller.focusedPaneId = workspace.focusedPaneId
            controller.claudePaneId = workspace.claudePaneId
            // Initialize scratchpad content from saved tree (scan every tab in every pane)
            outer: for leaf in workspace.layout.allLeaves {
                for tab in leaf.content.tabs {
                    if case .scratchpad(let config) = tab {
                        controller.scratchpadContent = config.content
                        break outer
                    }
                }
            }

            // Watch controller changes for auto-save
            observeController(controller, workspaceId: workspace.id)
            controllers[workspace.id] = controller

            // Start git monitoring
            monitors[workspace.id] = GitStatusMonitor(repoPath: workspace.repoPath)
            claudeMonitors[workspace.id] = ClaudeProcessMonitor(repoPath: workspace.repoPath)
        }

        // Load closed workspaces and windows
        closedWorkspaces = state.closedWorkspaces
        closedWindows = state.closedWindows

        activeWorkspaceId = state.activeWorkspaceId ?? workspaces.first?.id

        // v2: return saved window descriptors
        if !state.windows.isEmpty {
            return state.windows
        }

        // v1 migration: synthesize a single window from old fields
        if let frame = state.windowFrame {
            return [WindowDescriptor(
                id: UUID(),
                workspaceId: state.activeWorkspaceId,
                frame: frame
            )]
        }

        return []
    }

    func saveState() {
        // Build state from a local copy — do NOT mutate self.workspaces here,
        // because it's @Published and would trigger an infinite save loop.
        var snapshot = workspaces
        for i in snapshot.indices {
            if let ctrl = controllers[snapshot[i].id] {
                // Sync file explorer states back to configs before saving
                for leaf in ctrl.tree.allLeaves {
                    for tab in leaf.content.tabs {
                        if case .fileExplorer(let config) = tab {
                            ctrl.updateFileExplorerConfig(explorerId: config.id)
                        }
                    }
                }
                snapshot[i].layout = treeStrippingEphemeral(ctrl.tree)
                snapshot[i].focusedPaneId = ctrl.focusedPaneId
                snapshot[i].claudePaneId = ctrl.claudePaneId
            }
        }

        let state = AppState(
            workspaces: snapshot,
            closedWorkspaces: closedWorkspaces,
            closedWindows: closedWindows,
            activeWorkspaceId: activeWorkspaceId,
            version: 2,
            windows: windowDescriptorProvider?() ?? []
        )
        PersistenceService.save(state)
    }

    /// Detect running commands in all terminal panes and persist them into the
    /// workspace layouts. Called once from applicationWillTerminate — uses native
    /// libproc APIs so it's fast even with many terminals.
    func detectAndSaveCommands() {
        for i in workspaces.indices {
            if let ctrl = controllers[workspaces[i].id] {
                var tree = ctrl.tree
                for leaf in tree.allLeaves {
                    var pane = leaf.content
                    var changed = false
                    for (idx, tab) in pane.tabs.enumerated() {
                        if case .terminal(var config) = tab {
                            config.startupCommand = capturedStartupCommand(forTerminal: config.id)
                            pane.tabs[idx] = .terminal(config)
                            changed = true
                        }
                    }
                    if changed {
                        tree = tree.replaceContent(leafId: leaf.id, newContent: pane)
                    }
                }
                ctrl.tree = tree
            }
        }
    }

    /// Whether a pre-created pane should replay its startup command eagerly (without waiting
    /// to be displayed) and at what off-screen size.
    enum EagerStartupDecision: Equatable {
        /// Don't fire here — either it's the displayed workspace (fires via the view layer)
        /// or there's no command to replay.
        case skip
        /// Fire the queued command; resize the off-screen terminal to `targetSize` first when
        /// non-nil, else fire at the conservative fallback size.
        case fire(targetSize: CGSize?)
    }

    /// Pure policy behind `preCreateTerminals`:
    /// - Displayed workspaces fire via `TerminalContainerView.fireStartupIfReady` at the exact
    ///   laid-out size, so skip them here (firing at an estimated size could mis-wrap the one
    ///   pane the user is looking at).
    /// - Panes with no startup command have nothing to replay.
    /// - Single-leaf workspaces size to the owning window's content area; multi-pane keep the
    ///   conservative fallback (nil), which launches narrower than the eventual pane — the safe
    ///   wrap direction, since SwiftTerm doesn't reflow already-wrapped lines on grow.
    static func eagerStartupDecision(
        isDisplayed: Bool,
        isSingleLeaf: Bool,
        startupCommand: String?,
        contentSize: CGSize?
    ) -> EagerStartupDecision {
        guard !isDisplayed, let command = startupCommand, !command.isEmpty else { return .skip }
        return .fire(targetSize: isSingleLeaf ? contentSize : nil)
    }

    /// A terminal pane to (re)create on relaunch.
    private struct PaneJob {
        let workspaceId: UUID
        let terminalId: UUID
        let workingDirectory: String
        let startupCommand: String?
        let isSingleLeaf: Bool
    }

    /// Pre-create terminals on relaunch and replay startup commands, in two passes.
    ///
    /// The split matters because firing the (few) command panes must beat a manual workspace
    /// switch — otherwise the user activates the workspace first and it looks lazy:
    /// 1. Command panes (non-displayed + non-empty startupCommand) fire FIRST with a tight
    ///    stagger, so claude restarts within ~1s instead of being buried behind the bulk
    ///    shell warm-up. Displayed panes are excluded — the view layer
    ///    (TerminalContainerView.fireStartupIfReady) fires them at the exact laid-out size.
    /// 2. Remaining panes are pre-warmed with a gentle stagger (for fast switching); no command.
    ///
    /// Safe after window restoration — TerminalStore.terminal(for:) is idempotent, and the
    /// one-shot runStartupCommandIfPending prevents a double-send if a pane is activated first.
    func preCreateTerminals(
        displayedWorkspaceIds: Set<UUID>,
        contentSizeProvider: @escaping (_ workspaceId: UUID) -> CGSize?
    ) {
        var commandPanes: [PaneJob] = []   // non-displayed + has command → fire fast, first
        var warmPanes: [PaneJob] = []      // everything else → pre-warm the shell, no command
        for workspace in workspaces {
            guard let controller = controllers[workspace.id] else { continue }
            let isDisplayed = displayedWorkspaceIds.contains(workspace.id)
            let isSingleLeaf = controller.tree.allLeaves.count == 1
            for leaf in controller.tree.allLeaves {
                for tab in leaf.content.tabs {
                    guard case .terminal(let config) = tab else { continue }
                    let job = PaneJob(workspaceId: workspace.id, terminalId: config.id,
                                      workingDirectory: config.workingDirectory,
                                      startupCommand: config.startupCommand, isSingleLeaf: isSingleLeaf)
                    let fires = Self.eagerStartupDecision(isDisplayed: isDisplayed, isSingleLeaf: isSingleLeaf,
                                                          startupCommand: config.startupCommand, contentSize: nil) != .skip
                    if fires { commandPanes.append(job) } else { warmPanes.append(job) }
                }
            }
        }

        // Pass 1: restore commands fast. Start at 0.3s (windows are laid out → contentSize valid),
        // tight 0.05s stagger so even a dozen live commands all fire within ~1s.
        var delay: TimeInterval = 0.3
        for job in commandPanes {
            DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
                _ = TerminalStore.shared.terminal(for: job.terminalId, workingDirectory: job.workingDirectory,
                                                  startupCommand: job.startupCommand)
                let decision = Self.eagerStartupDecision(isDisplayed: false, isSingleLeaf: job.isSingleLeaf,
                                                         startupCommand: job.startupCommand,
                                                         contentSize: contentSizeProvider(job.workspaceId))
                if case .fire(let targetSize) = decision {
                    TerminalStore.shared.fireStartupEagerly(paneId: job.terminalId, targetSize: targetSize)
                }
            }
            delay += 0.05
        }

        // Pass 2: pre-warm the remaining shells gently (no command fired here).
        for job in warmPanes {
            DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
                _ = TerminalStore.shared.terminal(for: job.terminalId, workingDirectory: job.workingDirectory,
                                                  startupCommand: job.startupCommand)
            }
            delay += 0.15
        }
    }

    /// Called by the window controller on resize/move to persist the frame.
    func scheduleSaveFromWindow() {
        scheduleSave()
    }

    private func scheduleSave() {
        saveDebouncer.call { [weak self] in
            self?.saveState()
        }
    }

    private func observeController(_ controller: SplitTreeController, workspaceId: UUID) {
        var bag = Set<AnyCancellable>()
        controller.$tree
            .dropFirst()
            .sink { [weak self] _ in self?.scheduleSave() }
            .store(in: &bag)
        controller.$focusedPaneId
            .dropFirst()
            .sink { [weak self] _ in self?.scheduleSave() }
            .store(in: &bag)
        controller.$claudePaneId
            .dropFirst()
            .sink { [weak self] _ in self?.scheduleSave() }
            .store(in: &bag)
        controllerCancellables[workspaceId] = bag
    }

    // MARK: - Workspace Operations

    func addWorkspace(name: String, repoPath: String) {
        let workspace = Workspace.create(name: name, repoPath: repoPath)
        workspaces.append(workspace)

        let controller = SplitTreeController(workingDirectory: repoPath)
        observeController(controller, workspaceId: workspace.id)
        controllers[workspace.id] = controller

        // Start git monitoring
        monitors[workspace.id] = GitStatusMonitor(repoPath: repoPath)
        claudeMonitors[workspace.id] = ClaudeProcessMonitor(repoPath: repoPath)

        activeWorkspaceId = workspace.id
    }

    // MARK: - Teammate Spawning (ccmux://spawn)

    /// Spawn a "teammate" claude for `request`, returning the workspace it landed in.
    ///
    /// Finds the workspace whose repo matches `request.repoPath` (creating one if none
    /// exists), then adds an ephemeral split pane that launches `claude` seeded with the
    /// birth prompt. The pane is marked ephemeral so it is never persisted — the seed
    /// fires exactly once and a closed/quit teammate disappears rather than re-birthing.
    @discardableResult
    func spawnTeammate(_ request: SpawnRequest) -> UUID {
        let repo = Self.normalizePath(request.repoPath)
        let workspaceId = existingWorkspaceId(forRepo: repo) ?? createWorkspace(forRepo: repo)
        guard let controller = controllers[workspaceId] else { return workspaceId }

        // Prefer the workspace's designated Claude pane when it exists and is idle
        // (sitting at a shell prompt). Otherwise fall back to a fresh ephemeral split
        // so we never clobber a pane that's busy or already running something.
        if let paneId = controller.claudePaneId,
           controller.leafContaining(terminalId: paneId) != nil,
           TerminalStore.shared.detectRunningCommand(for: paneId) == nil {
            deliverSeed(request, toExistingPane: paneId, in: controller)
        } else {
            addEphemeralTeammatePane(request, repo: repo, in: controller)
        }
        activeWorkspaceId = workspaceId
        return workspaceId
    }

    /// Launch the teammate into an already-open, idle pane (the designated Claude pane).
    /// The seed is sent as live input now; the pane's persisted command is set to a plain
    /// `claude` so a restart resumes normally rather than re-firing the birth prompt.
    private func deliverSeed(_ request: SpawnRequest, toExistingPane terminalId: UUID, in controller: SplitTreeController) {
        TerminalStore.shared.sendCommand(request.claudeStartupCommand(), to: terminalId)
        TerminalStore.shared.autoConfirmStartupPrompts(paneId: terminalId)
        controller.setTerminalStartupCommand("claude", terminalId: terminalId)
        teammateSeededTerminalIds.insert(terminalId)
        if let leafId = controller.leafContaining(terminalId: terminalId) {
            controller.setFocus(paneId: leafId)
        }
    }

    /// Fallback: add a fresh ephemeral split pane running the teammate. Stripped from
    /// persistence so the seed fires exactly once.
    private func addEphemeralTeammatePane(_ request: SpawnRequest, repo: String, in controller: SplitTreeController) {
        guard let targetLeaf = controller.focusedPaneId ?? controller.tree.allLeaves.first?.id else { return }
        let config = TerminalConfig(
            id: UUID(),
            shell: ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh",
            workingDirectory: repo,
            title: "claude ⟵ peer",
            startupCommand: request.claudeStartupCommand()
        )
        ephemeralTerminalIds.insert(config.id)
        controller.tree = controller.tree.splitLeaf(
            targetId: targetLeaf,
            direction: .horizontal,
            newContent: PaneTabs(single: .terminal(config))
        )
        TerminalStore.shared.autoConfirmStartupPrompts(paneId: config.id)
    }

    /// First open workspace whose repo path matches (normalized), if any.
    private func existingWorkspaceId(forRepo repo: String) -> UUID? {
        workspaces.first { Self.normalizePath($0.repoPath) == repo }?.id
    }

    /// Create a fresh workspace rooted at `repo` and return its id.
    private func createWorkspace(forRepo repo: String) -> UUID {
        let name = (repo as NSString).lastPathComponent
        addWorkspace(name: name.isEmpty ? repo : name, repoPath: repo)
        // addWorkspace sets activeWorkspaceId to the new workspace.
        return activeWorkspaceId ?? workspaces.last!.id
    }

    /// Startup command to persist for a terminal. A pane that received a teammate seed
    /// this session always replays a plain `claude` (never the one-shot birth prompt);
    /// every other pane replays whatever command is currently running in it.
    private func capturedStartupCommand(forTerminal id: UUID) -> String? {
        if teammateSeededTerminalIds.contains(id) { return "claude" }
        return TerminalStore.shared.detectRunningCommand(for: id)
    }

    /// Normalize a path for comparison: expand `~`, resolve `.`/`..`, drop trailing slash.
    static func normalizePath(_ path: String) -> String {
        var p = ((path as NSString).expandingTildeInPath as NSString).standardizingPath
        if p.count > 1, p.hasSuffix("/") { p.removeLast() }
        return p
    }

    /// Return `tree` with every leaf that contains an ephemeral terminal removed.
    /// If dropping a leaf would empty the tree (it was the sole pane), the leaf is kept
    /// but its startup commands are stripped so the seed still never replays.
    private func treeStrippingEphemeral(_ tree: SplitTree<PaneTabs>) -> SplitTree<PaneTabs> {
        var result = tree
        for leaf in tree.allLeaves where leafIsEphemeral(leaf.content) {
            if let pruned = result.closeLeaf(targetId: leaf.id) {
                result = pruned
            } else {
                result = result.replaceContent(leafId: leaf.id, newContent: paneStrippingStartupCommands(leaf.content))
            }
        }
        return result
    }

    private func leafIsEphemeral(_ pane: PaneTabs) -> Bool {
        pane.tabs.contains { tab in
            if case .terminal(let config) = tab { return ephemeralTerminalIds.contains(config.id) }
            return false
        }
    }

    private func paneStrippingStartupCommands(_ pane: PaneTabs) -> PaneTabs {
        var copy = pane
        for (idx, tab) in copy.tabs.enumerated() {
            if case .terminal(var config) = tab {
                config.startupCommand = nil
                copy.tabs[idx] = .terminal(config)
            }
        }
        return copy
    }

    /// Save the window frame for a workspace (called before closing).
    func saveWindowFrame(workspaceId: UUID, frame: WindowFrame) {
        if let idx = workspaces.firstIndex(where: { $0.id == workspaceId }) {
            workspaces[idx].lastWindowFrame = frame
        }
    }

    /// Close a workspace: save its state and move it to closedWorkspaces.
    /// Terminals are terminated. The workspace can be reopened later.
    func closeWorkspace(id: UUID) {
        guard let idx = workspaces.firstIndex(where: { $0.id == id }) else { return }

        // Sync controller state to workspace before closing
        var workspace = workspaces[idx]
        if let ctrl = controllers[id] {
            workspace.layout = ctrl.tree
            workspace.focusedPaneId = ctrl.focusedPaneId
            workspace.claudePaneId = ctrl.claudePaneId
        }

        // Detect running commands before terminating terminals
        for leaf in workspace.layout.allLeaves {
            var pane = leaf.content
            var changed = false
            for (idx, tab) in pane.tabs.enumerated() {
                if case .terminal(var config) = tab {
                    config.startupCommand = TerminalStore.shared.detectRunningCommand(for: config.id)
                    pane.tabs[idx] = .terminal(config)
                    changed = true
                }
            }
            if changed {
                workspace.layout = workspace.layout.replaceContent(leafId: leaf.id, newContent: pane)
            }
        }

        // Clean up all terminals in this workspace
        var terminalIdsInWorkspace: Set<UUID> = []
        for leaf in workspace.layout.allLeaves {
            for tab in leaf.content.tabs {
                if case .terminal(let config) = tab {
                    TerminalStore.shared.remove(paneId: config.id)
                    terminalIdsInWorkspace.insert(config.id)
                }
            }
        }

        // Drop ephemeral teammate panes so reopening this workspace never replays a seed.
        // Strip the layout first (consults ephemeralTerminalIds), then forget those ids.
        workspace.layout = treeStrippingEphemeral(workspace.layout)
        ephemeralTerminalIds.subtract(terminalIdsInWorkspace)

        // Move to closed list
        closedWorkspaces.append(workspace)

        // Remove from open list
        workspaces.remove(at: idx)
        controllers.removeValue(forKey: id)
        controllerCancellables.removeValue(forKey: id)
        monitors[id]?.stop()
        monitors.removeValue(forKey: id)
        claudeMonitors[id]?.stop()
        claudeMonitors.removeValue(forKey: id)

        if activeWorkspaceId == id {
            activeWorkspaceId = workspaces.first?.id
        }
    }

    /// Permanently delete a closed workspace.
    func deleteClosedWorkspace(id: UUID) {
        closedWorkspaces.removeAll { $0.id == id }
        // Also remove from any closed windows
        for i in closedWindows.indices {
            closedWindows[i].workspaceIds.removeAll { $0 == id }
        }
        closedWindows.removeAll { $0.workspaceIds.isEmpty }
        scheduleSave()
    }

    /// Save a closed window group for later restoration.
    func saveClosedWindow(_ closedWindow: ClosedWindow) {
        closedWindows.append(closedWindow)
        scheduleSave()
    }

    /// Permanently delete a closed window group.
    func deleteClosedWindow(id: UUID) {
        guard let window = closedWindows.first(where: { $0.id == id }) else { return }
        // Also remove the individual workspaces
        for wsId in window.workspaceIds {
            closedWorkspaces.removeAll { $0.id == wsId }
        }
        closedWindows.removeAll { $0.id == id }
        scheduleSave()
    }

    /// Reopen a previously closed workspace.
    func reopenWorkspace(id: UUID) -> Workspace? {
        guard let idx = closedWorkspaces.firstIndex(where: { $0.id == id }) else { return nil }

        let workspace = closedWorkspaces.remove(at: idx)
        workspaces.append(workspace)

        // Create controller with saved layout
        let controller = SplitTreeController(workingDirectory: workspace.repoPath)
        controller.tree = workspace.layout
        controller.focusedPaneId = workspace.focusedPaneId
        controller.claudePaneId = workspace.claudePaneId
        // Restore scratchpad content (first scratchpad tab across all panes)
        outer: for leaf in workspace.layout.allLeaves {
            for tab in leaf.content.tabs {
                if case .scratchpad(let config) = tab {
                    controller.scratchpadContent = config.content
                    break outer
                }
            }
        }
        observeController(controller, workspaceId: workspace.id)
        controllers[workspace.id] = controller

        // Start git monitoring
        monitors[workspace.id] = GitStatusMonitor(repoPath: workspace.repoPath)
        claudeMonitors[workspace.id] = ClaudeProcessMonitor(repoPath: workspace.repoPath)

        activeWorkspaceId = workspace.id
        return workspace
    }

    func removeWorkspace(id: UUID) {
        workspaces.removeAll { $0.id == id }
        controllers.removeValue(forKey: id)
        controllerCancellables.removeValue(forKey: id)
        monitors[id]?.stop()
        monitors.removeValue(forKey: id)
        claudeMonitors[id]?.stop()
        claudeMonitors.removeValue(forKey: id)
        if activeWorkspaceId == id {
            activeWorkspaceId = workspaces.first?.id
        }
        onWorkspaceRemoved?(id)
    }

    func selectWorkspace(id: UUID) {
        activeWorkspaceId = id
    }

    /// Detect sub-items (directories with common project markers) in a workspace.
    func detectSubItems(for workspaceId: UUID) {
        guard let idx = workspaces.firstIndex(where: { $0.id == workspaceId }) else { return }
        let repoPath = workspaces[idx].repoPath

        let markers = ["package.json", "Cargo.toml", "Package.swift", "go.mod", "pyproject.toml", "pom.xml", "build.gradle"]
        let fm = FileManager.default

        var subItems: [WorkspaceSubItem] = []
        guard let contents = try? fm.contentsOfDirectory(atPath: repoPath) else { return }

        for item in contents where !item.hasPrefix(".") {
            let fullPath = (repoPath as NSString).appendingPathComponent(item)
            var isDir: ObjCBool = false
            guard fm.fileExists(atPath: fullPath, isDirectory: &isDir), isDir.boolValue else { continue }

            let hasMarker = markers.contains { marker in
                fm.fileExists(atPath: (fullPath as NSString).appendingPathComponent(marker))
            }
            if hasMarker {
                subItems.append(WorkspaceSubItem(
                    id: UUID(),
                    name: item,
                    relativePath: item,
                    needsInput: false
                ))
            }
        }

        workspaces[idx].subItems = subItems
    }
}

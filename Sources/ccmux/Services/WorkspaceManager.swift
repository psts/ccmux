import Foundation
import Combine

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

    /// Called by WindowManager when a workspace is removed
    var onWorkspaceRemoved: ((UUID) -> Void)?

    /// Provides window descriptors at save time (set by AppDelegate after WindowManager is created)
    var windowDescriptorProvider: (() -> [WindowDescriptor])?

    private let saveDebouncer = Debouncer(delay: 0.3)
    private var cancellables = Set<AnyCancellable>()

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
                snapshot[i].layout = ctrl.tree
                snapshot[i].focusedPaneId = ctrl.focusedPaneId
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
                            config.startupCommand = TerminalStore.shared.detectRunningCommand(for: config.id)
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

    /// Pre-create terminals for ALL workspaces so startup commands are replayed
    /// even for workspaces not currently displayed. Staggers creation to avoid
    /// launching many shell processes simultaneously.
    /// Safe to call after window restoration — TerminalStore.terminal(for:) is
    /// idempotent, so terminals already created by the view layer are skipped.
    func preCreateTerminals() {
        var delay: TimeInterval = 0.3
        let stagger: TimeInterval = 0.15

        for workspace in workspaces {
            guard let controller = controllers[workspace.id] else { continue }
            for leaf in controller.tree.allLeaves {
                for tab in leaf.content.tabs {
                    if case .terminal(let config) = tab {
                        let terminalId = config.id
                        let workingDir = config.workingDirectory
                        let command = config.startupCommand
                        DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
                            _ = TerminalStore.shared.terminal(
                                for: terminalId,
                                workingDirectory: workingDir,
                                startupCommand: command
                            )
                        }
                        delay += stagger
                    }
                }
            }
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
        controller.$tree
            .dropFirst()
            .sink { [weak self] _ in self?.scheduleSave() }
            .store(in: &cancellables)
        controller.$focusedPaneId
            .dropFirst()
            .sink { [weak self] _ in self?.scheduleSave() }
            .store(in: &cancellables)
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
        for leaf in workspace.layout.allLeaves {
            for tab in leaf.content.tabs {
                if case .terminal(let config) = tab {
                    TerminalStore.shared.remove(paneId: config.id)
                }
            }
        }

        // Move to closed list
        closedWorkspaces.append(workspace)

        // Remove from open list
        workspaces.remove(at: idx)
        controllers.removeValue(forKey: id)
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

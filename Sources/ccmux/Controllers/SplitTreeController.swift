import Foundation
import Combine

class SplitTreeController: ObservableObject {
    @Published var tree: SplitTree<PaneTabs>
    @Published var focusedPaneId: UUID?
    /// TerminalConfig.id of the designated "Claude pane" (see Workspace.claudePaneId).
    @Published var claudePaneId: UUID?

    let workingDirectory: String

    /// Workspace-level scratchpad content — survives pane close/reopen.
    var scratchpadContent: String = ""

    /// Runtime state for file explorer tabs, keyed by the FileExplorerConfig UUID.
    var fileExplorerStates: [UUID: FileExplorerState] = [:]

    /// Where this workspace's file explorers read/write files. nil = local disk;
    /// RemoteSessionService sets a DaemonFileSource for hosted workspaces.
    var fileSource: FileSource?

    // MARK: - Hosted-workspace hooks (set by RemoteSessionService; nil for local)

    /// Intercepts terminal creation: in a hosted workspace a new terminal must be
    /// a daemon tmux pane, not a local child shell. Args: target leaf, split
    /// direction (nil = append as a tab). The service spawns the pane and inserts
    /// the hosted tab/split itself.
    var onHostedTerminalRequest: ((UUID, SplitDirection?) -> Void)?
    /// Fired with the daemon pane id of every hosted terminal tab the user closes,
    /// so the service kills the remote pane (otherwise it would keep running and
    /// the next reconcile's merge would resurface it).
    var onHostedPaneClosed: ((String) -> Void)?

    init(workingDirectory: String) {
        self.workingDirectory = workingDirectory
        let initialContent = PaneContent.defaultTerminal(workingDirectory: workingDirectory)
        let initialTabs = PaneTabs(single: initialContent)
        self.tree = .leaf(id: initialTabs.id, content: initialTabs)
        self.focusedPaneId = initialTabs.id
    }

    // MARK: - Pane Mutations

    func splitPane(id: UUID, direction: SplitDirection) {
        if let onHostedTerminalRequest {
            onHostedTerminalRequest(id, direction)
            return
        }
        let newContent = PaneContent.defaultTerminal(workingDirectory: workingDirectory)
        let newTabs = PaneTabs(single: newContent)
        tree = tree.splitLeaf(targetId: id, direction: direction, newContent: newTabs)
        focusedPaneId = newTabs.id
    }

    func closePane(id: UUID) {
        // Don't close if it's the last pane
        guard tree.leafCount > 1 else { return }

        // Capture content before mutation so we can clean up resources
        let closedTabs = tree.findLeaf(id: id)

        if let newTree = tree.closeLeaf(targetId: id) {
            if let pane = closedTabs {
                for tab in pane.tabs {
                    // Stash last scratchpad content so reopening a scratchpad restores it
                    if case .scratchpad(let config) = tab {
                        scratchpadContent = config.content
                    }
                    cleanupResources(for: tab)
                }
            }

            tree = newTree
            if focusedPaneId == id {
                focusedPaneId = tree.allLeaves.first?.id
            }
        }
    }

    func updateRatio(splitId: UUID, newRatio: CGFloat) {
        let clamped = max(0.1, min(0.9, newRatio))
        tree = tree.updateRatio(splitId: splitId, newRatio: clamped)
    }

    func setFocus(paneId: UUID) {
        focusedPaneId = paneId
    }

    func movePane(sourceId: UUID, targetId: UUID, direction: SplitDirection, insertAsFirst: Bool) {
        guard sourceId != targetId else { return }
        if let newTree = tree.moveLeaf(sourceId: sourceId, targetId: targetId, direction: direction, insertAsFirst: insertAsFirst) {
            tree = newTree
            focusedPaneId = sourceId
        }
    }

    // MARK: - Claude Pane Designation

    /// Toggle the designated Claude pane. Passing the currently-designated terminal clears it.
    func setClaudePane(terminalId: UUID) {
        claudePaneId = (claudePaneId == terminalId) ? nil : terminalId
    }

    /// Leaf (pane) id that contains the terminal tab `terminalId`, if any.
    func leafContaining(terminalId: UUID) -> UUID? {
        for leaf in tree.allLeaves {
            for tab in leaf.content.tabs {
                if case .terminal(let config) = tab, config.id == terminalId { return leaf.id }
            }
        }
        return nil
    }

    /// Set a terminal tab's persisted startup command. Used to keep a spawned teammate's
    /// pane replaying a plain `claude` on restart instead of re-firing the birth prompt.
    func setTerminalStartupCommand(_ command: String?, terminalId: UUID) {
        guard let leafId = leafContaining(terminalId: terminalId),
              var pane = tree.findLeaf(id: leafId) else { return }
        for (idx, tab) in pane.tabs.enumerated() {
            if case .terminal(var config) = tab, config.id == terminalId {
                config.startupCommand = command
                pane.tabs[idx] = .terminal(config)
                tree = tree.replaceContent(leafId: leafId, newContent: pane)
                return
            }
        }
    }

    // MARK: - Tab Mutations

    /// Append a new tab to an existing pane and activate it. A *local* terminal
    /// request in a hosted workspace is routed to the daemon instead; hosted
    /// content (the service inserting the spawned pane) and non-terminal tabs
    /// pass straight through.
    func addTab(leafId: UUID, newContent: PaneContent) {
        if let onHostedTerminalRequest, case .terminal(let cfg) = newContent, !cfg.host.isHosted {
            onHostedTerminalRequest(leafId, nil)
            return
        }
        guard var pane = tree.findLeaf(id: leafId) else { return }
        pane.addTab(newContent)
        tree = tree.replaceContent(leafId: leafId, newContent: pane)
        focusedPaneId = leafId
    }

    /// Switch the active tab within a pane.
    func activateTab(leafId: UUID, tabId: UUID) {
        guard var pane = tree.findLeaf(id: leafId) else { return }
        guard pane.activeTabId != tabId else { return }
        guard pane.tabs.contains(where: { $0.id == tabId }) else { return }
        pane.activeTabId = tabId
        tree = tree.replaceContent(leafId: leafId, newContent: pane)
        focusedPaneId = leafId
    }

    /// Close a tab. If it was the last tab in the pane, the pane itself is closed
    /// (unless it's also the last pane, in which case nothing happens).
    func closeTab(leafId: UUID, tabId: UUID) {
        guard var pane = tree.findLeaf(id: leafId) else { return }

        // Single-tab pane → delegate to closePane
        if pane.tabs.count <= 1 {
            closePane(id: leafId)
            return
        }

        // Clean up the tab's resources before removing
        if let tab = pane.tabs.first(where: { $0.id == tabId }) {
            if case .scratchpad(let config) = tab {
                scratchpadContent = config.content
            }
            cleanupResources(for: tab)
        }

        if pane.removeTab(tabId: tabId) {
            tree = tree.replaceContent(leafId: leafId, newContent: pane)
        }
    }

    private func cleanupResources(for content: PaneContent) {
        switch content {
        case .terminal(let config):
            if let paneId = config.host.hostedPaneId {
                onHostedPaneClosed?(paneId)   // remote pane — kill it on the daemon
            } else {
                TerminalStore.shared.remove(paneId: config.id)
            }
        case .fileExplorer(let config):
            fileExplorerStates.removeValue(forKey: config.id)
        default:
            break
        }
    }

    // MARK: - Content-specific helpers

    /// Get or create a FileExplorerState for a file-explorer tab, keyed by its config UUID.
    func fileExplorerState(for explorerId: UUID) -> FileExplorerState? {
        if let existing = fileExplorerStates[explorerId] {
            return existing
        }
        // Find the config by scanning all panes' tabs
        guard let config = findFileExplorerConfig(explorerId: explorerId) else { return nil }
        let state = FileExplorerState(rootPath: config.rootPath, source: fileSource)
        state.restoreFromConfig(config)
        fileExplorerStates[explorerId] = state
        return state
    }

    private func findFileExplorerConfig(explorerId: UUID) -> FileExplorerConfig? {
        for leaf in tree.allLeaves {
            for tab in leaf.content.tabs {
                if case .fileExplorer(let config) = tab, config.id == explorerId {
                    return config
                }
            }
        }
        return nil
    }

    /// Locate the pane (leaf) containing a file-explorer tab with the given id.
    private func findLeafContaining(explorerId: UUID) -> UUID? {
        for leaf in tree.allLeaves {
            for tab in leaf.content.tabs {
                if case .fileExplorer(let config) = tab, config.id == explorerId {
                    return leaf.id
                }
            }
        }
        return nil
    }

    /// Sync a file explorer's runtime state back to its persisted config.
    func updateFileExplorerConfig(explorerId: UUID) {
        guard let state = fileExplorerStates[explorerId] else { return }
        guard let leafId = findLeafContaining(explorerId: explorerId) else { return }
        guard var pane = tree.findLeaf(id: leafId) else { return }
        let newConfig = state.persistableConfig(id: explorerId)
        pane.updateTab(tabId: explorerId, newContent: .fileExplorer(newConfig))
        tree = tree.replaceContent(leafId: leafId, newContent: pane)
    }

    /// Open a file in the first available File Explorer tab. Returns true if successful.
    func openFileInExplorer(relativePath: String) -> Bool {
        guard let (leafId, configId) = firstExplorerTab(),
              let state = fileExplorerState(for: configId) else { return false }
        activateTab(leafId: leafId, tabId: configId)
        state.openFile(relativePath: relativePath)
        return true
    }

    /// Like `openFileInExplorer`, but creates a File Explorer tab in the focused
    /// pane when the workspace has none — a clicked file link should always land
    /// somewhere visible. The read is probed FIRST: SwiftTerm's implicit link
    /// regex also fires on non-files, and a miss must not leave a junk Files tab
    /// behind (hosted layouts sync to the daemon and every other lens).
    func revealFileInExplorer(relativePath: String) {
        if openFileInExplorer(relativePath: relativePath) { return }
        let source = fileSource ?? LocalFileSource(rootPath: workingDirectory)
        Task { @MainActor in
            guard await source.read(path: relativePath) != nil else { return }
            if self.openFileInExplorer(relativePath: relativePath) { return }
            guard let leafId = self.focusedPaneId ?? self.tree.allLeaves.first?.id else { return }
            let content = PaneContent.defaultFileExplorer(rootPath: self.workingDirectory)
            self.addTab(leafId: leafId, newContent: content)
            self.fileExplorerState(for: content.id)?.openFile(relativePath: relativePath)
        }
    }

    private func firstExplorerTab() -> (leafId: UUID, configId: UUID)? {
        for leaf in tree.allLeaves {
            for tab in leaf.content.tabs {
                if case .fileExplorer(let config) = tab { return (leaf.id, config.id) }
            }
        }
        return nil
    }

    /// Update the content of a scratchpad tab.
    func updateScratchpadContent(leafId: UUID, tabId: UUID, newText: String) {
        guard var pane = tree.findLeaf(id: leafId) else { return }
        guard let tab = pane.tabs.first(where: { $0.id == tabId }) else { return }
        guard case .scratchpad(var config) = tab else { return }
        config.content = newText
        scratchpadContent = newText
        pane.updateTab(tabId: tabId, newContent: .scratchpad(config))
        tree = tree.replaceContent(leafId: leafId, newContent: pane)
    }
}

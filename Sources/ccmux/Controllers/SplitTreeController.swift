import Foundation
import Combine

class SplitTreeController: ObservableObject {
    @Published var tree: SplitTree<PaneTabs>
    @Published var focusedPaneId: UUID?

    let workingDirectory: String

    /// Workspace-level scratchpad content — survives pane close/reopen.
    var scratchpadContent: String = ""

    /// Runtime state for file explorer tabs, keyed by the FileExplorerConfig UUID.
    var fileExplorerStates: [UUID: FileExplorerState] = [:]

    init(workingDirectory: String) {
        self.workingDirectory = workingDirectory
        let initialContent = PaneContent.defaultTerminal(workingDirectory: workingDirectory)
        let initialTabs = PaneTabs(single: initialContent)
        self.tree = .leaf(id: initialTabs.id, content: initialTabs)
        self.focusedPaneId = initialTabs.id
    }

    // MARK: - Pane Mutations

    func splitPane(id: UUID, direction: SplitDirection) {
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

    // MARK: - Tab Mutations

    /// Append a new tab to an existing pane and activate it.
    func addTab(leafId: UUID, newContent: PaneContent) {
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
            TerminalStore.shared.remove(paneId: config.id)
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
        let state = FileExplorerState(rootPath: config.rootPath)
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
        for leaf in tree.allLeaves {
            for tab in leaf.content.tabs {
                if case .fileExplorer(let config) = tab {
                    if let state = fileExplorerState(for: config.id) {
                        state.openFile(relativePath: relativePath)
                        return true
                    }
                }
            }
        }
        return false
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

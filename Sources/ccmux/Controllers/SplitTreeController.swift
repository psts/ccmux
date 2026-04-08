import Foundation
import Combine

class SplitTreeController: ObservableObject {
    @Published var tree: SplitTree<PaneContent>
    @Published var focusedPaneId: UUID?

    let workingDirectory: String

    /// Workspace-level scratchpad content — survives pane close/reopen.
    var scratchpadContent: String = ""

    /// Runtime state for file explorer panes, keyed by pane ID.
    var fileExplorerStates: [UUID: FileExplorerState] = [:]

    init(workingDirectory: String) {
        self.workingDirectory = workingDirectory
        let initialContent = PaneContent.defaultTerminal(workingDirectory: workingDirectory)
        let leafId = initialContent.id
        self.tree = .leaf(id: leafId, content: initialContent)
        self.focusedPaneId = leafId
    }

    // MARK: - Mutations

    func splitPane(id: UUID, direction: SplitDirection) {
        let newContent = PaneContent.defaultTerminal(workingDirectory: workingDirectory)
        tree = tree.splitLeaf(targetId: id, direction: direction, newContent: newContent)
        focusedPaneId = newContent.id
    }

    func closePane(id: UUID) {
        // Don't close if it's the last pane
        guard tree.leafCount > 1 else { return }

        // Save scratchpad content before closing, in case it's a scratchpad pane
        if case .scratchpad(let config) = tree.findLeaf(id: id) {
            scratchpadContent = config.content
        }

        // Check what type of pane is being closed before removing it from the tree
        let closedContent = tree.findLeaf(id: id)

        if let newTree = tree.closeLeaf(targetId: id) {
            // Clean up resources for the closed pane
            if case .terminal = closedContent {
                TerminalStore.shared.remove(paneId: id)
            }
            if case .fileExplorer = closedContent {
                fileExplorerStates.removeValue(forKey: id)
            }

            tree = newTree
            // Move focus to first remaining leaf if we closed the focused pane
            if focusedPaneId == id {
                focusedPaneId = tree.allLeaves.first?.id
            }
        }
    }

    func updateRatio(splitId: UUID, newRatio: CGFloat) {
        let clamped = max(0.1, min(0.9, newRatio))
        tree = tree.updateRatio(splitId: splitId, newRatio: clamped)
    }

    func replaceContent(leafId: UUID, newContent: PaneContent) {
        // Clean up old resources if switching away
        if case .terminal = tree.findLeaf(id: leafId) {
            TerminalStore.shared.remove(paneId: leafId)
        }
        if case .fileExplorer = tree.findLeaf(id: leafId) {
            fileExplorerStates.removeValue(forKey: leafId)
        }
        tree = tree.replaceContent(leafId: leafId, newContent: newContent)
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

    /// Get or create a FileExplorerState for a pane. Lazily initializes and restores from config.
    func fileExplorerState(for paneId: UUID) -> FileExplorerState? {
        if let existing = fileExplorerStates[paneId] {
            return existing
        }
        guard case .fileExplorer(let config) = tree.findLeaf(id: paneId) else { return nil }
        let state = FileExplorerState(rootPath: config.rootPath)
        state.restoreFromConfig(config)
        fileExplorerStates[paneId] = state
        return state
    }

    /// Sync file explorer runtime state back to the tree config for persistence.
    func updateFileExplorerConfig(leafId: UUID) {
        guard let state = fileExplorerStates[leafId] else { return }
        let config = state.persistableConfig(id: leafId)
        tree = tree.replaceContent(leafId: leafId, newContent: .fileExplorer(config))
    }

    /// Open a file in the first available File Explorer pane. Returns true if successful.
    func openFileInExplorer(relativePath: String) -> Bool {
        // Find first file explorer pane
        for leaf in tree.allLeaves {
            if case .fileExplorer = leaf.content {
                if let state = fileExplorerState(for: leaf.id) {
                    state.openFile(relativePath: relativePath)
                    return true
                }
            }
        }
        return false
    }

    func updateScratchpadContent(leafId: UUID, newText: String) {
        guard case .scratchpad(var config) = tree.findLeaf(id: leafId) else { return }
        config.content = newText
        scratchpadContent = newText
        tree = tree.replaceContent(leafId: leafId, newContent: .scratchpad(config))
    }
}

import Foundation

struct Workspace: Codable, Identifiable {
    let id: UUID
    var name: String
    var repoPath: String
    var layout: SplitTree<PaneContent>
    var focusedPaneId: UUID?
    var subItems: [WorkspaceSubItem]
    var lastOpened: Date
    var lastWindowFrame: WindowFrame?

    /// Create a new workspace for a given directory.
    static func create(name: String, repoPath: String) -> Workspace {
        let terminalContent = PaneContent.defaultTerminal(workingDirectory: repoPath)
        return Workspace(
            id: UUID(),
            name: name,
            repoPath: repoPath,
            layout: .leaf(id: terminalContent.id, content: terminalContent),
            focusedPaneId: terminalContent.id,
            subItems: [],
            lastOpened: Date()
        )
    }
}

struct WorkspaceSubItem: Codable, Identifiable {
    let id: UUID
    var name: String
    var relativePath: String
    var needsInput: Bool
}

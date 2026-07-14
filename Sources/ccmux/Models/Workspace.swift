import Foundation

struct Workspace: Codable, Identifiable {
    let id: UUID
    var name: String
    var repoPath: String
    var layout: SplitTree<PaneTabs>
    var focusedPaneId: UUID?
    /// TerminalConfig.id of the pane designated as this workspace's "Claude pane" —
    /// where a spawned teammate (ccmux://spawn) lands instead of a new split.
    /// Optional so older persisted state decodes cleanly.
    var claudePaneId: UUID?
    var subItems: [WorkspaceSubItem]
    var lastOpened: Date
    var lastWindowFrame: WindowFrame?
    /// `.local` (driver, default) or `.hosted` (attached to a ccmuxd/tmux session).
    /// Hosted workspaces are sourced live from the daemon and are NOT persisted; the
    /// default keeps older state.json workspaces decoding as local.
    var mode: WorkspaceMode = .local

    /// Create a new workspace for a given directory.
    static func create(name: String, repoPath: String) -> Workspace {
        let terminalContent = PaneContent.defaultTerminal(workingDirectory: repoPath)
        let tabs = PaneTabs(single: terminalContent)
        return Workspace(
            id: UUID(),
            name: name,
            repoPath: repoPath,
            layout: .leaf(id: tabs.id, content: tabs),
            focusedPaneId: tabs.id,
            subItems: [],
            lastOpened: Date()
        )
    }
}

extension Workspace {
    // Custom decoder so adding `mode` (and any future default-valued field) tolerates
    // older state.json files. Declared in an extension to preserve the synthesized
    // memberwise initializer that `Workspace.create` and callers rely on.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(UUID.self, forKey: .id)
        name = try c.decode(String.self, forKey: .name)
        repoPath = try c.decode(String.self, forKey: .repoPath)
        layout = try c.decode(SplitTree<PaneTabs>.self, forKey: .layout)
        focusedPaneId = try c.decodeIfPresent(UUID.self, forKey: .focusedPaneId)
        claudePaneId = try c.decodeIfPresent(UUID.self, forKey: .claudePaneId)
        subItems = try c.decodeIfPresent([WorkspaceSubItem].self, forKey: .subItems) ?? []
        lastOpened = try c.decodeIfPresent(Date.self, forKey: .lastOpened) ?? Date()
        lastWindowFrame = try c.decodeIfPresent(WindowFrame.self, forKey: .lastWindowFrame)
        mode = try c.decodeIfPresent(WorkspaceMode.self, forKey: .mode) ?? .local
    }
}

struct WorkspaceSubItem: Codable, Identifiable {
    let id: UUID
    var name: String
    var relativePath: String
    var needsInput: Bool
}

import Foundation

struct AppState: Codable {
    var workspaces: [Workspace]
    var closedWorkspaces: [Workspace] = []  // saved but not currently open
    var closedWindows: [ClosedWindow] = []  // saved window groups
    var activeWorkspaceId: UUID?
    var version: Int = 2
    var windowFrame: WindowFrame?       // v1 compat
    var windows: [WindowDescriptor] = [] // v2: multi-window state
}

/// A closed window with its workspace group, saved for later restoration.
struct ClosedWindow: Codable, Identifiable {
    let id: UUID
    var windowName: String?
    var workspaceIds: [UUID]
    var displayedWorkspaceId: UUID?
    var frame: WindowFrame

    /// Display name — custom name or auto-generated from workspace names
    var displayName: String {
        if let name = windowName { return name }
        return "Window"
    }
}

struct WindowFrame: Codable {
    var x: Double
    var y: Double
    var width: Double
    var height: Double
}

struct WindowDescriptor: Codable, Identifiable {
    let id: UUID
    var workspaceId: UUID?
    var ownedWorkspaceIds: [UUID] = []  // all workspaces owned by this window
    var windowName: String?             // custom name, nil = auto "Window N"
    var frame: WindowFrame
    var space: SpaceSnapshot?
}

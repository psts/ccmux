import Foundation

struct AppState: Codable {
    var workspaces: [Workspace]
    var closedWorkspaces: [Workspace] = []  // saved but not currently open
    var activeWorkspaceId: UUID?
    var version: Int = 2
    var windowFrame: WindowFrame?       // v1 compat
    var windows: [WindowDescriptor] = [] // v2: multi-window state
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
    var frame: WindowFrame
}

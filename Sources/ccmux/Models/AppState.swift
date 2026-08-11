import Foundation

struct AppState: Codable {
    var workspaces: [Workspace]
    var closedWorkspaces: [Workspace] = []  // saved but not currently open
    var closedWindows: [ClosedWindow] = []  // saved window groups
    var activeWorkspaceId: UUID?
    var version: Int = 2
    var windowFrame: WindowFrame?       // v1 compat
    var windows: [WindowDescriptor] = [] // v2: multi-window state

    init(
        workspaces: [Workspace],
        closedWorkspaces: [Workspace] = [],
        closedWindows: [ClosedWindow] = [],
        activeWorkspaceId: UUID? = nil,
        version: Int = 2,
        windowFrame: WindowFrame? = nil,
        windows: [WindowDescriptor] = []
    ) {
        self.workspaces = workspaces
        self.closedWorkspaces = closedWorkspaces
        self.closedWindows = closedWindows
        self.activeWorkspaceId = activeWorkspaceId
        self.version = version
        self.windowFrame = windowFrame
        self.windows = windows
    }

    // See WindowDescriptor.init(from:) for the rationale — `decodeIfPresent`
    // keeps default-valued fields tolerant of older state.json files.
    //
    // The three collections decode leniently (`decodeLossyArray`): one corrupt
    // workspace used to throw and take the entire file with it, losing every other
    // workspace, both closed lists, and the whole window/Space layout. Drops are
    // tallied into `decoder.dropLog` so PersistenceService can back the file up
    // before the launch-time autosave overwrites it.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let log = decoder.dropLog
        workspaces = try c.decodeLossyArray(Workspace.self, forKey: .workspaces, into: log, required: true)
        closedWorkspaces = try c.decodeLossyArray(Workspace.self, forKey: .closedWorkspaces, into: log)
        closedWindows = try c.decodeLossyArray(ClosedWindow.self, forKey: .closedWindows, into: log)
        activeWorkspaceId = try c.decodeIfPresent(UUID.self, forKey: .activeWorkspaceId)
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 2
        windowFrame = try c.decodeIfPresent(WindowFrame.self, forKey: .windowFrame)
        windows = try c.decodeLossyArray(WindowDescriptor.self, forKey: .windows, into: log)
    }
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
    /// Workspace IDs whose sidebar disclosure is collapsed in this window.
    /// Absence = expanded (the default). Only honored when this window is current;
    /// other-window rows always start collapsed regardless of saved state.
    var collapsedWorkspaceIds: [UUID] = []

    init(
        id: UUID,
        workspaceId: UUID? = nil,
        ownedWorkspaceIds: [UUID] = [],
        windowName: String? = nil,
        frame: WindowFrame,
        space: SpaceSnapshot? = nil,
        collapsedWorkspaceIds: [UUID] = []
    ) {
        self.id = id
        self.workspaceId = workspaceId
        self.ownedWorkspaceIds = ownedWorkspaceIds
        self.windowName = windowName
        self.frame = frame
        self.space = space
        self.collapsedWorkspaceIds = collapsedWorkspaceIds
    }

    // Custom decoder so that adding optional-with-default fields later does not
    // break loading older state.json files. Swift's synthesized `init(from:)`
    // calls `decode` (not `decodeIfPresent`) for non-Optional properties, even
    // when they have a default value, which throws `keyNotFound` if the JSON
    // omits the key. Using `decodeIfPresent` here keeps the defaults working.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(UUID.self, forKey: .id)
        workspaceId = try c.decodeIfPresent(UUID.self, forKey: .workspaceId)
        ownedWorkspaceIds = try c.decodeIfPresent([UUID].self, forKey: .ownedWorkspaceIds) ?? []
        windowName = try c.decodeIfPresent(String.self, forKey: .windowName)
        frame = try c.decode(WindowFrame.self, forKey: .frame)
        space = try c.decodeIfPresent(SpaceSnapshot.self, forKey: .space)
        collapsedWorkspaceIds = try c.decodeIfPresent([UUID].self, forKey: .collapsedWorkspaceIds) ?? []
    }
}

import Foundation

enum PersistenceService {
    private static var stateURL: URL {
        let appSupport = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        let dir = appSupport.appendingPathComponent("ccmux", isDirectory: true)

        if !FileManager.default.fileExists(atPath: dir.path) {
            try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        }

        return dir.appendingPathComponent("state.json")
    }

    static func load() -> AppState? {
        guard let data = try? Data(contentsOf: stateURL) else { return nil }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601

        if let state = try? decoder.decode(AppState.self, from: data) {
            return state
        }

        // Legacy fallback: old format stored `layout: SplitTree<PaneContent>` (one PaneContent
        // per leaf). Try to decode that shape and wrap each leaf in a single-tab PaneTabs.
        if let migrated = try? decoder.decode(LegacyAppState.self, from: data).migrated() {
            return migrated
        }

        // Decoding failed entirely. Preserve the un-decodable file under a
        // timestamped name so the next save() doesn't silently overwrite it.
        // (This is the safety net that would have saved the May 2026 collapse-
        // expansion field rollout if it had been in place earlier.)
        preserveUnreadableState(data: data)
        return nil
    }

    private static func preserveUnreadableState(data: Data) {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        formatter.timeZone = TimeZone.current
        let timestamp = formatter.string(from: Date())
        let backupURL = stateURL.deletingLastPathComponent()
            .appendingPathComponent("state.unreadable-\(timestamp).json")
        do {
            try data.write(to: backupURL, options: .atomic)
            NSLog("[ccmux] state.json could not be decoded — preserved a copy at %@", backupURL.path)
        } catch {
            NSLog("[ccmux] state.json could not be decoded AND backup failed: %@", String(describing: error))
        }
    }

    static func save(_ state: AppState) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        encoder.dateEncodingStrategy = .iso8601

        guard let data = try? encoder.encode(state) else { return }
        try? data.write(to: stateURL, options: .atomic)
    }
}

// MARK: - Legacy (single-content-per-leaf) migration

/// Mirror of `AppState` as of the pre-tabs schema. Used only for one-shot migration.
private struct LegacyAppState: Codable {
    var workspaces: [LegacyWorkspace]
    var closedWorkspaces: [LegacyWorkspace] = []
    var closedWindows: [ClosedWindow] = []
    var activeWorkspaceId: UUID?
    var version: Int = 2
    var windowFrame: WindowFrame?
    var windows: [WindowDescriptor] = []

    func migrated() -> AppState {
        AppState(
            workspaces: workspaces.map { $0.migrated() },
            closedWorkspaces: closedWorkspaces.map { $0.migrated() },
            closedWindows: closedWindows,
            activeWorkspaceId: activeWorkspaceId,
            version: version,
            windowFrame: windowFrame,
            windows: windows
        )
    }
}

private struct LegacyWorkspace: Codable, Identifiable {
    let id: UUID
    var name: String
    var repoPath: String
    var layout: SplitTree<PaneContent>
    var focusedPaneId: UUID?
    var subItems: [WorkspaceSubItem]
    var lastOpened: Date
    var lastWindowFrame: WindowFrame?

    func migrated() -> Workspace {
        Workspace(
            id: id,
            name: name,
            repoPath: repoPath,
            layout: Self.migrateTree(layout),
            focusedPaneId: focusedPaneId,
            subItems: subItems,
            lastOpened: lastOpened,
            lastWindowFrame: lastWindowFrame
        )
    }

    /// Convert an old `SplitTree<PaneContent>` into a `SplitTree<PaneTabs>` by wrapping
    /// each leaf's content in a single-tab `PaneTabs`. Leaf IDs are preserved so focus
    /// and TerminalStore keys stay stable across the migration.
    private static func migrateTree(_ old: SplitTree<PaneContent>) -> SplitTree<PaneTabs> {
        switch old {
        case .leaf(let id, let content):
            let tabs = PaneTabs(id: id, tabs: [content], activeTabId: content.id)
            return .leaf(id: id, content: tabs)
        case .split(let id, let direction, let ratio, let first, let second):
            return .split(
                id: id,
                direction: direction,
                ratio: ratio,
                first: migrateTree(first),
                second: migrateTree(second)
            )
        }
    }
}

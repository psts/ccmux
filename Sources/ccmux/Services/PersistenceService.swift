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

    /// What `decode` made of a state.json. `partial` and `failed` both mean data was
    /// lost, so both get the file preserved before anything overwrites it.
    enum LoadOutcome {
        case clean(AppState)
        /// Some entries were dropped; `summary` names the fields and counts.
        case partial(AppState, summary: String)
        /// Decoded through the pre-tabs migration path.
        case legacy(AppState)
        /// Nothing survived; `reason` is the underlying DecodingError.
        case failed(reason: String)

        var state: AppState? {
            switch self {
            case .clean(let s), .partial(let s, _), .legacy(let s): return s
            case .failed: return nil
            }
        }
    }

    /// Set by `load()` when state.json did not come back whole, for the launch path
    /// to show the user. Names the loss and where the untouched copy went — a syslog
    /// line they will never read is not a notification.
    private(set) static var lastLoadWarning: String?

    static func load() -> AppState? {
        guard let data = try? Data(contentsOf: stateURL) else { return nil }
        let outcome = decode(data)
        switch outcome {
        case .partial(_, let summary):
            // A partial decode means data on the floor, and the autosave that fires
            // ~300ms into launch would overwrite the original. Back it up first.
            lastLoadWarning = warning(
                "Some saved state could not be read (\(summary)).",
                backup: preserveState(data: data, reason: "partially decoded (\(summary))"))
        case .failed(let reason):
            lastLoadWarning = warning(
                "Your saved workspaces could not be read.",
                backup: preserveState(data: data, reason: "could not be decoded: \(reason)"))
        case .clean, .legacy:
            break
        }
        return outcome.state
    }

    private static func warning(_ lead: String, backup: URL?) -> String {
        guard let backup else { return "\(lead) Saving a copy of the previous state also failed." }
        return "\(lead) The previous state.json was left untouched at \(backup.path)."
    }

    /// Pure decode — no file I/O, so it unit-tests without touching the real state.json.
    static func decode(_ data: Data) -> LoadOutcome {
        let log = DecodeDropLog()
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        decoder.userInfo[DecodeDropLog.key] = log

        var primaryFailure: Error?
        let partial: AppState?
        do {
            partial = try decoder.decode(AppState.self, from: data)
        } catch {
            partial = nil
            primaryFailure = error
        }
        if let state = partial, log.isEmpty { return .clean(state) }

        // Legacy fallback: old format stored `layout: SplitTree<PaneContent>` (one PaneContent
        // per leaf). Try to decode that shape and wrap each leaf in a single-tab PaneTabs.
        //
        // Also reached when a whole array above was dropped: losing every entry of a field
        // means the wrong schema, not corruption, and the lenient decode must not let a
        // legacy file through as an empty state. (`decoder` is reused deliberately —
        // LegacyAppState decodes strictly and never touches the drop log.)
        if partial == nil || log.lostAnEntireField,
           let migrated = try? decoder.decode(LegacyAppState.self, from: data).migrated() {
            return .legacy(migrated)
        }

        // Keep what survived — the alternative is discarding the intact entries too.
        if let state = partial { return .partial(state, summary: log.summary) }

        // Decoding failed entirely. The caller preserves the un-decodable file under a
        // timestamped name so the next save() doesn't silently overwrite it. (This is the
        // safety net that would have saved the May 2026 collapse-expansion field rollout
        // if it had been in place earlier.) The DecodingError names the failing key path,
        // which usually identifies the bad rollout on sight.
        return .failed(reason: primaryFailure.map { String(describing: $0) } ?? "no usable schema matched")
    }

    /// Copy the on-disk bytes aside before anything overwrites them. Returns where they
    /// landed, or nil when even the backup failed.
    @discardableResult
    private static func preserveState(data: Data, reason: String) -> URL? {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        formatter.timeZone = TimeZone.current
        let timestamp = formatter.string(from: Date())
        let backupURL = stateURL.deletingLastPathComponent()
            .appendingPathComponent("state.unreadable-\(timestamp).json")
        do {
            try data.write(to: backupURL, options: .atomic)
            NSLog("[ccmux] state.json %@ — preserved a copy at %@", reason, backupURL.path)
            return backupURL
        } catch {
            NSLog("[ccmux] state.json %@ AND backup failed: %@", reason, String(describing: error))
            return nil
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

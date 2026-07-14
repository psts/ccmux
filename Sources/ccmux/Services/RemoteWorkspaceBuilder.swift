import Foundation

/// Pure construction of app-side models from daemon workspace data — no I/O, so it
/// unit-tests cleanly. `RemoteSessionService` wraps the runtime around it.
enum RemoteWorkspaceBuilder {

    /// Stable app UUID for a daemon workspace/pane. Daemon ids are `uuid.NewString()`,
    /// so this is normally a straight parse; the deterministic fallback keeps ids
    /// stable (no view churn) should the daemon ever emit a non-UUID id.
    static func workspaceUUID(_ id: String) -> UUID { UUID(uuidString: id) ?? deterministicUUID(seed: id) }
    static func paneUUID(_ id: String) -> UUID { UUID(uuidString: id) ?? deterministicUUID(seed: id) }

    /// A hosted pane as a `TerminalConfig` tagged `.hosted`. `shell` is unused (no
    /// local process); `workingDirectory` powers local-clone file-link resolution.
    static func terminalConfig(for pane: DaemonPane, repoPath: String) -> TerminalConfig {
        var config = TerminalConfig(
            id: paneUUID(pane.id),
            shell: "",
            workingDirectory: pane.cwd.isEmpty ? repoPath : pane.cwd,
            title: pane.title.isEmpty ? nil : pane.title,
            startupCommand: nil)
        config.host = .hosted(paneId: pane.id)
        return config
    }

    /// Build the workspace's SplitTree. A stored `layoutBlob` (Phase 7 layout sync)
    /// restores the exact pane arrangement; otherwise panes chain left-to-right.
    /// Returns nil when the workspace has no panes.
    static func buildTree(panes: [DaemonPane], repoPath: String, layoutBlob: String? = nil) -> (tree: SplitTree<PaneTabs>, focused: UUID)? {
        guard let first = panes.first else { return nil }
        if let blob = layoutBlob, let restored = restoredTree(blob: blob, panes: panes) {
            return restored
        }
        let firstTabs = PaneTabs(single: .terminal(terminalConfig(for: first, repoPath: repoPath)))
        var tree: SplitTree<PaneTabs> = .leaf(id: firstTabs.id, content: firstTabs)
        // `splitLeaf` preserves the *target* leaf's id but mints a fresh id for the new
        // leaf, so we can only reliably re-target the first leaf (whose id we hold). Each
        // split therefore peels the next pane off the first leaf.
        for pane in panes.dropFirst() {
            let tabs = PaneTabs(single: .terminal(terminalConfig(for: pane, repoPath: repoPath)))
            tree = tree.splitLeaf(targetId: firstTabs.id, direction: .horizontal, newContent: tabs)
        }
        return (tree, firstTabs.id)
    }

    /// Decode a layout blob and accept it only when its hosted pane set *exactly*
    /// matches the workspace's live panes — a stale blob (a pane was added/closed on
    /// the daemon) is rejected so we never render a broken arrangement; the caller
    /// falls back to the default chain.
    static func restoredTree(blob: String, panes: [DaemonPane]) -> (tree: SplitTree<PaneTabs>, focused: UUID)? {
        guard let tree = HostedLayoutCodec.decode(blob) else { return nil }
        let live = Set(panes.map { $0.id })
        guard HostedLayoutCodec.hostedPaneIds(tree) == live else { return nil }
        guard let firstLeaf = tree.allLeaves.first else { return nil }
        return (tree, firstLeaf.id)
    }

    /// Ordered daemon pane-id set — a change means the layout must be rebuilt;
    /// no change means keep the live tree/connection untouched (no churn).
    static func paneSignature(_ panes: [DaemonPane]) -> [String] { panes.map { $0.id } }

    /// FNV-1a(seed) → 16 bytes → UUID. Deterministic, so a given seed always maps to
    /// the same id across refreshes and launches.
    static func deterministicUUID(seed: String) -> UUID {
        var bytes = [UInt8]()
        var hash: UInt64 = 0xcbf29ce484222325
        // Two rounds (with a salt tweak) fill all 16 bytes.
        for round: UInt8 in 0..<2 {
            hash ^= UInt64(round &+ 1)
            for b in seed.utf8 { hash ^= UInt64(b); hash = hash &* 0x100000001b3 }
            withUnsafeBytes(of: hash.bigEndian) { bytes.append(contentsOf: $0) }
        }
        return bytes.prefix(16).withUnsafeBytes {
            NSUUID(uuidBytes: $0.bindMemory(to: UInt8.self).baseAddress) as UUID
        }
    }
}

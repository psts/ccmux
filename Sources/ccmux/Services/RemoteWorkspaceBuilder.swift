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
        config.dormant = pane.dormant
        return config
    }

    /// Build the workspace's SplitTree. A stored `layoutBlob` (Phase 7 layout sync)
    /// restores the saved arrangement merged with the live pane set — daemon-side
    /// pane churn (dev-server start/stop, teammate spawn, another lens) must never
    /// reset the layout. Without a usable blob, panes chain left-to-right.
    /// Returns nil when the workspace has no panes.
    static func buildTree(panes: [DaemonPane], repoPath: String, layoutBlob: String? = nil) -> (tree: SplitTree<PaneTabs>, focused: UUID)? {
        guard let first = panes.first else { return nil }
        if let blob = layoutBlob, let restored = restoredTree(blob: blob, panes: panes, repoPath: repoPath) {
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

    /// Decode a layout blob and reconcile it with the live pane set instead of
    /// demanding an exact match. Nil only when nothing usable remains (garbage
    /// blob, or every referenced pane is gone); the caller falls back to the chain.
    static func restoredTree(blob: String, panes: [DaemonPane], repoPath: String) -> (tree: SplitTree<PaneTabs>, focused: UUID)? {
        guard let decoded = HostedLayoutCodec.decode(blob),
              let tree = mergedTree(decoded, panes: panes, repoPath: repoPath),
              let firstLeaf = tree.allLeaves.first else { return nil }
        return (tree, firstLeaf.id)
    }

    /// Reconcile a tree with the live pane set: hosted tabs whose daemon pane is
    /// gone are pruned (a leaf losing its last tab collapses), and live panes the
    /// tree doesn't reference land as tabs on the last leaf — geometry, ratios, and
    /// non-hosted tabs all survive. Works on a decoded blob or a live controller
    /// tree (the in-place reconcile path). Deterministic — new tabs get the pane's
    /// deterministic UUID — so independent lenses converge on identical trees.
    /// Nil when no leaf remains.
    static func mergedTree(_ tree: SplitTree<PaneTabs>, panes: [DaemonPane], repoPath: String) -> SplitTree<PaneTabs>? {
        guard let pruned = pruneStalePanes(tree, liveIds: Set(panes.map { $0.id })) else { return nil }
        return insertNewPanes(
            pruned, panes: panes,
            alreadyPlaced: HostedLayoutCodec.hostedPaneIds(tree), repoPath: repoPath)
    }

    /// Fold refreshed daemon pane titles into a tree's hosted terminal tabs (the
    /// daemon re-derives titles live from tmux — "Claude", "Terminal", "vim").
    /// Returns nil when nothing changed so callers can skip republishing.
    static func updatingTitles(_ tree: SplitTree<PaneTabs>, panes: [DaemonPane]) -> SplitTree<PaneTabs>? {
        let titles = Dictionary(panes.map { ($0.id, $0.title) }, uniquingKeysWith: { a, _ in a })
        // Dormancy rides the same reconcile as titles: it changes for the same
        // reason (the pane's foreground command moved) and must not need its own.
        let dormant = Dictionary(panes.map { ($0.id, $0.dormant) }, uniquingKeysWith: { a, _ in a })
        var result = tree
        var changed = false
        for (leafId, tabs) in tree.allLeaves {
            var newTabs = tabs
            var leafChanged = false
            for (idx, tab) in tabs.tabs.enumerated() {
                guard case .terminal(var cfg) = tab, let paneId = cfg.host.hostedPaneId,
                      let raw = titles[paneId] else { continue }
                let title: String? = raw.isEmpty ? nil : raw
                let isDormant = dormant[paneId] ?? false
                if cfg.title != title || cfg.dormant != isDormant {
                    cfg.title = title
                    cfg.dormant = isDormant
                    newTabs.tabs[idx] = .terminal(cfg)
                    leafChanged = true
                }
            }
            if leafChanged {
                result = result.replaceContent(leafId: leafId, newContent: newTabs)
                changed = true
            }
        }
        return changed ? result : nil
    }

    /// Place a freshly spawned daemon pane at the leaf the user acted on — appended
    /// as a tab (direction nil) or as a split. Dedupe: if a racing reconcile already
    /// merged the pane in, returns nil and it stays where the merge put it. If the
    /// target leaf vanished mid-flight (closed during the POST), falls back to the
    /// merge policy: a tab on the last leaf.
    static func insertingPane(
        _ pane: DaemonPane, into tree: SplitTree<PaneTabs>, at leafId: UUID,
        direction: SplitDirection?, repoPath: String
    ) -> (tree: SplitTree<PaneTabs>, focusLeafId: UUID)? {
        guard !HostedLayoutCodec.hostedPaneIds(tree).contains(pane.id) else { return nil }
        let content = PaneContent.terminal(terminalConfig(for: pane, repoPath: repoPath))
        let targetExists = tree.findLeaf(id: leafId) != nil
        guard let targetId = targetExists ? leafId : tree.allLeaves.last?.id else { return nil }
        if let direction, targetExists {
            let tabs = PaneTabs(single: content)
            let split = tree.splitLeaf(targetId: targetId, direction: direction, newContent: tabs)
            guard let leaf = split.allLeaves.first(where: { $0.content.id == tabs.id }) else { return nil }
            return (split, leaf.id)
        }
        guard var target = tree.findLeaf(id: targetId) else { return nil }
        target.addTab(content)
        return (tree.replaceContent(leafId: targetId, newContent: target), targetId)
    }

    /// Drop hosted-terminal tabs whose daemon pane no longer exists. A leaf whose
    /// tabs all vanish collapses (its sibling takes the space); one that keeps other
    /// tabs survives with a valid active pointer. Nil when no leaf remains.
    private static func pruneStalePanes(_ tree: SplitTree<PaneTabs>, liveIds: Set<String>) -> SplitTree<PaneTabs>? {
        var result: SplitTree<PaneTabs>? = tree
        for (leafId, tabs) in tree.allLeaves {
            let kept = tabs.tabs.filter { tab in
                guard case .terminal(let cfg) = tab, let paneId = cfg.host.hostedPaneId else { return true }
                return liveIds.contains(paneId)
            }
            if kept.count == tabs.tabs.count { continue }
            if kept.isEmpty {
                result = result?.closeLeaf(targetId: leafId)
            } else {
                let active = kept.contains { $0.id == tabs.activeTabId } ? tabs.activeTabId : kept[0].id
                result = result?.replaceContent(
                    leafId: leafId, newContent: PaneTabs(id: tabs.id, tabs: kept, activeTabId: active))
            }
        }
        return result
    }

    /// Append panes the blob doesn't reference as tabs on the last leaf — no
    /// geometry change (a tab can always be dragged out into a split), and the
    /// newest tab becomes active so a just-started dev server shows its logs.
    private static func insertNewPanes(
        _ tree: SplitTree<PaneTabs>, panes: [DaemonPane], alreadyPlaced: Set<String>, repoPath: String
    ) -> SplitTree<PaneTabs> {
        let missing = panes.filter { !alreadyPlaced.contains($0.id) }
        guard !missing.isEmpty, let target = tree.allLeaves.last else { return tree }
        var tabs = target.content
        for pane in missing {
            tabs.addTab(.terminal(terminalConfig(for: pane, repoPath: repoPath)))
        }
        return tree.replaceContent(leafId: target.id, newContent: tabs)
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

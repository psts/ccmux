import AppKit

/// Where a window opens and at what size.
///
/// A shared window opens at this Mac's own last size for it, else the size the
/// daemon says any Mac last had it at, else `standard`, always fitted to the
/// screen and centered. Only the size travels: where a window sits depends on
/// the screen it is on, and two people's screens differ.
enum WindowSizing {
    /// The size a window opens at when nobody has left it one.
    static let standard = NSSize(width: 1200, height: 800)

    /// The size rule, apart from the screen.
    static func pick(mine: NSSize?, shared: NSSize?) -> NSSize {
        mine ?? shared ?? standard
    }

    /// A frame of `size`, grown to at least `minSize`, shrunk to fit
    /// `visible`, and centered in it.
    static func centeredFrame(_ size: NSSize, in visible: NSRect, minSize: NSSize) -> NSRect {
        let width = min(max(size.width, minSize.width), visible.width)
        let height = min(max(size.height, minSize.height), visible.height)
        return NSRect(
            x: (visible.midX - width / 2).rounded(),
            y: (visible.midY - height / 2).rounded(),
            width: width, height: height)
    }

    /// The daemon id of the shared window a Mac window shows. The Mac window's
    /// own UUID is minted fresh on every open, so it cannot be the key.
    ///
    /// A stamped id (set when the window was opened from the shared list) wins
    /// while the daemon still has it. Otherwise the window's own name, then its
    /// sessions, the same order `resolveOpenSet` uses. Never the automatic
    /// "Window N": it shifts as other windows close, and would hand one
    /// window's size to another. An empty list means it has not loaded yet,
    /// not that nothing exists, so the stamp stands unchecked.
    static func sharedWindowId(
        stamped: String?, name: String?, members: Set<UUID>, shared: [DaemonWindow]
    ) -> String? {
        if shared.isEmpty { return stamped }
        if let stamped, shared.contains(where: { $0.id == stamped }) { return stamped }
        if let name, let hit = shared.first(where: { WindowManager.sameWindowName($0.name, name) }) {
            return hit.id
        }
        let byMembers = shared.first { win in
            win.workspaceIds.contains { members.contains(RemoteWorkspaceBuilder.workspaceUUID($0)) }
        }
        return byMembers?.id
    }

    /// The shared window a Mac window is, from its context: the stamp, its OWN
    /// name (never `autoName`), then its sessions with the displayed one
    /// folded in. The one place those inputs are picked, so a test can hold
    /// the real choice to the rule rather than a copy of it.
    static func sharedWindowId(for context: WindowContext, shared: [DaemonWindow]) -> String? {
        var members = context.ownedWorkspaceIds
        if let displayed = context.displayedWorkspaceId { members.insert(displayed) }
        return sharedWindowId(
            stamped: context.sharedWindowId, name: context.windowName, members: members, shared: shared)
    }
}

extension DaemonWindow {
    /// The size the daemon says a Mac last had this window at. Nil when nobody
    /// has, or when the daemon predates sizes.
    var sharedSize: NSSize? {
        guard let width, let height, width > 0, height > 0 else { return nil }
        return NSSize(width: width, height: height)
    }
}

/// This Mac's own last size for each shared window, keyed by the daemon's
/// window id. Per Mac rather than per person, because the right size depends
/// on the screen.
struct SharedWindowSizeMemory {
    static let key = "ccmux.sharedWindowSizes"
    var defaults: UserDefaults = .standard

    func size(for id: String) -> NSSize? {
        guard let pair = defaults.dictionary(forKey: Self.key)?[id] as? [Double],
              pair.count == 2, pair[0] > 0, pair[1] > 0
        else { return nil }
        return NSSize(width: pair[0], height: pair[1])
    }

    func remember(_ size: NSSize, for id: String) {
        var all = defaults.dictionary(forKey: Self.key) ?? [:]
        all[id] = [Double(size.width), Double(size.height)]
        defaults.set(all, forKey: Self.key)
    }
}

/// Keeps shared-window sizes: picks where one opens, and when one settles at
/// a new size, remembers it here and hands it to `publish` for the daemon.
final class SharedWindowSizer {
    private let memory: SharedWindowSizeMemory
    private let publish: (String, NSSize) -> Void
    /// The size each shared window was last seen at, so the every-reconcile
    /// pass writes nothing for a window that has not changed.
    private var lastSeen: [String: NSSize] = [:]

    init(memory: SharedWindowSizeMemory = SharedWindowSizeMemory(), publish: @escaping (String, NSSize) -> Void) {
        self.memory = memory
        self.publish = publish
    }

    func openingFrame(for win: DaemonWindow, in visible: NSRect, minSize: NSSize) -> NSRect {
        let size = WindowSizing.pick(mine: memory.size(for: win.id), shared: win.sharedSize)
        return WindowSizing.centeredFrame(size, in: visible, minSize: minSize)
    }

    /// A window just opened at `size`. That is this Mac's size for it now, and
    /// not news to the daemon: opening is not resizing, so it publishes nothing.
    func opened(_ id: String, at size: NSSize) {
        lastSeen[id] = size
        memory.remember(size, for: id)
    }

    /// A window's size settled: after a resize, on close, or on the reconcile
    /// pass, which is what publishes a window already open at launch. Full
    /// screen is skipped: it is not a size anyone wants a window to open at.
    func settled(_ id: String, size: NSSize, fullScreen: Bool) {
        guard !fullScreen, lastSeen[id] != size else { return }
        lastSeen[id] = size
        memory.remember(size, for: id)
        publish(id, size)
    }
}

import Foundation

/// Runtime state for a File Explorer pane. Manages open file tabs, content, and modification tracking.
/// This is NOT persisted directly — the Codable `FileExplorerConfig` stores only paths.
///
/// All file access goes through `source` so the same explorer works for local
/// workspaces (disk) and hosted ones (daemon file routes). Reads and writes are
/// async; tab mutations always land back on the main thread.
class FileExplorerState: ObservableObject {
    let rootPath: String
    let source: FileSource

    @Published var openTabs: [FileTab] = []
    @Published var activeTabId: UUID?

    struct FileTab: Identifiable {
        let id: UUID
        let relativePath: String
        let absolutePath: String
        var content: String
        var originalContent: String
        var isPreviewMode: Bool = false
        var diskChangedExternally: Bool = false
        /// Last save attempt failed (daemon unreachable, file gone, too large).
        var saveErrored: Bool = false
        var isModified: Bool { content != originalContent }

        var filename: String {
            (relativePath as NSString).lastPathComponent
        }

        var isMarkdown: Bool {
            let ext = (relativePath as NSString).pathExtension.lowercased()
            return ext == "md" || ext == "markdown"
        }
    }

    private var watchers: [UUID: FileWatcher] = [:]

    init(rootPath: String, source: FileSource? = nil) {
        self.rootPath = rootPath
        self.source = source ?? LocalFileSource(rootPath: rootPath)
    }

    deinit {
        for w in watchers.values { w.stop() }
    }

    // MARK: - Tab Operations

    func openFile(relativePath: String) {
        // If already open, just activate it
        if let existing = openTabs.first(where: { $0.relativePath == relativePath }) {
            activeTabId = existing.id
            return
        }
        Task { @MainActor in await self.openFileNow(relativePath: relativePath) }
    }

    /// Fetches and appends the tab. Re-checks for an existing tab after the
    /// await — a double-click races two fetches for the same path.
    @MainActor
    private func openFileNow(relativePath: String) async {
        guard let content = await source.read(path: relativePath) else { return }
        if let existing = openTabs.first(where: { $0.relativePath == relativePath }) {
            activeTabId = existing.id
            return
        }
        let absolutePath = relativePath.hasPrefix("/")
            ? relativePath
            : (rootPath as NSString).appendingPathComponent(relativePath)
        let isMd = relativePath.hasSuffix(".md") || relativePath.hasSuffix(".markdown")
        let tab = FileTab(
            id: UUID(),
            relativePath: relativePath,
            absolutePath: absolutePath,
            content: content,
            originalContent: content,
            isPreviewMode: isMd
        )
        openTabs.append(tab)
        activeTabId = tab.id
        if source.watchesDisk {
            startWatcher(for: tab.id, path: absolutePath)
        }
    }

    func closeTab(id: UUID) {
        watchers[id]?.stop()
        watchers.removeValue(forKey: id)
        openTabs.removeAll { $0.id == id }
        if activeTabId == id {
            activeTabId = openTabs.last?.id
        }
    }

    func activateTab(id: UUID) {
        activeTabId = id
    }

    func updateContent(tabId: UUID, newContent: String) {
        guard let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
        openTabs[idx].content = newContent
    }

    func togglePreview(tabId: UUID) {
        guard let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
        openTabs[idx].isPreviewMode.toggle()
    }

    func saveActiveFile() {
        guard let id = activeTabId,
              let idx = openTabs.firstIndex(where: { $0.id == id }) else { return }
        let tab = openTabs[idx]
        Task { @MainActor in
            let ok = await source.write(path: tab.relativePath, content: tab.content)
            guard let idx = openTabs.firstIndex(where: { $0.id == tab.id }) else { return }
            openTabs[idx].saveErrored = !ok
            guard ok else { return }
            openTabs[idx].originalContent = tab.content
            openTabs[idx].diskChangedExternally = false
        }
    }

    func dismissSaveError(tabId: UUID) {
        guard let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
        openTabs[idx].saveErrored = false
    }

    // MARK: - External Disk Changes

    func reloadFromDisk(tabId: UUID) {
        guard let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
        let path = openTabs[idx].relativePath
        Task { @MainActor in
            guard let newContent = await source.read(path: path),
                  let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
            openTabs[idx].content = newContent
            openTabs[idx].originalContent = newContent
            openTabs[idx].diskChangedExternally = false
        }
    }

    func dismissDiskChange(tabId: UUID) {
        guard let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
        openTabs[idx].diskChangedExternally = false
    }

    private func startWatcher(for tabId: UUID, path: String) {
        let watcher = FileWatcher(path: path) { [weak self] in
            DispatchQueue.main.async { self?.handleDiskChange(tabId: tabId) }
        }
        watchers[tabId] = watcher
        watcher.start()
    }

    private func handleDiskChange(tabId: UUID) {
        guard let idx = openTabs.firstIndex(where: { $0.id == tabId }) else { return }
        let tab = openTabs[idx]
        guard let data = FileManager.default.contents(atPath: tab.absolutePath),
              let newContent = String(data: data, encoding: .utf8) else { return }
        // Self-write echo: disk now matches what we last loaded/saved. No-op.
        if newContent == tab.originalContent { return }
        if tab.isModified {
            // Don't clobber unsaved edits — let the UI surface a banner.
            if !openTabs[idx].diskChangedExternally {
                openTabs[idx].diskChangedExternally = true
            }
        } else {
            openTabs[idx].content = newContent
            openTabs[idx].originalContent = newContent
            openTabs[idx].diskChangedExternally = false
        }
    }

    // MARK: - Persistence

    /// Create a Codable config snapshot for saving.
    func persistableConfig(id: UUID) -> FileExplorerConfig {
        FileExplorerConfig(
            id: id,
            rootPath: rootPath,
            openFilePaths: openTabs.map(\.relativePath),
            activeFilePath: openTabs.first(where: { $0.id == activeTabId })?.relativePath
        )
    }

    /// Restore open files from a saved config. Opens are async, so the active
    /// tab is re-applied once every restored file has loaded.
    func restoreFromConfig(_ config: FileExplorerConfig) {
        Task { @MainActor in
            for path in config.openFilePaths {
                if !openTabs.contains(where: { $0.relativePath == path }) {
                    await openFileNow(relativePath: path)
                }
            }
            if let activePath = config.activeFilePath,
               let tab = openTabs.first(where: { $0.relativePath == activePath }) {
                activeTabId = tab.id
            }
        }
    }
}

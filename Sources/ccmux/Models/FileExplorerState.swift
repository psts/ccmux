import Foundation

/// Runtime state for a File Explorer pane. Manages open file tabs, content, and modification tracking.
/// This is NOT persisted directly — the Codable `FileExplorerConfig` stores only paths.
class FileExplorerState: ObservableObject {
    let rootPath: String

    @Published var openTabs: [FileTab] = []
    @Published var activeTabId: UUID?

    struct FileTab: Identifiable {
        let id: UUID
        let relativePath: String
        let absolutePath: String
        var content: String
        var originalContent: String
        var isModified: Bool { content != originalContent }

        var filename: String {
            (relativePath as NSString).lastPathComponent
        }
    }

    init(rootPath: String) {
        self.rootPath = rootPath
    }

    // MARK: - Tab Operations

    func openFile(relativePath: String) {
        // If already open, just activate it
        if let existing = openTabs.first(where: { $0.relativePath == relativePath }) {
            activeTabId = existing.id
            return
        }

        let absolutePath = (rootPath as NSString).appendingPathComponent(relativePath)
        guard let data = FileManager.default.contents(atPath: absolutePath),
              let content = String(data: data, encoding: .utf8) else { return }

        let tab = FileTab(
            id: UUID(),
            relativePath: relativePath,
            absolutePath: absolutePath,
            content: content,
            originalContent: content
        )
        openTabs.append(tab)
        activeTabId = tab.id
    }

    func closeTab(id: UUID) {
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

    func saveActiveFile() -> Bool {
        guard let id = activeTabId,
              let idx = openTabs.firstIndex(where: { $0.id == id }) else { return false }
        let tab = openTabs[idx]
        do {
            try tab.content.write(toFile: tab.absolutePath, atomically: true, encoding: .utf8)
            openTabs[idx].originalContent = tab.content
            return true
        } catch {
            return false
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

    /// Restore open files from a saved config.
    func restoreFromConfig(_ config: FileExplorerConfig) {
        for path in config.openFilePaths {
            openFile(relativePath: path)
        }
        if let activePath = config.activeFilePath,
           let tab = openTabs.first(where: { $0.relativePath == activePath }) {
            activeTabId = tab.id
        }
    }
}

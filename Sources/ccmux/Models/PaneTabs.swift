import Foundation

/// A pane holds one or more tabs of `PaneContent`. Exactly one is active at a time.
/// Invariant: `tabs` is non-empty and `activeTabId` matches some element's id.
struct PaneTabs: Codable, Identifiable {
    let id: UUID
    var tabs: [PaneContent]
    var activeTabId: UUID

    init(id: UUID = UUID(), tabs: [PaneContent], activeTabId: UUID) {
        self.id = id
        self.tabs = tabs
        self.activeTabId = activeTabId
    }

    /// Wrap a single piece of content as a one-tab pane.
    init(single content: PaneContent, id: UUID = UUID()) {
        self.id = id
        self.tabs = [content]
        self.activeTabId = content.id
    }

    var activeTab: PaneContent? {
        tabs.first { $0.id == activeTabId }
    }

    mutating func addTab(_ content: PaneContent) {
        tabs.append(content)
        activeTabId = content.id
    }

    /// Remove a tab. Returns true on success, false if it was the last remaining tab
    /// (caller should close the whole pane in that case).
    mutating func removeTab(tabId: UUID) -> Bool {
        guard tabs.count > 1 else { return false }
        guard let idx = tabs.firstIndex(where: { $0.id == tabId }) else { return false }
        tabs.remove(at: idx)
        if activeTabId == tabId {
            let newIdx = min(idx, tabs.count - 1)
            activeTabId = tabs[newIdx].id
        }
        return true
    }

    mutating func updateTab(tabId: UUID, newContent: PaneContent) {
        guard let idx = tabs.firstIndex(where: { $0.id == tabId }) else { return }
        tabs[idx] = newContent
        // If the id changed (shouldn't happen, but be defensive), keep the active pointer valid.
        if activeTabId == tabId && newContent.id != tabId {
            activeTabId = newContent.id
        }
    }
}

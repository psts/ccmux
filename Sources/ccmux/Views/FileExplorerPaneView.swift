import SwiftUI
import AppKit

// MARK: - Main File Explorer Pane

struct FileExplorerPaneView: View {
    let paneId: UUID
    let config: FileExplorerConfig
    @ObservedObject var controller: SplitTreeController

    var body: some View {
        if let state = controller.fileExplorerState(for: paneId) {
            FileExplorerContent(state: state, onStateChange: {
                controller.updateFileExplorerConfig(leafId: paneId)
            })
        } else {
            VStack {
                Image(systemName: "exclamationmark.triangle")
                    .font(.system(size: 24))
                    .foregroundColor(.secondary)
                Text("Failed to initialize file explorer")
                    .font(.system(size: 12))
                    .foregroundColor(.secondary)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(Color(nsColor: NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)))
        }
    }
}

// MARK: - Content (split: tree + editor)

private struct FileExplorerContent: View {
    @ObservedObject var state: FileExplorerState
    let onStateChange: () -> Void

    var body: some View {
        HSplitView {
            // Left: File tree
            FileTreeView(
                rootPath: state.rootPath,
                onFileSelected: { relativePath in
                    state.openFile(relativePath: relativePath)
                    onStateChange()
                }
            )
            .frame(minWidth: 140, idealWidth: 200, maxWidth: 350)

            // Right: Tabs + Editor
            VStack(spacing: 0) {
                if !state.openTabs.isEmpty {
                    FileTabBarView(
                        tabs: state.openTabs,
                        activeTabId: state.activeTabId,
                        onActivate: { id in
                            state.activateTab(id: id)
                            onStateChange()
                        },
                        onClose: { id in
                            state.closeTab(id: id)
                            onStateChange()
                        }
                    )

                    if let activeId = state.activeTabId,
                       let tab = state.openTabs.first(where: { $0.id == activeId }) {
                        FileEditorView(
                            tabId: tab.id,
                            content: tab.content,
                            onContentChange: { newContent in
                                state.updateContent(tabId: tab.id, newContent: newContent)
                            },
                            onSave: {
                                _ = state.saveActiveFile()
                                onStateChange()
                            }
                        )
                    }
                } else {
                    // Empty state
                    VStack(spacing: 8) {
                        Image(systemName: "doc.text")
                            .font(.system(size: 36))
                            .foregroundColor(.secondary.opacity(0.3))
                        Text("Select a file from the tree")
                            .font(.system(size: 12))
                            .foregroundColor(.secondary.opacity(0.5))
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
        }
        .background(Color(nsColor: NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)))
    }
}

// MARK: - File Tree

private struct FileTreeView: View {
    let rootPath: String
    let onFileSelected: (String) -> Void

    var body: some View {
        List {
            FileTreeNode(
                path: rootPath,
                relativePath: "",
                isRoot: true,
                onFileSelected: onFileSelected
            )
        }
        .listStyle(.sidebar)
        .scrollContentBackground(.hidden)
        .background(Color(nsColor: NSColor(red: 0.13, green: 0.14, blue: 0.16, alpha: 1.0)))
    }
}

private struct FileTreeNode: View {
    let path: String
    let relativePath: String
    let isRoot: Bool
    let onFileSelected: (String) -> Void

    @State private var children: [FileItem]?
    @State private var isExpanded: Bool = false

    struct FileItem: Identifiable {
        let id = UUID()
        let name: String
        let relativePath: String
        let absolutePath: String
        let isDirectory: Bool
    }

    var body: some View {
        if isDirectory(at: path) {
            DisclosureGroup(isExpanded: $isExpanded) {
                if let children {
                    ForEach(children) { child in
                        if child.isDirectory {
                            FileTreeNode(
                                path: child.absolutePath,
                                relativePath: child.relativePath,
                                isRoot: false,
                                onFileSelected: onFileSelected
                            )
                        } else {
                            fileRow(child)
                        }
                    }
                }
            } label: {
                Label(
                    (path as NSString).lastPathComponent,
                    systemImage: isExpanded ? "folder.fill" : "folder"
                )
                .font(.system(size: 11))
            }
            .onChange(of: isExpanded) { _, expanded in
                if expanded && children == nil {
                    loadChildren()
                }
            }
            .onAppear {
                if isRoot {
                    isExpanded = true
                    loadChildren()
                }
            }
        }
    }

    private func fileRow(_ item: FileItem) -> some View {
        HStack(spacing: 4) {
            Image(systemName: iconForFile(item.name))
                .font(.system(size: 10))
                .foregroundColor(colorForFile(item.name))
            Text(item.name)
                .font(.system(size: 11))
                .lineLimit(1)
        }
        .contentShape(Rectangle())
        .onTapGesture {
            onFileSelected(item.relativePath)
        }
    }

    private func loadChildren() {
        let fm = FileManager.default
        guard let contents = try? fm.contentsOfDirectory(atPath: path) else {
            children = []
            return
        }
        children = contents
            .filter { !$0.hasPrefix(".") }
            .sorted { lhs, rhs in
                let lhsIsDir = isDirectory(at: (path as NSString).appendingPathComponent(lhs))
                let rhsIsDir = isDirectory(at: (path as NSString).appendingPathComponent(rhs))
                if lhsIsDir != rhsIsDir { return lhsIsDir }
                return lhs.localizedCaseInsensitiveCompare(rhs) == .orderedAscending
            }
            .map { name in
                let absPath = (path as NSString).appendingPathComponent(name)
                let relPath = relativePath.isEmpty ? name : relativePath + "/" + name
                return FileItem(
                    name: name,
                    relativePath: relPath,
                    absolutePath: absPath,
                    isDirectory: isDirectory(at: absPath)
                )
            }
    }

    private func isDirectory(at path: String) -> Bool {
        var isDir: ObjCBool = false
        return FileManager.default.fileExists(atPath: path, isDirectory: &isDir) && isDir.boolValue
    }

    private func iconForFile(_ name: String) -> String {
        let ext = (name as NSString).pathExtension.lowercased()
        switch ext {
        case "swift": return "swift"
        case "js", "ts", "jsx", "tsx": return "curlybraces"
        case "json": return "curlybraces.square"
        case "md", "txt", "rtf": return "doc.text"
        case "py": return "chevron.left.forwardslash.chevron.right"
        case "html", "css", "scss": return "globe"
        case "png", "jpg", "jpeg", "gif", "svg", "ico": return "photo"
        case "yml", "yaml", "toml": return "gearshape"
        case "sh", "bash", "zsh": return "terminal"
        case "rs": return "gearshape.2"
        case "go": return "chevron.left.forwardslash.chevron.right"
        default: return "doc"
        }
    }

    private func colorForFile(_ name: String) -> Color {
        let ext = (name as NSString).pathExtension.lowercased()
        switch ext {
        case "swift": return .orange
        case "js", "jsx": return .yellow
        case "ts", "tsx": return .blue
        case "json": return .yellow.opacity(0.7)
        case "py": return .blue
        case "html": return .red.opacity(0.8)
        case "css", "scss": return .blue.opacity(0.7)
        case "md": return .cyan
        case "rs": return .orange.opacity(0.8)
        case "go": return .cyan
        default: return .secondary
        }
    }
}

// MARK: - Internal File Tab Bar

private struct FileTabBarView: View {
    let tabs: [FileExplorerState.FileTab]
    let activeTabId: UUID?
    let onActivate: (UUID) -> Void
    let onClose: (UUID) -> Void

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 0) {
                ForEach(tabs) { tab in
                    FileTabButton(
                        name: tab.filename,
                        isActive: tab.id == activeTabId,
                        isModified: tab.isModified,
                        onActivate: { onActivate(tab.id) },
                        onClose: { onClose(tab.id) }
                    )
                }
            }
        }
        .frame(height: 28)
        .background(Color(white: 0.12))
    }
}

private struct FileTabButton: View {
    let name: String
    let isActive: Bool
    let isModified: Bool
    let onActivate: () -> Void
    let onClose: () -> Void

    @State private var isHovered = false

    var body: some View {
        HStack(spacing: 4) {
            if isModified {
                Circle()
                    .fill(Color.orange)
                    .frame(width: 6, height: 6)
            }
            Text(name)
                .font(.system(size: 11))
                .lineLimit(1)
            Button(action: onClose) {
                Image(systemName: "xmark")
                    .font(.system(size: 8))
                    .foregroundColor(.secondary)
            }
            .buttonStyle(.plain)
            .opacity(isHovered || isActive ? 1 : 0)
        }
        .padding(.horizontal, 10)
        .frame(height: 28)
        .background(isActive ? Color.white.opacity(0.1) : Color.clear)
        .overlay(alignment: .bottom) {
            if isActive {
                Rectangle().fill(Color.accentColor).frame(height: 1)
            }
        }
        .contentShape(Rectangle())
        .onTapGesture { onActivate() }
        .onHover { isHovered = $0 }
    }
}

// MARK: - File Editor (NSTextView wrapper)

struct FileEditorView: NSViewRepresentable {
    let tabId: UUID
    let content: String
    let onContentChange: (String) -> Void
    let onSave: () -> Void

    func makeCoordinator() -> Coordinator {
        Coordinator(onContentChange: onContentChange, onSave: onSave)
    }

    func makeNSView(context: Context) -> NSScrollView {
        let scrollView = NSScrollView()
        scrollView.hasVerticalScroller = true
        scrollView.borderType = .noBorder

        let textView = SaveableTextView()
        textView.isEditable = true
        textView.isSelectable = true
        textView.allowsUndo = true
        textView.isRichText = false
        textView.usesFindBar = true
        textView.isAutomaticQuoteSubstitutionEnabled = false
        textView.isAutomaticDashSubstitutionEnabled = false
        textView.isAutomaticTextReplacementEnabled = false
        textView.isAutomaticSpellingCorrectionEnabled = false
        textView.font = NSFont.monospacedSystemFont(ofSize: 13, weight: .regular)
        textView.textColor = NSColor(white: 0.85, alpha: 1.0)
        textView.backgroundColor = NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)
        textView.insertionPointColor = NSColor.white
        textView.autoresizingMask = [.width]
        textView.isVerticallyResizable = true
        textView.textContainer?.widthTracksTextView = true

        textView.string = content
        textView.delegate = context.coordinator
        textView.onSave = onSave
        context.coordinator.currentTabId = tabId

        scrollView.documentView = textView
        return scrollView
    }

    func updateNSView(_ nsView: NSScrollView, context: Context) {
        guard let textView = nsView.documentView as? SaveableTextView else { return }
        // Only update content when switching tabs (different tabId)
        if context.coordinator.currentTabId != tabId {
            context.coordinator.currentTabId = tabId
            context.coordinator.isUpdating = true
            textView.string = content
            textView.onSave = onSave
            context.coordinator.onContentChange = onContentChange
            context.coordinator.isUpdating = false
        }
    }

    class Coordinator: NSObject, NSTextViewDelegate {
        var onContentChange: (String) -> Void
        var onSave: () -> Void
        var currentTabId: UUID?
        var isUpdating = false

        init(onContentChange: @escaping (String) -> Void, onSave: @escaping () -> Void) {
            self.onContentChange = onContentChange
            self.onSave = onSave
        }

        func textDidChange(_ notification: Notification) {
            guard !isUpdating, let tv = notification.object as? NSTextView else { return }
            onContentChange(tv.string)
        }
    }
}

/// NSTextView subclass that intercepts Cmd+S for save.
class SaveableTextView: NSTextView {
    var onSave: (() -> Void)?

    override func performKeyEquivalent(with event: NSEvent) -> Bool {
        if event.modifierFlags.contains(.command) && event.charactersIgnoringModifiers == "s" {
            onSave?()
            return true
        }
        return super.performKeyEquivalent(with: event)
    }
}

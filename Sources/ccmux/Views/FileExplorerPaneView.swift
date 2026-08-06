import SwiftUI
import AppKit
import WebKit

// MARK: - Main File Explorer Pane

struct FileExplorerPaneView: View {
    let explorerId: UUID
    let config: FileExplorerConfig
    @ObservedObject var controller: SplitTreeController

    var body: some View {
        if let state = controller.fileExplorerState(for: explorerId) {
            FileExplorerContent(state: state, onStateChange: {
                controller.updateFileExplorerConfig(explorerId: explorerId)
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
                rootName: (state.rootPath as NSString).lastPathComponent,
                source: state.source,
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
                        VStack(spacing: 0) {
                            if tab.diskChangedExternally {
                                DiskChangedBanner(
                                    onReload: {
                                        state.reloadFromDisk(tabId: tab.id)
                                        onStateChange()
                                    },
                                    onDismiss: {
                                        state.dismissDiskChange(tabId: tab.id)
                                    }
                                )
                            }
                            if tab.saveErrored {
                                SaveFailedBanner(
                                    onRetry: { state.saveActiveFile() },
                                    onDismiss: { state.dismissSaveError(tabId: tab.id) }
                                )
                            }

                            ZStack(alignment: .topTrailing) {
                                if tab.isPreviewMode {
                                    MarkdownPreviewView(content: tab.content)
                                } else {
                                    FileEditorView(
                                        tabId: tab.id,
                                        content: tab.content,
                                        onContentChange: { newContent in
                                            state.updateContent(tabId: tab.id, newContent: newContent)
                                        },
                                        onSave: {
                                            state.saveActiveFile()
                                            onStateChange()
                                        }
                                    )
                                }

                                if tab.isMarkdown {
                                    Button(action: {
                                        state.togglePreview(tabId: tab.id)
                                        onStateChange()
                                    }) {
                                        Image(systemName: tab.isPreviewMode ? "doc.plaintext" : "eye")
                                            .font(.system(size: 11))
                                            .foregroundColor(.secondary)
                                    }
                                    .buttonStyle(.plain)
                                    .padding(5)
                                    .background(.ultraThinMaterial)
                                    .cornerRadius(4)
                                    .padding(8)
                                    .help(tab.isPreviewMode ? "Show raw text" : "Show preview")
                                }
                            }
                            .frame(maxWidth: .infinity, maxHeight: .infinity)
                        }
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
    let rootName: String
    let source: FileSource
    let onFileSelected: (String) -> Void

    var body: some View {
        List {
            FileTreeNode(
                name: rootName,
                relativePath: "",
                isRoot: true,
                source: source,
                onFileSelected: onFileSelected
            )
        }
        .listStyle(.sidebar)
        .scrollContentBackground(.hidden)
        .background(Color(nsColor: NSColor(red: 0.13, green: 0.14, blue: 0.16, alpha: 1.0)))
    }
}

/// One DIRECTORY row of the tree — files render as plain rows inside their
/// parent's DisclosureGroup. Listing goes through the explorer's `FileSource`,
/// so the tree browses the daemon's repo for hosted workspaces.
private struct FileTreeNode: View {
    let name: String
    let relativePath: String
    let isRoot: Bool
    let source: FileSource
    let onFileSelected: (String) -> Void

    @State private var children: [FileItem]?
    @State private var isExpanded: Bool = false

    struct FileItem: Identifiable {
        let id = UUID()
        let name: String
        let relativePath: String
        let isDirectory: Bool
    }

    var body: some View {
        DisclosureGroup(isExpanded: $isExpanded) {
            if let children {
                ForEach(children) { child in
                    if child.isDirectory {
                        FileTreeNode(
                            name: child.name,
                            relativePath: child.relativePath,
                            isRoot: false,
                            source: source,
                            onFileSelected: onFileSelected
                        )
                    } else {
                        fileRow(child)
                    }
                }
            }
        } label: {
            Label(name, systemImage: isExpanded ? "folder.fill" : "folder")
                .font(.system(size: 11))
        }
        .onChange(of: isExpanded) { _, expanded in
            if expanded && children == nil {
                loadChildren()
            }
        }
        .onAppear {
            // Only flip the flag — onChange does the (async) load, and calling
            // loadChildren here too would double-fetch the root listing.
            if isRoot {
                isExpanded = true
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
        Task {
            let entries = await source.list(path: relativePath) ?? []
            let items = entries
                .filter { !$0.name.hasPrefix(".") }
                .sorted { lhs, rhs in
                    if lhs.isDirectory != rhs.isDirectory { return lhs.isDirectory }
                    return lhs.name.localizedCaseInsensitiveCompare(rhs.name) == .orderedAscending
                }
                .map { entry in
                    FileItem(
                        name: entry.name,
                        relativePath: relativePath.isEmpty ? entry.name : relativePath + "/" + entry.name,
                        isDirectory: entry.isDirectory
                    )
                }
            await MainActor.run { children = items }
        }
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

// MARK: - Disk Changed Banner

private struct DiskChangedBanner: View {
    let onReload: () -> Void
    let onDismiss: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundColor(.orange)
                .font(.system(size: 11))
            Text("File changed on disk — your unsaved edits would be overwritten.")
                .font(.system(size: 11))
                .foregroundColor(.primary)
            Spacer(minLength: 8)
            Button("Reload from disk", action: onReload)
                .font(.system(size: 11))
            Button(action: onDismiss) {
                Image(systemName: "xmark")
                    .font(.system(size: 9))
                    .foregroundColor(.secondary)
            }
            .buttonStyle(.plain)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(Color.orange.opacity(0.12))
        .overlay(alignment: .bottom) {
            Rectangle().fill(Color.orange.opacity(0.4)).frame(height: 1)
        }
    }
}

// MARK: - Save Failed Banner

private struct SaveFailedBanner: View {
    let onRetry: () -> Void
    let onDismiss: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: "exclamationmark.octagon.fill")
                .foregroundColor(.red)
                .font(.system(size: 11))
            Text("Save failed — the file was not written.")
                .font(.system(size: 11))
                .foregroundColor(.primary)
            Spacer(minLength: 8)
            Button("Retry", action: onRetry)
                .font(.system(size: 11))
            Button(action: onDismiss) {
                Image(systemName: "xmark")
                    .font(.system(size: 9))
                    .foregroundColor(.secondary)
            }
            .buttonStyle(.plain)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(Color.red.opacity(0.12))
        .overlay(alignment: .bottom) {
            Rectangle().fill(Color.red.opacity(0.4)).frame(height: 1)
        }
    }
}

// MARK: - Markdown Preview (WKWebView wrapper)

struct MarkdownPreviewView: NSViewRepresentable {
    let content: String

    func makeNSView(context: Context) -> WKWebView {
        let config = WKWebViewConfiguration()
        let webView = WKWebView(frame: .zero, configuration: config)
        webView.setValue(false, forKey: "drawsBackground")
        loadMarkdown(webView: webView)
        return webView
    }

    func updateNSView(_ webView: WKWebView, context: Context) {
        loadMarkdown(webView: webView)
    }

    private func loadMarkdown(webView: WKWebView) {
        let escaped = content
            .replacingOccurrences(of: "\\", with: "\\\\")
            .replacingOccurrences(of: "`", with: "\\`")
            .replacingOccurrences(of: "$", with: "\\$")
        let html = """
        <!DOCTYPE html>
        <html>
        <head>
        <meta charset="utf-8">
        <script src="https://cdn.jsdelivr.net/npm/marked@15/marked.min.js"></script>
        <script src="https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.min.js"></script>
        <style>
          body { font-family: -apple-system, BlinkMacSystemFont, sans-serif;
                 color: #d9d9d9; background: #1c1e24;
                 padding: 20px; font-size: 14px; line-height: 1.6; margin: 0; }
          table { border-collapse: collapse; width: 100%; margin: 12px 0; }
          th, td { border: 1px solid #3a3d45; padding: 8px 12px; text-align: left; }
          th { background: #2a2d35; font-weight: 600; }
          tr:nth-child(even) { background: rgba(255,255,255,0.02); }
          code { background: #2a2d35; padding: 2px 6px; border-radius: 3px;
                 font-family: 'SF Mono', Monaco, monospace; font-size: 13px; }
          pre { background: #2a2d35; padding: 14px; border-radius: 6px;
                overflow-x: auto; margin: 12px 0; }
          pre code { background: none; padding: 0; }
          pre.mermaid { background: #1c1e24; padding: 8px; text-align: center; }
          h1, h2, h3, h4, h5, h6 { color: #e5e5e5; margin-top: 24px; }
          h1, h2 { border-bottom: 1px solid #3a3d45; padding-bottom: 6px; }
          a { color: #58a6ff; text-decoration: none; }
          a:hover { text-decoration: underline; }
          blockquote { border-left: 3px solid #3a3d45; margin: 12px 0;
                       padding: 4px 16px; color: #8b949e; }
          hr { border: none; border-top: 1px solid #3a3d45; margin: 20px 0; }
          img { max-width: 100%; border-radius: 4px; }
          ul, ol { padding-left: 24px; }
          li { margin: 4px 0; }
          input[type="checkbox"] { margin-right: 6px; }
        </style>
        </head>
        <body>
        <div id="content"></div>
        <script>
          const raw = `\(escaped)`;
          // marked v15 calls renderer.code with a single token: { text, lang, escaped }.
          // Emit <pre class="mermaid"> for mermaid fences so mermaid.run() can pick them up.
          marked.use({
            renderer: {
              code({ text, lang }) {
                if (((lang || '') + '').trim().toLowerCase() === 'mermaid') {
                  const escaped = String(text)
                    .replace(/&/g, '&amp;')
                    .replace(/</g, '&lt;')
                    .replace(/>/g, '&gt;');
                  return '<pre class="mermaid">' + escaped + '</pre>';
                }
                return false;
              }
            }
          });
          mermaid.initialize({ startOnLoad: false, theme: 'dark', securityLevel: 'loose' });
          document.getElementById('content').innerHTML = marked.parse(raw);
          mermaid.run({ querySelector: '.mermaid' }).catch(() => {});
        </script>
        </body>
        </html>
        """
        webView.loadHTMLString(html, baseURL: nil)
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

        // Line number gutter
        let ruler = LineNumberRulerView(textView: textView)
        scrollView.verticalRulerView = ruler
        scrollView.hasVerticalRuler = true
        scrollView.rulersVisible = true

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
            nsView.verticalRulerView?.needsDisplay = true
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

// MARK: - Line Number Ruler

/// Draws line numbers in a gutter alongside the text view.
class LineNumberRulerView: NSRulerView {
    private weak var textView: NSTextView?

    private let gutterBackgroundColor = NSColor(red: 0.09, green: 0.10, blue: 0.12, alpha: 1.0)
    private let lineNumberColor = NSColor(white: 0.40, alpha: 1.0)
    private let currentLineColor = NSColor(white: 0.70, alpha: 1.0)

    private lazy var lineNumberFont: NSFont = {
        NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)
    }()

    init(textView: NSTextView) {
        self.textView = textView
        super.init(scrollView: textView.enclosingScrollView!, orientation: .verticalRuler)
        self.clipsToBounds = true  // macOS 14+: prevent drawing outside ruler bounds
        self.ruleThickness = 40
        self.clientView = textView

        NotificationCenter.default.addObserver(
            self, selector: #selector(textDidChange),
            name: NSText.didChangeNotification, object: textView
        )
        NotificationCenter.default.addObserver(
            self, selector: #selector(boundsDidChange),
            name: NSView.boundsDidChangeNotification,
            object: textView.enclosingScrollView?.contentView
        )
    }

    required init(coder: NSCoder) {
        fatalError("init(coder:) not implemented")
    }

    deinit {
        NotificationCenter.default.removeObserver(self)
    }

    @objc private func textDidChange(_ notification: Notification) {
        needsDisplay = true
    }

    @objc private func boundsDidChange(_ notification: Notification) {
        needsDisplay = true
    }

    override func drawHashMarksAndLabels(in rect: NSRect) {
        guard let textView = textView,
              let layoutManager = textView.layoutManager,
              let textContainer = textView.textContainer else { return }

        // Fill background — use bounds, NOT rect (rect can extend beyond ruler on macOS 14+)
        gutterBackgroundColor.setFill()
        bounds.fill()

        // Draw separator line
        NSColor(white: 0.2, alpha: 1.0).setStroke()
        let separatorX = bounds.maxX - 0.5
        NSBezierPath.strokeLine(from: NSPoint(x: separatorX, y: bounds.minY),
                                to: NSPoint(x: separatorX, y: bounds.maxY))

        let string = textView.string as NSString
        let visibleRect = scrollView?.contentView.bounds ?? textView.visibleRect
        let yOffset = textView.textContainerInset.height

        // Ensure layout is complete
        layoutManager.ensureLayout(for: textContainer)

        let visibleGlyphRange = layoutManager.glyphRange(forBoundingRect: visibleRect, in: textContainer)

        // Get the current line (where the insertion point is)
        let selectedRange = textView.selectedRange()
        let currentLineRange = string.length > 0
            ? string.lineRange(for: NSRange(location: min(selectedRange.location, string.length - 1), length: 0))
            : NSRange(location: 0, length: 0)

        // Use enumerateLineFragments for robust line rect computation.
        // First, count lines before the visible range to get the starting line number.
        let visibleCharRange = layoutManager.characterRange(forGlyphRange: visibleGlyphRange, actualGlyphRange: nil)
        var startingLineNumber = 1
        if visibleCharRange.location > 0 {
            let preText = string.substring(to: visibleCharRange.location)
            startingLineNumber = preText.components(separatedBy: "\n").count
            // If the visible range starts right after a newline, we're on the next line
            if preText.hasSuffix("\n") {
                // Already counted correctly
            }
        }

        var lineNumber = startingLineNumber

        // Draw line numbers using enumerateLineFragments — handles empty lines correctly
        layoutManager.enumerateLineFragments(forGlyphRange: visibleGlyphRange) { (fragmentRect, usedRect, container, glyphRange, stop) in
            let charRange = layoutManager.characterRange(forGlyphRange: glyphRange, actualGlyphRange: nil)
            let lineRect = NSRect(
                x: fragmentRect.origin.x,
                y: fragmentRect.origin.y + yOffset,
                width: fragmentRect.width,
                height: fragmentRect.height
            )

            let yPosition = lineRect.origin.y - visibleRect.origin.y

            let isCurrentLine = NSLocationInRange(currentLineRange.location, charRange) ||
                (currentLineRange.location == charRange.location)
            let attrs: [NSAttributedString.Key: Any] = [
                .font: self.lineNumberFont,
                .foregroundColor: isCurrentLine ? self.currentLineColor : self.lineNumberColor
            ]

            let lineStr = "\(lineNumber)" as NSString
            let strSize = lineStr.size(withAttributes: attrs)
            let drawPoint = NSPoint(
                x: self.ruleThickness - strSize.width - 8,
                y: yPosition + (lineRect.height - strSize.height) / 2
            )
            lineStr.draw(at: drawPoint, withAttributes: attrs)

            lineNumber += 1
        }

        // Handle the extra line after a trailing newline
        let extraRect = layoutManager.extraLineFragmentRect
        if extraRect.height > 0 {
            let yPosition = extraRect.origin.y + yOffset - visibleRect.origin.y
            let isCurrentLine = selectedRange.location == string.length
            let attrs: [NSAttributedString.Key: Any] = [
                .font: lineNumberFont,
                .foregroundColor: isCurrentLine ? currentLineColor : lineNumberColor
            ]
            let lineStr = "\(lineNumber)" as NSString
            let strSize = lineStr.size(withAttributes: attrs)
            let drawPoint = NSPoint(
                x: ruleThickness - strSize.width - 8,
                y: yPosition + (extraRect.height - strSize.height) / 2
            )
            lineStr.draw(at: drawPoint, withAttributes: attrs)
        }
    }
}

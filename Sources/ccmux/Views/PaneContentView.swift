import SwiftUI

/// Routes to the correct pane view based on the active tab's PaneContent.
struct PaneContentView: View {
    let paneId: UUID
    let tabs: PaneTabs
    @ObservedObject var controller: SplitTreeController

    var body: some View {
        if let active = tabs.activeTab {
            contentView(for: active)
                .id(active.id)
        } else {
            Color.clear
        }
    }

    @ViewBuilder
    private func contentView(for content: PaneContent) -> some View {
        switch content {
        case .terminal(let config):
            if let hostedPaneId = config.host.hostedPaneId {
                // Hosted pane: attach to the daemon over WebSocket instead of spawning
                // a local process. The local driver path below is untouched.
                HostedTerminalPaneView(paneId: hostedPaneId, workingDirectory: config.workingDirectory)
            } else {
                TerminalPaneView(terminalId: config.id, workingDirectory: config.workingDirectory, startupCommand: config.startupCommand)
            }

        case .browser(let config):
            BrowserPaneView(config: config)

        case .editor(let config):
            EditorPaneView(config: config)

        case .diff(let config):
            DiffPaneView(config: config)

        case .scratchpad(let config):
            ScratchpadPaneView(initialText: config.content, paneId: paneId, tabId: config.id, controller: controller)

        case .fileExplorer(let config):
            FileExplorerPaneView(explorerId: config.id, config: config, controller: controller)
        }
    }
}

/// Simple scratchpad — a text editor for quick notes.
/// Uses @State for responsive local editing, with debounced sync to the controller for persistence.
struct ScratchpadPaneView: View {
    let paneId: UUID
    let tabId: UUID
    @ObservedObject var controller: SplitTreeController
    @State private var text: String

    init(initialText: String, paneId: UUID, tabId: UUID, controller: SplitTreeController) {
        self.paneId = paneId
        self.tabId = tabId
        self.controller = controller
        self._text = State(initialValue: initialText)
    }

    var body: some View {
        VStack(spacing: 0) {
            // Toolbar
            HStack(spacing: 6) {
                Spacer()
                Button {
                    text = cleanClaudeCodePaste(text)
                } label: {
                    HStack(spacing: 3) {
                        Image(systemName: "sparkles")
                            .font(.system(size: 9))
                        Text("Clean text")
                            .font(.system(size: 10))
                    }
                    .padding(.horizontal, 6)
                    .padding(.vertical, 3)
                    .background(Color.white.opacity(0.06))
                    .cornerRadius(4)
                }
                .buttonStyle(.plain)
                .help("Remove 2 leading spaces and trailing whitespace from each line (Claude Code paste cleanup)")
            }
            .padding(.horizontal, 8)
            .padding(.vertical, 4)

            TextEditor(text: $text)
                .font(.system(size: 13, design: .monospaced))
                .scrollContentBackground(.hidden)
                .background(Color(nsColor: NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)))
        }
        .background(Color(nsColor: NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)))
        .task(id: text) {
            try? await Task.sleep(for: .milliseconds(500))
            guard !Task.isCancelled else { return }
            controller.updateScratchpadContent(leafId: paneId, tabId: tabId, newText: text)
        }
        .onDisappear {
            controller.updateScratchpadContent(leafId: paneId, tabId: tabId, newText: text)
        }
    }

    /// Clean up text pasted from Claude Code:
    /// - Remove 2 leading spaces from each line
    /// - Remove trailing whitespace from each line
    private func cleanClaudeCodePaste(_ input: String) -> String {
        input
            .components(separatedBy: "\n")
            .map { line in
                var cleaned = line
                // Remove 2 leading spaces
                if cleaned.hasPrefix("  ") {
                    cleaned = String(cleaned.dropFirst(2))
                }
                // Remove trailing whitespace
                while cleaned.last?.isWhitespace == true {
                    cleaned.removeLast()
                }
                return cleaned
            }
            .joined(separator: "\n")
    }
}

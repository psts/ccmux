import SwiftUI

struct PaneTabBar: View {
    let paneId: UUID
    let content: PaneContent
    let isFocused: Bool
    let isOnlyPane: Bool
    let workingDirectory: String
    let scratchpadContent: String
    let onSplitH: () -> Void
    let onSplitV: () -> Void
    let onClose: () -> Void
    let onFocus: () -> Void
    let onChangeType: (PaneContent) -> Void
    var onMovePane: ((UUID, DropZone) -> Void)?

    @EnvironmentObject var dragState: PaneDragState

    var body: some View {
        HStack(spacing: 0) {
            // Pane type icon and name — click to focus, drag to move
            HStack(spacing: 6) {
                Image(systemName: content.iconName)
                    .font(.system(size: 11))
                Text(content.displayName)
                    .font(.system(size: 11, weight: .medium))
                    .lineLimit(1)
            }
            .padding(.horizontal, 10)
            .frame(maxHeight: .infinity)
            .contentShape(Rectangle())
            .gesture(
                DragGesture(minimumDistance: 5, coordinateSpace: .named("splitTree"))
                    .onChanged { value in
                        if !dragState.isDragging {
                            dragState.beginDrag(paneId: paneId)
                        }
                        dragState.updateLocation(value.location)
                    }
                    .onEnded { _ in
                        if let targetId = dragState.hoveredPaneId,
                           let zone = dragState.dropZone {
                            onMovePane?(targetId, zone)
                        }
                        dragState.endDrag()
                    }
                    .exclusively(before: TapGesture().onEnded { onFocus() })
            )
            .contextMenu {
                paneTypeMenu
            }

            Spacer()

            // Pane type + split + close controls
            HStack(spacing: 2) {
                TabBarButton(icon: "terminal", tooltip: "Terminal") {
                    onChangeType(.defaultTerminal(workingDirectory: workingDirectory))
                }
                TabBarButton(icon: "globe", tooltip: "Browser") {
                    onChangeType(.browser(BrowserConfig(id: UUID(), urlString: "https://google.com")))
                }
                TabBarButton(icon: "note.text", tooltip: "Scratchpad") {
                    onChangeType(.scratchpad(ScratchpadConfig(id: UUID(), title: "Scratchpad", content: scratchpadContent)))
                }
                TabBarButton(icon: "folder.fill", tooltip: "Files") {
                    onChangeType(.defaultFileExplorer(rootPath: workingDirectory))
                }

                TabBarButton(icon: "square.split.2x1", tooltip: "Split Horizontal") {
                    onSplitH()
                }
                TabBarButton(icon: "square.split.1x2", tooltip: "Split Vertical") {
                    onSplitV()
                }

                if !isOnlyPane {
                    TabBarButton(icon: "xmark", tooltip: "Close") {
                        onClose()
                    }
                }
            }
            .padding(.trailing, 6)
        }
        .frame(height: 28)
        .background(isFocused ? Color.white.opacity(0.08) : Color.white.opacity(0.04))
        .overlay(alignment: .bottom) {
            if isFocused {
                Rectangle()
                    .fill(Color.accentColor)
                    .frame(height: 1)
            }
        }
    }

    @ViewBuilder
    private var paneTypeMenu: some View {
        Button("Terminal") { onChangeType(.defaultTerminal(workingDirectory: workingDirectory)) }
        Button("Browser") {
            onChangeType(.browser(BrowserConfig(id: UUID(), urlString: "https://google.com")))
        }
        Button("Scratchpad") {
            onChangeType(.scratchpad(ScratchpadConfig(id: UUID(), title: "Scratchpad", content: scratchpadContent)))
        }
        Button("Files") {
            onChangeType(.defaultFileExplorer(rootPath: workingDirectory))
        }
    }
}

private struct TabBarButton: View {
    let icon: String
    let tooltip: String
    let action: () -> Void

    @State private var isHovered = false

    var body: some View {
        Button(action: action) {
            Image(systemName: icon)
                .font(.system(size: 10))
                .frame(width: 22, height: 22)
                .background(isHovered ? Color.white.opacity(0.1) : Color.clear)
                .cornerRadius(4)
        }
        .buttonStyle(.plain)
        .onHover { isHovered = $0 }
        .help(tooltip)
    }
}

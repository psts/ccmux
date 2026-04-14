import SwiftUI

struct PaneTabBar: View {
    let paneId: UUID
    let tabs: PaneTabs
    let isFocused: Bool
    let isOnlyPane: Bool
    let workingDirectory: String
    let scratchpadContent: String
    let onSplitH: () -> Void
    let onSplitV: () -> Void
    let onClose: () -> Void
    let onFocus: () -> Void
    let onAddTab: (PaneContent) -> Void
    let onActivateTab: (UUID) -> Void
    let onCloseTab: (UUID) -> Void
    var onMovePane: ((UUID, DropZone) -> Void)?

    @EnvironmentObject var dragState: PaneDragState

    var body: some View {
        HStack(spacing: 0) {
            // Scrollable tab strip — each tab is clickable, draggable (to move the whole pane),
            // and has a hover-close button.
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 0) {
                    ForEach(tabs.tabs) { tab in
                        TabChip(
                            tab: tab,
                            isActive: tab.id == tabs.activeTabId,
                            canClose: !(isOnlyPane && tabs.tabs.count == 1),
                            onActivate: { onActivateTab(tab.id) },
                            onClose: { onCloseTab(tab.id) }
                        )
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
                                .exclusively(before: TapGesture().onEnded {
                                    onFocus()
                                    onActivateTab(tab.id)
                                })
                        )
                        .contextMenu { addTabMenu }
                    }
                }
            }
            .frame(maxHeight: .infinity)

            Spacer(minLength: 4)

            // Add-tab buttons + split/close controls
            HStack(spacing: 2) {
                TabBarButton(icon: "terminal", tooltip: "New Terminal Tab") {
                    onAddTab(.defaultTerminal(workingDirectory: workingDirectory))
                }
                TabBarButton(icon: "globe", tooltip: "New Browser Tab") {
                    onAddTab(.browser(BrowserConfig(id: UUID(), urlString: "https://google.com")))
                }
                TabBarButton(icon: "note.text", tooltip: "New Scratchpad Tab") {
                    onAddTab(.scratchpad(ScratchpadConfig(id: UUID(), title: "Scratchpad", content: scratchpadContent)))
                }
                TabBarButton(icon: "folder.fill", tooltip: "New Files Tab") {
                    onAddTab(.defaultFileExplorer(rootPath: workingDirectory))
                }

                Rectangle()
                    .fill(Color.white.opacity(0.08))
                    .frame(width: 1, height: 14)
                    .padding(.horizontal, 3)

                TabBarButton(icon: "square.split.2x1", tooltip: "Split Horizontal") {
                    onSplitH()
                }
                TabBarButton(icon: "square.split.1x2", tooltip: "Split Vertical") {
                    onSplitV()
                }

                if !isOnlyPane {
                    TabBarButton(icon: "xmark", tooltip: "Close Pane") {
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
    private var addTabMenu: some View {
        Button("New Terminal Tab") { onAddTab(.defaultTerminal(workingDirectory: workingDirectory)) }
        Button("New Browser Tab") {
            onAddTab(.browser(BrowserConfig(id: UUID(), urlString: "https://google.com")))
        }
        Button("New Scratchpad Tab") {
            onAddTab(.scratchpad(ScratchpadConfig(id: UUID(), title: "Scratchpad", content: scratchpadContent)))
        }
        Button("New Files Tab") {
            onAddTab(.defaultFileExplorer(rootPath: workingDirectory))
        }
    }
}

private struct TabChip: View {
    let tab: PaneContent
    let isActive: Bool
    let canClose: Bool
    let onActivate: () -> Void
    let onClose: () -> Void

    @State private var isHovered = false

    var body: some View {
        HStack(spacing: 5) {
            Image(systemName: tab.iconName)
                .font(.system(size: 10))
                .foregroundColor(isActive ? .primary : .secondary)
            Text(tab.displayName)
                .font(.system(size: 11, weight: isActive ? .medium : .regular))
                .foregroundColor(isActive ? .primary : .secondary)
                .lineLimit(1)
                .fixedSize(horizontal: true, vertical: false)

            if canClose {
                Button(action: onClose) {
                    Image(systemName: "xmark")
                        .font(.system(size: 8, weight: .bold))
                        .foregroundColor(.secondary)
                        .frame(width: 14, height: 14)
                        .background(
                            Circle()
                                .fill(Color.white.opacity(isHovered ? 0.08 : 0))
                        )
                }
                .buttonStyle(.plain)
                .opacity(isActive || isHovered ? 1 : 0)
            }
        }
        .padding(.horizontal, 10)
        .frame(maxHeight: .infinity)
        .background(
            isActive
                ? Color.white.opacity(0.10)
                : (isHovered ? Color.white.opacity(0.04) : Color.clear)
        )
        .overlay(alignment: .bottom) {
            if isActive {
                Rectangle()
                    .fill(Color.accentColor)
                    .frame(height: 1)
            }
        }
        .contentShape(Rectangle())
        .onHover { isHovered = $0 }
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

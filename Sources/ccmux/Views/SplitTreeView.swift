import SwiftUI

/// Recursively renders a SplitTree<PaneContent> as nested split views.
struct SplitTreeView: View {
    @ObservedObject var controller: SplitTreeController
    @StateObject private var dragState = PaneDragState()

    var body: some View {
        SplitNodeView(node: controller.tree, controller: controller)
            .coordinateSpace(name: "splitTree")
            .environmentObject(dragState)
            .onPreferenceChange(PaneFramePreferenceKey.self) { frames in
                dragState.paneFrames = frames
            }
    }
}

/// Renders a single node of the split tree.
private struct SplitNodeView: View {
    let node: SplitTree<PaneTabs>
    @ObservedObject var controller: SplitTreeController

    var body: some View {
        switch node {
        case .leaf(let id, let tabs):
            LeafPaneView(
                paneId: id,
                tabs: tabs,
                controller: controller
            )

        case .split(let splitId, let direction, let ratio, let first, let second):
            CustomSplitView(
                splitId: splitId,
                direction: direction,
                ratio: ratio,
                controller: controller
            ) {
                SplitNodeView(node: first, controller: controller)
            } second: {
                SplitNodeView(node: second, controller: controller)
            }
        }
    }
}

/// Custom split view using GeometryReader + HStack/VStack with a draggable divider.
private struct CustomSplitView<First: View, Second: View>: View {
    let splitId: UUID
    let direction: SplitDirection
    let ratio: CGFloat
    @ObservedObject var controller: SplitTreeController
    @ViewBuilder let first: First
    @ViewBuilder let second: Second

    var body: some View {
        GeometryReader { geo in
            switch direction {
            case .horizontal:
                let totalWidth = geo.size.width
                let dividerWidth: CGFloat = 4
                let availableWidth = max(totalWidth - dividerWidth, 1)
                let firstWidth = availableWidth * ratio
                let secondWidth = availableWidth * (1 - ratio)

                HStack(spacing: 0) {
                    first
                        .frame(width: firstWidth, height: geo.size.height)

                    SplitDivider(direction: .horizontal)
                        .frame(height: geo.size.height)
                        .gesture(
                            DragGesture()
                                .onChanged { value in
                                    let newRatio = (firstWidth + value.translation.width) / availableWidth
                                    controller.updateRatio(splitId: splitId, newRatio: newRatio)
                                }
                        )

                    second
                        .frame(width: secondWidth, height: geo.size.height)
                }

            case .vertical:
                let totalHeight = geo.size.height
                let dividerHeight: CGFloat = 4
                let availableHeight = max(totalHeight - dividerHeight, 1)
                let firstHeight = availableHeight * ratio
                let secondHeight = availableHeight * (1 - ratio)

                VStack(spacing: 0) {
                    first
                        .frame(width: geo.size.width, height: firstHeight)

                    SplitDivider(direction: .vertical)
                        .frame(width: geo.size.width)
                        .gesture(
                            DragGesture()
                                .onChanged { value in
                                    let newRatio = (firstHeight + value.translation.height) / availableHeight
                                    controller.updateRatio(splitId: splitId, newRatio: newRatio)
                                }
                        )

                    second
                        .frame(width: geo.size.width, height: secondHeight)
                }
            }
        }
    }
}

/// A thin draggable divider between split panes.
private struct SplitDivider: View {
    let direction: SplitDirection

    @State private var isHovered = false

    var body: some View {
        Rectangle()
            .fill(isHovered ? Color.accentColor.opacity(0.6) : Color(white: 0.2))
            .frame(
                width: direction == .horizontal ? 4 : nil,
                height: direction == .vertical ? 4 : nil
            )
            .onHover { isHovered = $0 }
            .cursor(direction == .horizontal ? .resizeLeftRight : .resizeUpDown)
    }
}

private extension View {
    func cursor(_ cursor: NSCursor) -> some View {
        onHover { inside in
            if inside {
                cursor.push()
            } else {
                NSCursor.pop()
            }
        }
    }
}

/// A leaf pane: tab bar + content + drop zone overlay.
private struct LeafPaneView: View {
    let paneId: UUID
    let tabs: PaneTabs
    @ObservedObject var controller: SplitTreeController
    @EnvironmentObject var dragState: PaneDragState

    var body: some View {
        VStack(spacing: 0) {
            PaneTabBar(
                paneId: paneId,
                tabs: tabs,
                isFocused: controller.focusedPaneId == paneId,
                isOnlyPane: controller.tree.leafCount <= 1,
                workingDirectory: controller.workingDirectory,
                scratchpadContent: controller.scratchpadContent,
                onSplitH: { controller.splitPane(id: paneId, direction: .horizontal) },
                onSplitV: { controller.splitPane(id: paneId, direction: .vertical) },
                onClose: { controller.closePane(id: paneId) },
                onFocus: { controller.setFocus(paneId: paneId) },
                // A new tab is made active, so it wants focus for the same reason a
                // clicked one does. In a hosted workspace addTab routes to the daemon
                // and this content is dropped; the request then simply expires.
                onAddTab: { newContent in
                    controller.addTab(leafId: paneId, newContent: newContent)
                    PaneFocusCoordinator.shared.requestFocus(tabId: newContent.id)
                },
                // The click only moves state; ask the clicked tab's terminal to take
                // keyboard focus too, so there's no second click inside it. A tab that
                // isn't a terminal simply leaves the request unclaimed.
                onActivateTab: { tabId in
                    controller.activateTab(leafId: paneId, tabId: tabId)
                    PaneFocusCoordinator.shared.requestFocus(tabId: tabId)
                },
                onCloseTab: { tabId in
                    controller.closeTab(leafId: paneId, tabId: tabId)
                },
                onMovePane: { targetId, zone in
                    controller.movePane(
                        sourceId: paneId,
                        targetId: targetId,
                        direction: zone.splitDirection,
                        insertAsFirst: zone.insertAsFirst
                    )
                },
                claudePaneId: controller.claudePaneId,
                onDesignateClaudePane: { terminalId in
                    controller.setClaudePane(terminalId: terminalId)
                },
                // harnesses stays empty in driver mode (never fed), which hides
                // the picker; addHarnessTab no-ops without a hosted handler.
                harnesses: controller.harnesses,
                onAddHarnessTab: { name in
                    controller.addHarnessTab(leafId: paneId, harness: name)
                }
            )

            Divider()

            PaneContentView(
                paneId: paneId,
                tabs: tabs,
                controller: controller
            )
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .clipped()
                .contentShape(Rectangle())
                .onTapGesture {
                    controller.setFocus(paneId: paneId)
                }
        }
        // Report frame for hit testing
        .background(
            GeometryReader { geo in
                Color.clear.preference(
                    key: PaneFramePreferenceKey.self,
                    value: [paneId: geo.frame(in: .named("splitTree"))]
                )
            }
        )
        // Drop zone overlay
        .overlay {
            if dragState.isDragging,
               dragState.hoveredPaneId == paneId,
               dragState.draggedPaneId != paneId,
               let zone = dragState.dropZone {
                DropZoneOverlay(zone: zone)
            }
        }
        // Dim while being dragged
        .opacity(dragState.draggedPaneId == paneId ? 0.4 : 1.0)
    }
}

/// Visual indicator showing where a dropped pane will land.
private struct DropZoneOverlay: View {
    let zone: DropZone

    var body: some View {
        GeometryReader { geo in
            let isHorizontal = (zone == .left || zone == .right)
            let w = isHorizontal ? geo.size.width / 2 : geo.size.width
            let h = isHorizontal ? geo.size.height : geo.size.height / 2

            Rectangle()
                .fill(Color.accentColor.opacity(0.2))
                .overlay(
                    Rectangle()
                        .stroke(Color.accentColor.opacity(0.5), lineWidth: 2)
                )
                .frame(width: w, height: h)
                .position(
                    x: zone == .right ? geo.size.width * 0.75 :
                       zone == .left ? geo.size.width * 0.25 : geo.size.width / 2,
                    y: zone == .bottom ? geo.size.height * 0.75 :
                       zone == .top ? geo.size.height * 0.25 : geo.size.height / 2
                )
        }
        .allowsHitTesting(false)
    }
}

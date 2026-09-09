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
    /// A tab dropped along its own strip: (tab id, slot). See PaneTabs.moveTab.
    var onMoveTab: ((UUID, Int) -> Void)?
    /// TerminalConfig.id designated as the workspace's Claude pane (for the checkmark).
    var claudePaneId: UUID?
    /// Toggle a terminal tab as the designated Claude pane.
    var onDesignateClaudePane: ((UUID) -> Void)?
    /// The daemon's harness list for the picker; empty (or a nil handler)
    /// hides the button — driver-mode workspaces have no harness spawns.
    var harnesses: [DaemonHarness] = []
    var onAddHarnessTab: ((String) -> Void)?
    /// LLM account names, the global route, and per-DAEMON-pane overrides for
    /// the tab menu's route picker. Empty accounts or a nil setter hides it —
    /// driver mode has no proxy.
    var llmAccounts: [String] = []
    var llmGlobalRoute: String = ""
    var llmPaneRoutes: [String: String] = [:]
    /// Resolved failover order per pane, head first (see SplitTreeController).
    var llmPaneOrders: [String: [String]] = [:]
    /// Panes whose routing could not be resolved, and why.
    var llmPaneOrderErrors: [String: String] = [:]
    /// Called as a pane's route menu opens, to re-read that pane's order:
    /// live account health reorders it with nothing here to notice.
    var onLLMMenuOpen: ((String) -> Void)?
    var onSetPaneLLMRoute: ((String, String) -> Void)?

    @EnvironmentObject var dragState: PaneDragState
    /// Chip and strip frames in the "splitTree" space, for mapping a drag to a slot.
    @State private var tabFrames: [UUID: CGRect] = [:]
    @State private var barFrame: CGRect = .zero

    var body: some View {
        HStack(spacing: 0) {
            // Scrollable tab strip — each tab is clickable, draggable (along the
            // strip to reorder, onto a pane edge to move the whole pane), and has
            // a hover-close button.
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: 0) {
                    ForEach(Array(tabs.tabs.enumerated()), id: \.element.id) { index, tab in
                        TabChip(
                            tab: tab,
                            isActive: tab.id == tabs.activeTabId,
                            canClose: !(isOnlyPane && tabs.tabs.count == 1),
                            onActivate: { onActivateTab(tab.id) },
                            onClose: { onCloseTab(tab.id) }
                        )
                        .background(GeometryReader { geo in
                            Color.clear.preference(
                                key: TabFramePreferenceKey.self,
                                value: [tab.id: geo.frame(in: .named("splitTree"))])
                        })
                        .overlay(alignment: .leading) { dropMarker(before: index) }
                        .overlay(alignment: .trailing) { dropMarker(after: index) }
                        .gesture(tabDrag(for: tab))
                        .contextMenu { tabContextMenu(for: tab) }
                    }
                }
                .onPreferenceChange(TabFramePreferenceKey.self) { tabFrames = $0 }
            }
            .background(GeometryReader { geo in
                Color.clear.preference(
                    key: TabBarFramePreferenceKey.self, value: geo.frame(in: .named("splitTree")))
            })
            .onPreferenceChange(TabBarFramePreferenceKey.self) { barFrame = $0 }
            .frame(maxHeight: .infinity)

            Spacer(minLength: 4)

            // Add-tab buttons + split/close controls
            HStack(spacing: 2) {
                TabBarButton(icon: "terminal", tooltip: "New Terminal Tab") {
                    onAddTab(.defaultTerminal(workingDirectory: workingDirectory))
                }
                if let onAddHarnessTab, !harnesses.isEmpty {
                    Menu {
                        ForEach(harnesses) { h in
                            Button("\(h.icon ?? "▸") \(h.name)") { onAddHarnessTab(h.name) }
                                .help(h.command ?? "")
                        }
                    } label: {
                        Image(systemName: "sparkles")
                            .font(.system(size: 10))
                            .frame(width: 22, height: 22)
                    }
                    .menuStyle(.borderlessButton)
                    .menuIndicator(.hidden)
                    .fixedSize()
                    .help("New harness tab (claude, pi, …)")
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

    // MARK: - Drag

    /// One gesture, two outcomes. While the pointer stays over this strip the
    /// drag is a reorder and lands as a slot; once it leaves, it is the old
    /// move-the-pane drag and lands on another pane's edge zone.
    private func tabDrag(for tab: PaneContent) -> some Gesture {
        DragGesture(minimumDistance: 5, coordinateSpace: .named("splitTree"))
            .onChanged { value in
                if !dragState.isDragging {
                    dragState.beginDrag(paneId: paneId)
                }
                if let slot = slot(at: value.location) {
                    dragState.setTabSlot(slot)
                } else {
                    dragState.updateLocation(value.location)
                }
            }
            .onEnded { _ in
                if let slot = dragState.tabSlot {
                    onMoveTab?(tab.id, slot)
                } else if let targetId = dragState.hoveredPaneId, let zone = dragState.dropZone {
                    onMovePane?(targetId, zone)
                }
                dragState.endDrag()
            }
            .exclusively(before: TapGesture().onEnded {
                onFocus()
                onActivateTab(tab.id)
            })
    }

    /// Where along this strip a drop would land: the number of chips whose
    /// middle lies left of the pointer, the dragged one counted too (the move
    /// removes it first). Nil when the pointer is off the strip. Same maths as
    /// the web lens's dropSlotAt, so a drag reads the same on both.
    private func slot(at point: CGPoint) -> Int? {
        guard barFrame.contains(point) else { return nil }
        return tabs.tabs.filter { tab in
            guard let frame = tabFrames[tab.id] else { return false }
            return frame.midX < point.x
        }.count
    }

    /// The drop marker: a 2pt accent line before the chip the dragged tab would
    /// push right, or after the last chip when it would land at the end.
    @ViewBuilder
    private func dropMarker(before index: Int) -> some View {
        if dragState.draggedPaneId == paneId, dragState.tabSlot == index {
            Rectangle().fill(Color.accentColor).frame(width: 2)
        }
    }

    @ViewBuilder
    private func dropMarker(after index: Int) -> some View {
        if dragState.draggedPaneId == paneId, index == tabs.tabs.count - 1,
           dragState.tabSlot == tabs.tabs.count {
            Rectangle().fill(Color.accentColor).frame(width: 2)
        }
    }

    /// The tab's right-click menu, grouped: everything spawnable lives under
    /// one "New Tab" submenu, and a hosted terminal adds its own concerns —
    /// which model account answers this pane, and the Claude-pane designation.
    @ViewBuilder
    private func tabContextMenu(for tab: PaneContent) -> some View {
        newTabMenu
        if case .terminal(let config) = tab {
            if let hostedId = config.host.hostedPaneId, onSetPaneLLMRoute != nil, !llmAccounts.isEmpty {
                Divider()
                llmRouteMenu(paneId: hostedId)
            }
            Divider()
            Button(tab.id == claudePaneId ? "✓ Claude pane (spawns land here)" : "Use as Claude pane") {
                onDesignateClaudePane?(tab.id)
            }
        }
    }

    private var newTabMenu: some View {
        Menu("New Tab") {
            // Shell-backed tabs first — a plain terminal, then the harnesses
            // that run in one — then the content tabs.
            Button("Terminal") { onAddTab(.defaultTerminal(workingDirectory: workingDirectory)) }
            if let onAddHarnessTab {
                ForEach(harnesses) { h in
                    Button("\(h.icon ?? "▸") \(h.name)") { onAddHarnessTab(h.name) }
                }
            }
            Divider()
            Button("Browser") {
                onAddTab(.browser(BrowserConfig(id: UUID(), urlString: "https://google.com")))
            }
            Button("Scratchpad") {
                onAddTab(.scratchpad(ScratchpadConfig(id: UUID(), title: "Scratchpad", content: scratchpadContent)))
            }
            Button("Files") {
                onAddTab(.defaultFileExplorer(rootPath: workingDirectory))
            }
        }
    }

    /// Which llm account answers this pane FIRST: no override (the pane
    /// follows its harness's account order, or the default account when it
    /// has no harness) or an explicit one. The ✓ marks the current choice,
    /// same style the Claude-pane row uses.
    ///
    /// An override is the HEAD of the pane's order, not the whole of it: the
    /// harness's remaining accounts still follow as failover, which is what
    /// the disabled first rows spell out. The order arrives resolved from the
    /// daemon (llmPaneOrders): it folds the pane's harness rules and live
    /// account health, so it names who is answering NOW, not who was
    /// configured — those differ the moment the first choice hits its limit.
    ///
    /// Refreshed as the menu opens (onLLMMenuOpen), because those two differ
    /// on their own: an account reaching its limit reorders the chain with
    /// nothing here to notice. Until that first answer lands the rows are
    /// simply absent rather than showing a stale claim.
    private func llmRouteMenu(paneId: String) -> some View {
        let current = llmPaneRoutes[paneId] ?? ""
        let fallback = llmGlobalRoute.isEmpty ? "Anthropic direct" : llmGlobalRoute
        let order = llmPaneOrders[paneId] ?? []
        return Menu("Model Account") {
            Color.clear
                .frame(height: 0)
                .onAppear { onLLMMenuOpen?(paneId) }
            if let failure = llmPaneOrderErrors[paneId] {
                // Said, not omitted: an absent order renders the same as a
                // one-account chain, so a pane that will fail its next
                // request looked ordinary.
                Button("unresolved: \(failure)") {}
                    .disabled(true)
                Divider()
            } else if let now = order.first {
                Button("now: \(now)") {}
                    .disabled(true)
                if order.count > 1 {
                    Button("then: " + order.dropFirst().joined(separator: " → ")) {}
                        .disabled(true)
                }
                Divider()
            }
            Button((current.isEmpty ? "✓ " : "") + "Use the harness order (else \(fallback))") {
                onSetPaneLLMRoute?(paneId, "")
            }
            Divider()
            ForEach(llmAccounts, id: \.self) { name in
                Button((current == name ? "✓ " : "") + name) {
                    onSetPaneLLMRoute?(paneId, name)
                }
            }
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
    @ObservedObject private var service = RemoteSessionService.shared

    /// An agent instance's pane carries the gear the web lens's tab strip
    /// shows (⚙), so the two lenses read the same at a glance.
    private var isAgent: Bool {
        guard let id = tab.hostedPaneId else { return false }
        return service.agentPanes[id] != nil
    }

    var body: some View {
        HStack(spacing: 5) {
            // A dormant tab is dimmed rather than badged: it marks an absence,
            // not an alert, and must not compete with attention states.
            Image(systemName: tab.isDormant ? "moon.zzz" : (isAgent ? "gearshape" : tab.iconName))
                .font(.system(size: 10))
                .foregroundColor(isActive ? .primary : .secondary)
                .opacity(tab.isDormant ? 0.55 : 1)
            Text(tab.displayName)
                .font(.system(size: 11, weight: isActive ? .medium : .regular))
                .foregroundColor(isActive ? .primary : .secondary)
                .opacity(tab.isDormant ? 0.55 : 1)
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
        .help(tab.isDormant ? "Claude exited — shell only" : "")
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

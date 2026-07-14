import SwiftUI

struct SidebarView: View {
    @ObservedObject var manager: WorkspaceManager
    @ObservedObject var windowContext: WindowContext
    /// Hosted (ccmuxd-backed) workspaces — the lens pivot's remote sessions.
    @ObservedObject var remoteService: RemoteSessionService
    let onAddWorkspace: () -> Void
    let onDetachWorkspace: (UUID) -> Void
    let onSelectWorkspace: (UUID) -> Void
    /// Selecting a hosted workspace just displays it (no local ownership machinery).
    let onSelectHosted: (UUID) -> Void
    let onReopenWorkspace: (UUID) -> Void
    let onMoveToThisWindow: (UUID) -> Void
    var currentWindowId: UUID?
    var onRenameWindow: ((UUID, String) -> Void)?
    var onRestoreWindow: ((UUID) -> Void)?
    var onNewHostedSession: (() -> Void)?

    /// Workspaces belonging to this window
    private var thisWindowWorkspaces: [Workspace] {
        manager.workspaces
            .filter { windowContext.ownedWorkspaceIds.contains($0.id) }
            .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
    }

    /// Current window's display name
    private var thisWindowName: String {
        windowContext.windowName ?? "This Window"
    }

    var body: some View {
        VStack(spacing: 0) {
            List {
                // Hosted (ccmuxd-backed) sessions — persistent, multi-dev, attach-from-anywhere.
                if !remoteService.workspaces.isEmpty {
                    Section {
                        ForEach(remoteService.workspaces) { workspace in
                            let isDisplayed = workspace.id == windowContext.displayedWorkspaceId
                            HostedWorkspaceRow(
                                workspace: workspace,
                                isActive: isDisplayed,
                                connectionState: remoteService.connectionState(for: workspace.id),
                                onSelect: { onSelectHosted(workspace.id) }
                            )
                            .listRowBackground(
                                AttentionRowBackground(
                                    monitor: remoteService.attentionMonitors[workspace.id] ?? .empty,
                                    isDisplayed: isDisplayed,
                                    onTap: { onSelectHosted(workspace.id) }
                                )
                            )
                            .contextMenu {
                                Button("Remove Session", role: .destructive) {
                                    Task { await remoteService.deleteWorkspace(workspace.id) }
                                }
                            }
                        }
                    } header: {
                        windowSectionHeader(name: "HOSTED", isCurrentWindow: false)
                    }
                }

                // This window's workspaces
                if !thisWindowWorkspaces.isEmpty {
                    Section {
                        ForEach(thisWindowWorkspaces) { workspace in
                            let isDisplayed = workspace.id == windowContext.displayedWorkspaceId
                            WorkspaceRow(
                                workspace: workspace,
                                monitor: manager.monitors[workspace.id] ?? GitStatusMonitor.empty,
                                claudeMonitor: manager.claudeMonitors[workspace.id] ?? ClaudeProcessMonitor.empty,
                                isActive: isDisplayed,
                                isInOtherWindow: false,
                                isExpanded: currentWindowExpansionBinding(for: workspace.id),
                                onSelect: { onSelectWorkspace(workspace.id) },
                                onFileClicked: { filePath in
                                    if let ctrl = manager.controllers[workspace.id] {
                                        if !isDisplayed {
                                            onSelectWorkspace(workspace.id)
                                        }
                                        _ = ctrl.openFileInExplorer(relativePath: filePath)
                                    }
                                }
                            )
                            .listRowBackground(
                                AttentionRowBackground(
                                    monitor: manager.attentionMonitors[workspace.id] ?? .empty,
                                    isDisplayed: isDisplayed,
                                    onTap: { onSelectWorkspace(workspace.id) }
                                )
                            )
                            .contextMenu {
                                workspaceContextMenu(for: workspace)
                            }
                        }
                    } header: {
                        windowSectionHeader(name: thisWindowName.uppercased(), isCurrentWindow: true)
                            .contextMenu {
                                if let windowId = currentWindowId {
                                    Button("Rename Window...") {
                                        onRenameWindow?(windowId, windowContext.windowName ?? "This Window")
                                    }
                                }
                            }
                    }
                }

                // Other windows — each as its own section
                ForEach(windowContext.otherWindowGroups) { group in
                    let groupWorkspaces = manager.workspaces
                        .filter { group.workspaceIds.contains($0.id) }
                        .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
                    if !groupWorkspaces.isEmpty {
                        Section {
                            ForEach(groupWorkspaces) { workspace in
                                OtherWindowWorkspaceRow(
                                    workspace: workspace,
                                    monitor: manager.monitors[workspace.id] ?? GitStatusMonitor.empty,
                                    claudeMonitor: manager.claudeMonitors[workspace.id] ?? ClaudeProcessMonitor.empty,
                                    onSelect: { onSelectWorkspace(workspace.id) },
                                    onFileClicked: { filePath in
                                        if let ctrl = manager.controllers[workspace.id] {
                                            onSelectWorkspace(workspace.id)
                                            _ = ctrl.openFileInExplorer(relativePath: filePath)
                                        }
                                    }
                                )
                                .opacity(0.7)
                                .listRowBackground(
                                    AttentionRowBackground(
                                        monitor: manager.attentionMonitors[workspace.id] ?? .empty,
                                        isDisplayed: false,
                                        onTap: { onSelectWorkspace(workspace.id) }
                                    )
                                )
                                .contextMenu {
                                    workspaceContextMenu(for: workspace)
                                }
                            }
                        } header: {
                            windowSectionHeader(name: group.name.uppercased(), isCurrentWindow: false)
                                .contextMenu {
                                    Button("Rename Window...") {
                                        onRenameWindow?(group.id, group.name)
                                    }
                                }
                        }
                    }
                }
            }
            .listStyle(.sidebar)
            .scrollContentBackground(.hidden)

            Divider()

            HStack {
                Menu {
                    Button {
                        onAddWorkspace()
                    } label: {
                        Label("New from Folder...", systemImage: "folder.badge.plus")
                    }

                    if let onNewHostedSession {
                        Button {
                            onNewHostedSession()
                        } label: {
                            Label("New Hosted Session...", systemImage: "antenna.radiowaves.left.and.right")
                        }
                    }

                    // Restore Window section
                    if !manager.closedWindows.isEmpty {
                        Divider()
                        Text("Restore Window")

                        ForEach(manager.closedWindows) { cw in
                            let wsNames = cw.workspaceIds.compactMap { id in
                                manager.closedWorkspaces.first(where: { $0.id == id })?.name
                            }
                            Button {
                                onRestoreWindow?(cw.id)
                            } label: {
                                Label {
                                    Text(cw.windowName ?? (wsNames.isEmpty ? "Window" : wsNames.joined(separator: ", ")))
                                } icon: {
                                    Image(systemName: "macwindow")
                                }
                            }
                        }
                    }

                    // Restore Workspace section
                    let standaloneWorkspaces = manager.closedWorkspaces.filter { ws in
                        !manager.closedWindows.contains { $0.workspaceIds.contains(ws.id) }
                    }
                    if !standaloneWorkspaces.isEmpty {
                        Divider()
                        Text("Restore Workspace")

                        ForEach(standaloneWorkspaces) { ws in
                            Button {
                                onReopenWorkspace(ws.id)
                            } label: {
                                Label(ws.name, systemImage: "folder")
                            }
                        }
                    }

                    // Clear History submenu
                    let hasAnythingToDelete = !manager.closedWindows.isEmpty || !standaloneWorkspaces.isEmpty
                    if hasAnythingToDelete {
                        Divider()

                        Menu {
                            ForEach(manager.closedWindows) { cw in
                                Button(role: .destructive) {
                                    manager.deleteClosedWindow(id: cw.id)
                                } label: {
                                    Label(cw.displayName, systemImage: "macwindow")
                                }
                            }

                            ForEach(standaloneWorkspaces) { ws in
                                Button(role: .destructive) {
                                    manager.deleteClosedWorkspace(id: ws.id)
                                } label: {
                                    Label(ws.name, systemImage: "folder")
                                }
                            }

                            Divider()

                            Button(role: .destructive) {
                                for cw in manager.closedWindows {
                                    manager.deleteClosedWindow(id: cw.id)
                                }
                                for ws in standaloneWorkspaces {
                                    manager.deleteClosedWorkspace(id: ws.id)
                                }
                            } label: {
                                Label("Clear All", systemImage: "trash")
                            }
                        } label: {
                            Label("Clear History...", systemImage: "clock.arrow.circlepath")
                        }
                    }
                } label: {
                    Label("Open Workspace", systemImage: "plus")
                        .font(.system(size: 11))
                }
                .menuStyle(.borderlessButton)
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
                .frame(maxWidth: 180, alignment: .leading)

                Spacer()
            }
        }
        .background(Color(nsColor: NSColor(red: 0.15, green: 0.16, blue: 0.17, alpha: 1.0)))
    }

    @ViewBuilder
    private func windowSectionHeader(name: String, isCurrentWindow: Bool) -> some View {
        Text(name)
            .font(.system(size: 9, weight: .semibold))
            .foregroundColor(.secondary.opacity(0.6))
            .tracking(1.2)
    }

    private func shortenPath(_ path: String) -> String {
        path.replacingOccurrences(of: NSHomeDirectory(), with: "~")
    }

    /// Binding for current-window rows: reads from the persisted set, writes back
    /// and triggers a debounced save. Default (id absent) = expanded.
    private func currentWindowExpansionBinding(for id: UUID) -> Binding<Bool> {
        Binding(
            get: { !windowContext.collapsedWorkspaceIds.contains(id) },
            set: { newValue in
                if newValue {
                    windowContext.collapsedWorkspaceIds.remove(id)
                } else {
                    windowContext.collapsedWorkspaceIds.insert(id)
                }
                manager.scheduleSaveFromWindow()
            }
        )
    }

    @ViewBuilder
    private func workspaceContextMenu(for workspace: Workspace) -> some View {
        let isInOtherWindow = windowContext.otherWindowWorkspaceIds.contains(workspace.id)

        if isInOtherWindow {
            Button("Move to This Window") {
                onMoveToThisWindow(workspace.id)
            }
            Divider()
        }
        Button("Open in New Window") {
            onDetachWorkspace(workspace.id)
        }
        Divider()
        Button("Reveal in Finder") {
            NSWorkspace.shared.selectFile(nil, inFileViewerRootedAtPath: workspace.repoPath)
        }
        Divider()
        Button("Remove Workspace", role: .destructive) {
            manager.removeWorkspace(id: workspace.id)
        }
    }
}

// MARK: - Attention Row Background

/// Row background for a sidebar workspace. Layers the existing selection highlight
/// with an attention signal: a pulsing orange tint + left accent bar when Claude
/// `needsInput`, a steady soft-green tint when a turn is `done`. Idle rows render
/// exactly as before. Observes the per-workspace `ClaudeAttentionMonitor` so it
/// repaints when the state changes.
private struct AttentionRowBackground: View {
    @ObservedObject var monitor: ClaudeAttentionMonitor
    let isDisplayed: Bool
    let onTap: () -> Void

    /// Drives the pulse oscillation; flipped once when entering `needsInput` so the
    /// repeating animation interpolates the tint/accent opacity back and forth.
    @State private var pulse = false

    var body: some View {
        ZStack(alignment: .leading) {
            isDisplayed ? Color.white.opacity(0.15) : Color.clear
            tintColor
            if monitor.state != .none {
                Rectangle()
                    .fill(accentColor)
                    .frame(width: 3)
            }
        }
        .contentShape(Rectangle())
        .onTapGesture(perform: onTap)
        .onAppear { syncPulse() }
        .onChange(of: monitor.state) { _, _ in syncPulse() }
        .animation(pulseAnimation, value: pulse)
    }

    private func syncPulse() {
        pulse = (monitor.state == .needsInput)
    }

    private var pulseAnimation: Animation {
        monitor.state == .needsInput
            ? .easeInOut(duration: 0.7).repeatForever(autoreverses: true)
            : .easeOut(duration: 0.25)
    }

    private var tintColor: Color {
        switch monitor.state {
        case .needsInput: return Color.orange.opacity(pulse ? 0.30 : 0.05)
        case .done: return Color.green.opacity(0.14)
        case .none: return Color.clear
        }
    }

    private var accentColor: Color {
        switch monitor.state {
        case .needsInput: return Color.orange.opacity(pulse ? 0.95 : 0.45)
        case .done: return Color.green.opacity(0.85)
        case .none: return Color.clear
        }
    }
}

// MARK: - Hosted Workspace Row

/// Sidebar row for a ccmuxd-backed (hosted) workspace. Leaner than the local
/// `WorkspaceRow` — no local git dashboard (v1 hosted scope is terminal-only) —
/// plus a connection dot reflecting the attach WebSocket's health.
private struct HostedWorkspaceRow: View {
    let workspace: Workspace
    let isActive: Bool
    let connectionState: DaemonConnectionState
    let onSelect: () -> Void

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: "antenna.radiowaves.left.and.right")
                .font(.system(size: 10))
                .foregroundColor(.secondary)
            Text(workspace.name)
                .font(.system(size: 12, weight: isActive ? .semibold : .regular))
                .foregroundColor(.primary)
                .lineLimit(1)
                .truncationMode(.middle)
            Spacer()
            Circle()
                .fill(connectionColor)
                .frame(width: 6, height: 6)
                .help(connectionHelp)
        }
        .contentShape(Rectangle())
        .onTapGesture(perform: onSelect)
    }

    private var connectionColor: Color {
        switch connectionState {
        case .connected: return .green
        case .connecting, .reconnecting: return .yellow
        case .closed: return .red
        }
    }

    private var connectionHelp: String {
        switch connectionState {
        case .connected: return "Attached"
        case .connecting: return "Connecting…"
        case .reconnecting: return "Reconnecting…"
        case .closed: return "Disconnected"
        }
    }
}

// MARK: - Workspace Row

private struct WorkspaceRow: View {
    let workspace: Workspace
    @ObservedObject var monitor: GitStatusMonitor
    @ObservedObject var claudeMonitor: ClaudeProcessMonitor
    let isActive: Bool
    let isInOtherWindow: Bool
    @Binding var isExpanded: Bool
    var onSelect: (() -> Void)?
    var onFileClicked: ((String) -> Void)?

    private var status: GitStatusInfo {
        monitor.status
    }

    var body: some View {
        DisclosureGroup(isExpanded: $isExpanded) {
            VStack(alignment: .leading, spacing: 3) {
                // Repo path
                HStack(spacing: 4) {
                    Image(systemName: "folder")
                        .font(.system(size: 10))
                        .foregroundColor(.secondary)
                    Text(abbreviatePath(workspace.repoPath))
                        .font(.system(size: 10))
                        .foregroundColor(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                .padding(.leading, 4)

                // Git dashboard
                if status.isGitRepo {
                    GitDashboardContent(status: status, onFileClicked: onFileClicked)
                } else {
                    HStack(spacing: 4) {
                        Image(systemName: "xmark.circle")
                            .font(.system(size: 9))
                            .foregroundColor(.secondary.opacity(0.5))
                        Text("Not a git repository")
                            .font(.system(size: 10))
                            .foregroundColor(.secondary.opacity(0.5))
                    }
                    .padding(.leading, 4)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(Rectangle())
            .onTapGesture { onSelect?() }
        } label: {
            // Workspace name + git status badges
            HStack(spacing: 5) {
                Text(workspace.name)
                    .font(.system(size: 12, weight: isActive ? .semibold : .regular))
                    .foregroundColor(.primary)

                if claudeMonitor.isRunning {
                    Image(systemName: "bolt.fill")
                        .font(.system(size: 9))
                        .foregroundColor(.orange)
                        .help("Claude is running")
                }

                Spacer()

                if status.isGitRepo {
                    // Branch name
                    Text(status.branch)
                        .font(.system(size: 9, design: .monospaced))
                        .foregroundColor(.secondary)
                        .lineLimit(1)

                    // Dirty count
                    if status.totalChanges > 0 {
                        HStack(spacing: 1) {
                            Circle()
                                .fill(Color.orange)
                                .frame(width: 5, height: 5)
                            Text("\(status.totalChanges)")
                                .font(.system(size: 9, weight: .bold))
                                .foregroundColor(.orange)
                        }
                    }

                    // Ahead
                    if status.ahead > 0 {
                        Text("↑\(status.ahead)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.orange)
                    }

                    // Behind
                    if status.behind > 0 {
                        Text("↓\(status.behind)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.orange)
                    }
                }

                // Badge: in another window
                if isInOtherWindow {
                    Image(systemName: "macwindow")
                        .font(.system(size: 9))
                        .foregroundColor(.secondary)
                        .help("Open in another window")
                }
            }
        }
    }

    private func abbreviatePath(_ path: String) -> String {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        if path.hasPrefix(home) {
            return "~" + path.dropFirst(home.count)
        }
        return path
    }
}

// MARK: - Other-window row (session-only peek expansion)

/// Wraps `WorkspaceRow` for workspaces in other windows. Holds session-only
/// expansion state initialized to collapsed; the user can click to expand
/// temporarily but the state is never persisted and resets on app restart.
private struct OtherWindowWorkspaceRow: View {
    let workspace: Workspace
    @ObservedObject var monitor: GitStatusMonitor
    @ObservedObject var claudeMonitor: ClaudeProcessMonitor
    var onSelect: (() -> Void)?
    var onFileClicked: ((String) -> Void)?

    @State private var isExpanded = false

    var body: some View {
        WorkspaceRow(
            workspace: workspace,
            monitor: monitor,
            claudeMonitor: claudeMonitor,
            isActive: false,
            isInOtherWindow: true,
            isExpanded: $isExpanded,
            onSelect: onSelect,
            onFileClicked: onFileClicked
        )
    }
}

// MARK: - Git Dashboard Content (inside disclosure)

private struct GitDashboardContent: View {
    let status: GitStatusInfo
    var onFileClicked: ((String) -> Void)?

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            // Branch tracking line (upstream)
            if let tracking = status.trackingBranch {
                HStack(spacing: 4) {
                    Image(systemName: "arrow.triangle.branch")
                        .font(.system(size: 9))
                        .foregroundColor(.secondary)
                    Text("\(status.branch) → \(tracking)")
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundColor(.secondary)
                        .lineLimit(1)
                    Spacer()
                    if status.ahead > 0 {
                        Text("↑\(status.ahead)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.orange)
                    }
                    if status.behind > 0 {
                        Text("↓\(status.behind)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.orange)
                    }
                }
                .padding(.leading, 4)
            }

            // Default branch comparison (e.g., dev vs main)
            if let defaultBranch = status.defaultBranch, !status.isOnDefaultBranch {
                HStack(spacing: 4) {
                    Image(systemName: "arrow.left.arrow.right")
                        .font(.system(size: 8))
                        .foregroundColor(.secondary.opacity(0.7))
                    Text("vs \(defaultBranch)")
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundColor(.secondary.opacity(0.7))
                    Spacer()
                    if status.aheadOfDefault > 0 {
                        Text("↑\(status.aheadOfDefault)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.orange.opacity(0.7))
                    }
                    if status.behindDefault > 0 {
                        Text("↓\(status.behindDefault)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.orange.opacity(0.8))
                            .help("\(status.behindDefault) commits behind \(defaultBranch)")
                    }
                }
                .padding(.leading, 4)
            }

            if status.isClean {
                // Clean state
                HStack(spacing: 4) {
                    Image(systemName: "checkmark.circle.fill")
                        .font(.system(size: 9))
                        .foregroundColor(.green)
                    Text("Clean — no changes")
                        .font(.system(size: 10))
                        .foregroundColor(.green.opacity(0.8))
                }
                .padding(.leading, 4)
            } else {
                // File sections
                if !status.stagedFiles.isEmpty {
                    fileSectionView(
                        title: "Staged",
                        files: status.stagedFiles,
                        color: .green
                    )
                }
                if !status.modifiedFiles.isEmpty {
                    fileSectionView(
                        title: "Modified",
                        files: status.modifiedFiles,
                        color: .orange
                    )
                }
                if !status.deletedFiles.isEmpty {
                    fileSectionView(
                        title: "Deleted",
                        files: status.deletedFiles,
                        color: .red
                    )
                }
                if !status.untrackedFiles.isEmpty {
                    fileSectionView(
                        title: "Untracked",
                        files: status.untrackedFiles,
                        color: .secondary
                    )
                }
            }
        }
    }

    @ViewBuilder
    private func fileSectionView(title: String, files: [GitStatusInfo.FileChange], color: Color) -> some View {
        // Section header
        HStack(spacing: 4) {
            Rectangle()
                .fill(color.opacity(0.3))
                .frame(height: 1)
                .frame(maxWidth: 8)
            Text("\(title) (\(files.count))")
                .font(.system(size: 9, weight: .medium))
                .foregroundColor(color)
            Rectangle()
                .fill(color.opacity(0.3))
                .frame(height: 1)
        }
        .padding(.leading, 4)
        .padding(.top, 2)

        // File list
        ForEach(files, id: \.path) { file in
            Text(file.filename)
                .font(.system(size: 10))
                .foregroundColor(.primary.opacity(0.8))
                .lineLimit(1)
                .truncationMode(.middle)
                .padding(.leading, 12)
                .contentShape(Rectangle())
                .onTapGesture {
                    onFileClicked?(file.path)
                }
                .onHover { hovering in
                    if hovering {
                        NSCursor.pointingHand.push()
                    } else {
                        NSCursor.pop()
                    }
                }
        }
    }
}

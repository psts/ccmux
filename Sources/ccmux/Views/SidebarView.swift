import SwiftUI

struct SidebarView: View {
    @ObservedObject var manager: WorkspaceManager
    @ObservedObject var windowContext: WindowContext
    let onAddWorkspace: () -> Void
    let onDetachWorkspace: (UUID) -> Void
    let onSelectWorkspace: (UUID) -> Void
    let onReopenWorkspace: (UUID) -> Void
    let onMoveToThisWindow: (UUID) -> Void

    /// Workspaces belonging to this window (not displayed in other windows)
    private var thisWindowWorkspaces: [Workspace] {
        manager.workspaces.filter { !windowContext.otherWindowWorkspaceIds.contains($0.id) }
    }

    /// Workspaces displayed in other windows
    private var otherWindowWorkspaces: [Workspace] {
        manager.workspaces.filter { windowContext.otherWindowWorkspaceIds.contains($0.id) }
    }

    var body: some View {
        VStack(spacing: 0) {
            List {
                // Workspaces in this window
                if !thisWindowWorkspaces.isEmpty {
                    Section {
                        ForEach(thisWindowWorkspaces) { workspace in
                            let isDisplayed = workspace.id == windowContext.displayedWorkspaceId
                            WorkspaceRow(
                                workspace: workspace,
                                monitor: manager.monitors[workspace.id] ?? GitStatusMonitor.empty,
                                isActive: isDisplayed,
                                isInOtherWindow: false,
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
                                (isDisplayed ? Color.white.opacity(0.15) : Color.clear)
                                    .contentShape(Rectangle())
                                    .onTapGesture { onSelectWorkspace(workspace.id) }
                            )
                            .contextMenu {
                                workspaceContextMenu(for: workspace)
                            }
                        }
                    } header: {
                        Text("THIS WINDOW")
                            .font(.system(size: 9, weight: .semibold))
                            .foregroundColor(.secondary.opacity(0.6))
                            .tracking(1.2)
                    }
                }

                // Workspaces in other windows
                if !otherWindowWorkspaces.isEmpty {
                    Section {
                        ForEach(otherWindowWorkspaces) { workspace in
                            WorkspaceRow(
                                workspace: workspace,
                                monitor: manager.monitors[workspace.id] ?? GitStatusMonitor.empty,
                                isActive: false,
                                isInOtherWindow: true,
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
                                Color.clear
                                    .contentShape(Rectangle())
                                    .onTapGesture { onSelectWorkspace(workspace.id) }
                            )
                            .contextMenu {
                                workspaceContextMenu(for: workspace)
                            }
                        }
                    } header: {
                        Text("OTHER WINDOWS")
                            .font(.system(size: 9, weight: .semibold))
                            .foregroundColor(.secondary.opacity(0.6))
                            .tracking(1.2)
                    }
                }
            }
            .listStyle(.sidebar)
            .scrollContentBackground(.hidden)

            Divider()

            HStack {
                Menu {
                    Button("New from Folder...") {
                        onAddWorkspace()
                    }

                    if !manager.closedWorkspaces.isEmpty {
                        Divider()

                        ForEach(manager.closedWorkspaces) { ws in
                            Button {
                                onReopenWorkspace(ws.id)
                            } label: {
                                HStack {
                                    VStack(alignment: .leading) {
                                        Text(ws.name)
                                        Text(shortenPath(ws.repoPath))
                                            .font(.caption)
                                            .foregroundColor(.secondary)
                                    }
                                    Spacer()
                                }
                            }
                        }

                        Divider()

                        ForEach(manager.closedWorkspaces) { ws in
                            Button(role: .destructive) {
                                manager.deleteClosedWorkspace(id: ws.id)
                            } label: {
                                Label("Delete \(ws.name)", systemImage: "trash")
                            }
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

    private func shortenPath(_ path: String) -> String {
        path.replacingOccurrences(of: NSHomeDirectory(), with: "~")
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

// MARK: - Workspace Row

private struct WorkspaceRow: View {
    let workspace: Workspace
    @ObservedObject var monitor: GitStatusMonitor
    let isActive: Bool
    let isInOtherWindow: Bool
    var onSelect: (() -> Void)?
    var onFileClicked: ((String) -> Void)?

    @State private var isExpanded = true

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
                            .foregroundColor(.green)
                    }

                    // Behind
                    if status.behind > 0 {
                        Text("↓\(status.behind)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(Color(nsColor: .systemBlue))
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
                            .foregroundColor(.green)
                    }
                    if status.behind > 0 {
                        Text("↓\(status.behind)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(Color(nsColor: .systemBlue))
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
                            .foregroundColor(.green.opacity(0.7))
                    }
                    if status.behindDefault > 0 {
                        Text("↓\(status.behindDefault)")
                            .font(.system(size: 9, weight: .medium))
                            .foregroundColor(.red.opacity(0.8))
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

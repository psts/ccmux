import SwiftUI
import UniformTypeIdentifiers

struct SidebarView: View {
    @ObservedObject var manager: WorkspaceManager
    @ObservedObject var windowContext: WindowContext
    /// Hosted (ccmuxd-backed) workspaces — the lens pivot's remote sessions.
    @ObservedObject var remoteService: RemoteSessionService
    let onAddWorkspace: () -> Void
    let onDetachWorkspace: (UUID) -> Void
    let onSelectWorkspace: (UUID) -> Void
    let onReopenWorkspace: (UUID) -> Void
    let onMoveToThisWindow: (UUID) -> Void
    var currentWindowId: UUID?
    var onRenameWindow: ((UUID, String) -> Void)?
    var onRestoreWindow: ((UUID) -> Void)?
    var onNewHostedSession: (() -> Void)?
    var onWorkspaceHostnames: ((UUID) -> Void)?
    var onNewWindow: (() -> Void)?
    /// Drag-and-drop move: (workspaceId, targetWindowId). Any window section —
    /// this window's or another's — accepts a dropped workspace row.
    var onMoveToWindow: ((UUID, UUID) -> Void)?

    /// Workspaces belonging to this window — local and hosted alike; a hosted
    /// workspace lives in whatever window group the user put it in, marked only
    /// by its beacon icon.
    private var thisWindowWorkspaces: [Workspace] {
        workspaces(ownedBy: windowContext.ownedWorkspaceIds)
    }

    /// Local + hosted workspaces owned by the given window, sorted by name.
    private func workspaces(ownedBy ids: some Collection<UUID>) -> [Workspace] {
        (manager.workspaces + remoteService.workspaces)
            .filter { ids.contains($0.id) }
            .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
    }

    /// Current window's display name
    /// Cold hosted sessions whose shared group names this window — rendered
    /// inside its section so a session that merely went cold doesn't visually
    /// leave the window it belongs to.
    private func coldWorkspaces(inGroup name: String) -> [DaemonWorkspace] {
        remoteService.coldWorkspaces.filter { $0.group == name }
    }

    /// Cold sessions with no matching window section ("" or a window that was
    /// closed) — the only ones the global COLD SESSIONS bucket shows.
    private var ungroupedColdWorkspaces: [DaemonWorkspace] {
        var names = Set(windowContext.otherWindowGroups.map(\.name))
        names.insert(thisWindowName)
        return remoteService.coldWorkspaces.filter { !names.contains($0.group) }
    }

    private var thisWindowName: String {
        windowContext.windowName ?? "This Window"
    }

    var body: some View {
        VStack(spacing: 0) {
            // Window-level actions live up here; the bottom menu adds CONTENT
            // (workspaces, hosted sessions) to the current window.
            HStack {
                Spacer()
                Button {
                    onNewWindow?()
                } label: {
                    Image(systemName: "macwindow.badge.plus")
                        .font(.system(size: 12))
                        .foregroundColor(.secondary)
                }
                .buttonStyle(.borderless)
                .help("New Window")
            }
            .padding(.horizontal, 12)
            .padding(.top, 6)

            List {
                // This window's workspaces. The section renders even when
                // EMPTY: a freshly created window must offer a drop target
                // for its first workspace — hidden sections would leave no
                // droppable pixel for it anywhere in the app.
                Section {
                    ForEach(thisWindowWorkspaces) { workspace in
                        Group {
                            if workspace.mode == .hosted {
                                hostedRow(workspace, dimmed: false)
                            } else {
                                localRow(workspace)
                            }
                        }
                        // Drag starts from the row's CONTENT (text/icons)
                        // only. Full-row dragging was tried and disproven
                        // live: contentShape(Rectangle()), a full-width
                        // frame, and a 0.001-opacity background all failed
                        // to extend the drag area — the AppKit-backed List
                        // routes empty-area mouse-downs to the row view,
                        // never to this content. Same limitation as
                        // Finder's sidebar; don't re-attempt those hacks.
                        .onDrag { dragProvider(for: workspace) }
                        .modifier(dropTarget(currentWindowId))
                    }
                    ForEach(coldWorkspaces(inGroup: thisWindowName), id: \.id) { cold in
                        coldRow(cold)
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
                        .modifier(dropTarget(currentWindowId))
                }

                // Other windows — each as its own section, EMPTY ones
                // included so every window is a visible drop target from
                // every sidebar.
                ForEach(windowContext.otherWindowGroups) { group in
                    Section {
                        ForEach(workspaces(ownedBy: group.workspaceIds)) { workspace in
                            Group {
                                if workspace.mode == .hosted {
                                    hostedRow(workspace, dimmed: true)
                                } else {
                                    otherWindowLocalRow(workspace)
                                }
                            }
                            .onDrag { dragProvider(for: workspace) }
                            .modifier(dropTarget(group.id))
                        }
                        ForEach(coldWorkspaces(inGroup: group.name), id: \.id) { cold in
                            coldRow(cold)
                        }
                    } header: {
                        windowSectionHeader(name: group.name.uppercased(), isCurrentWindow: false)
                            .contextMenu {
                                Button("Rename Window...") {
                                    onRenameWindow?(group.id, group.name)
                                }
                            }
                            .modifier(dropTarget(group.id))
                    }
                }

                // Cold hosted sessions (archived, or the host restarted): the
                // daemon keeps their full recipe — click revives in place.
                // Cold rows whose shared group matches a window render INSIDE
                // that window's section above (revive re-adopts them there via
                // reconcileHostedOwnership's group match); this global bucket
                // only catches ones with no matching window.
                if !ungroupedColdWorkspaces.isEmpty {
                    Section {
                        ForEach(ungroupedColdWorkspaces, id: \.id) { cold in
                            coldRow(cold)
                        }
                    } header: {
                        windowSectionHeader(name: "COLD SESSIONS", isCurrentWindow: false)
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

    /// Drag source for a workspace row. NSItemProvider (`onDrag`) rather than
    /// `.draggable`/Transferable: in an AppKit-backed List the Transferable
    /// path can leave the table's drag session stuck after a drop or cancel,
    /// after which NO drag starts in that window until its rows churn — seen
    /// live as "dragging dies for a minute until I switch windows around".
    /// The NSLog lines make the next report diagnosable from the log.
    private func dragProvider(for workspace: Workspace) -> NSItemProvider {
        NSLog("[ccmux drag] begin \(workspace.name)")
        return NSItemProvider(object: workspace.id.uuidString as NSString)
    }

    /// One drop rule for every window section (rows and headers alike): a
    /// plain-text payload means "move that workspace to this section's window".
    private struct WorkspaceDropTarget: ViewModifier {
        let handle: ([NSItemProvider]) -> Bool
        func body(content: Content) -> some View {
            content.onDrop(of: [.plainText], isTargeted: nil, perform: handle)
        }
    }

    private func dropTarget(_ windowId: UUID?) -> some ViewModifier {
        WorkspaceDropTarget { dropProviders($0, into: windowId) }
    }

    /// Shared drop handler: the payload is ONE workspace UUID string dragged
    /// from a sidebar row. Same-window drops no-op downstream (the manager
    /// skips targets that already own the workspace). A payload that fails to
    /// load or isn't a workspace UUID logs — the drop already animated as
    /// accepted by then, and a nothing-happened drop with no log line is the
    /// exact failure shape the [ccmux drag] breadcrumbs exist to explain.
    private func dropProviders(_ providers: [NSItemProvider], into windowId: UUID?) -> Bool {
        guard let windowId,
              let provider = providers.first(where: { $0.canLoadObject(ofClass: NSString.self) })
        else { return false }
        _ = provider.loadObject(ofClass: NSString.self) { object, error in
            guard let s = object as? String, let wsId = UUID(uuidString: s) else {
                NSLog("[ccmux drag] drop rejected: \(error.map(String.init(describing:)) ?? "payload is not a workspace UUID")")
                return
            }
            DispatchQueue.main.async {
                NSLog("[ccmux drag] drop \(s) -> window \(windowId)")
                onMoveToWindow?(wsId, windowId)
            }
        }
        return true
    }

    // MARK: - Row builders

    /// A hosted (daemon-backed) workspace row in whatever window group owns it.
    /// Renders through the SAME `WorkspaceRow` dashboard as local rows (branch,
    /// ahead/behind, changed files — daemon-computed, remote-fed monitors); only
    /// the beacon icon + connection dot mark it as hosted. `dimmed` matches
    /// other-window styling. File clicks are inert: the files live on the
    /// daemon's host and hosted panes are terminal-only.
    @ViewBuilder
    private func hostedRow(_ workspace: Workspace, dimmed: Bool) -> some View {
        let isDisplayed = workspace.id == windowContext.displayedWorkspaceId
        Group {
            if dimmed {
                OtherWindowWorkspaceRow(
                    workspace: workspace,
                    monitor: remoteService.gitMonitors[workspace.id] ?? GitStatusMonitor.empty,
                    claudeMonitor: remoteService.claudeMonitors[workspace.id] ?? ClaudeProcessMonitor.empty,
                    onSelect: { onSelectWorkspace(workspace.id) },
                    hostedConnection: remoteService.connectionState(for: workspace.id)
                )
                .opacity(0.7)
            } else {
                WorkspaceRow(
                    workspace: workspace,
                    monitor: remoteService.gitMonitors[workspace.id] ?? GitStatusMonitor.empty,
                    claudeMonitor: remoteService.claudeMonitors[workspace.id] ?? ClaudeProcessMonitor.empty,
                    isActive: isDisplayed,
                    isInOtherWindow: false,
                    isExpanded: currentWindowExpansionBinding(for: workspace.id),
                    onSelect: { onSelectWorkspace(workspace.id) },
                    hostedConnection: remoteService.connectionState(for: workspace.id),
                    hostnames: remoteService.hostnames[workspace.id] ?? [],
                    devRunning: remoteService.devRunning[workspace.id] ?? false,
                    onToggleDevServer: { toggleDevServer(workspace.id) }
                )
            }
        }
        .listRowBackground(
            AttentionRowBackground(
                monitor: remoteService.attentionMonitors[workspace.id] ?? .empty,
                isDisplayed: isDisplayed && !dimmed,
                onTap: { onSelectWorkspace(workspace.id) }
            )
        )
        .contextMenu {
            hostedContextMenu(for: workspace)
        }
    }

    @ViewBuilder
    private func localRow(_ workspace: Workspace) -> some View {
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

    @ViewBuilder
    private func otherWindowLocalRow(_ workspace: Workspace) -> some View {
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

    /// Context menu for hosted rows: same window plumbing as local ones, but the
    /// destructive action removes the session on the daemon (no local file
    /// affordances — the folder lives on the daemon's host).
    @ViewBuilder
    private func hostedContextMenu(for workspace: Workspace) -> some View {
        // Federation: the only place a workspace's host surfaces — a disabled line.
        if !remoteService.hostLabel(for: workspace.id).isEmpty {
            Button("⬡ \(remoteService.hostLabel(for: workspace.id))") {}
                .disabled(true)
            Divider()
        }
        if windowContext.otherWindowWorkspaceIds.contains(workspace.id) {
            Button("Move to This Window") {
                onMoveToThisWindow(workspace.id)
            }
            Divider()
        }
        Button("Open in New Window") {
            onDetachWorkspace(workspace.id)
        }
        Divider()
        Button("Hostnames…") {
            onWorkspaceHostnames?(workspace.id)
        }
        if !(remoteService.hostnames[workspace.id] ?? []).isEmpty {
            Button(remoteService.devRunning[workspace.id] == true ? "Stop Dev Server" : "Start Dev Server") {
                toggleDevServer(workspace.id)
            }
        }
        ForEach(remoteService.hostnames[workspace.id] ?? []) { hostname in
            hostnameMenu(hostname)
        }
        Divider()
        Button("Close Session") {
            Task { await remoteService.archiveWorkspace(workspace.id) }
        }
        Button("Remove Session…", role: .destructive) {
            confirmRemoveSession(name: workspace.name) {
                Task { await remoteService.deleteWorkspace(workspace.id) }
            }
        }
    }

    /// A cold hosted session: dimmed, with a moon icon; click (or the menu's
    /// Revive) recreates the tmux session from the daemon's stored recipe.
    private func coldRow(_ cold: DaemonWorkspace) -> some View {
        HStack(spacing: 6) {
            Image(systemName: "moon.zzz")
                .font(.system(size: 11))
            Text(cold.name)
                .lineLimit(1)
            Spacer()
        }
        .foregroundColor(.secondary)
        .contentShape(Rectangle())
        .onTapGesture {
            revive(cold)
        }
        .contextMenu {
            Button("Revive") {
                revive(cold)
            }
            Divider()
            Button("Remove Session…", role: .destructive) {
                confirmRemoveSession(name: cold.name) {
                    Task { await remoteService.deleteWorkspace(daemonId: cold.id) }
                }
            }
        }
    }

    /// Resurrect a cold session, reporting a refusal instead of swallowing it.
    /// The click has no other visible effect when it fails, so silence reads as a
    /// dead row.
    private func revive(_ cold: DaemonWorkspace) {
        Task {
            guard let error = await remoteService.reviveWorkspace(daemonId: cold.id) else { return }
            await MainActor.run { reportFailure(action: "Revive “\(cold.name)”", error: error) }
        }
    }

    /// Surface a daemon refusal. Modal because every caller is a direct response
    /// to a click the user just made.
    private func reportFailure(action: String, error: String) {
        let alert = NSAlert()
        alert.messageText = "\(action) failed"
        alert.informativeText = error
        alert.alertStyle = .warning
        alert.addButton(withTitle: "OK")
        alert.runModal()
    }

    /// Guard the permanent purge behind an explicit confirmation — it kills the
    /// session and erases the recipe (layout, hostnames, dev command), unlike
    /// Close Session, which keeps everything for a later revive.
    private func confirmRemoveSession(name: String, perform: @escaping () -> Void) {
        let alert = NSAlert()
        alert.messageText = "Remove “\(name)”?"
        alert.informativeText = "This kills the session and permanently deletes its panes, layout, hostnames, and dev command. Use Close Session instead to keep them for a later revive."
        alert.alertStyle = .warning
        alert.addButton(withTitle: "Remove")
        alert.addButton(withTitle: "Cancel")
        if alert.runModal() == .alertFirstButtonReturn { perform() }
    }

    /// Flip the workspace's dev server: the daemon spawns/kills its dev pane.
    private func toggleDevServer(_ id: UUID) {
        let running = remoteService.devRunning[id] ?? false
        Task {
            guard let error = await remoteService.setDevServer(id, running: !running) else { return }
            await MainActor.run {
                reportFailure(action: running ? "Stop dev server" : "Start dev server", error: error)
            }
        }
    }

    /// One mapped hostname's menu entry: open / copy, with the dev server's
    /// listening state in the icon (filled = answering, dashed = nothing on
    /// the port yet).
    @ViewBuilder
    private func hostnameMenu(_ hostname: DaemonHostname) -> some View {
        if let url = hostname.url {
            Menu {
                Button("Open") {
                    if let u = URL(string: url) { NSWorkspace.shared.open(u) }
                }
                Button("Copy URL") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(url, forType: .string)
                }
            } label: {
                Label("\(hostname.name) : \(hostname.port)",
                      systemImage: hostname.listening ? "circle.fill" : "circle.dashed")
            }
        }
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

// MARK: - Hosted adornments

/// The attach-connection health dot on a hosted row.
private struct ConnectionDot: View {
    let state: DaemonConnectionState

    var body: some View {
        Circle()
            .fill(color)
            .frame(width: 6, height: 6)
            .help(help)
    }

    private var color: Color {
        switch state {
        case .connected: return .green
        case .connecting, .reconnecting: return .yellow
        case .closed: return .red
        }
    }

    private var help: String {
        switch state {
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
    /// Non-nil marks a hosted (daemon-backed) workspace: beacon icon before the
    /// name, attach-connection dot after the badges.
    var hostedConnection: DaemonConnectionState?
    /// Dev-hostname mappings (hosted only): each renders as a clickable https
    /// URL in the expanded dashboard, with a listening dot from the daemon's probe.
    var hostnames: [DaemonHostname] = []
    /// Dev-server pane state + toggle (hosted only): ▶/■ on the first hostname row.
    var devRunning: Bool = false
    var onToggleDevServer: (() -> Void)?

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

                // Dev hostnames (hosted): daemon-stamped URL + listening probe;
                // the first row carries the dev-server ▶/■ toggle.
                ForEach(Array(hostnames.enumerated()), id: \.element.id) { index, hostname in
                    if let url = hostname.url {
                        HStack(spacing: 4) {
                            Circle()
                                .fill(hostname.listening ? Color.green : Color.secondary.opacity(0.4))
                                .frame(width: 5, height: 5)
                                .help(hostname.listening ? "Dev server answering on port \(hostname.port)" : "Nothing listening on port \(hostname.port)")
                            Text(url.replacingOccurrences(of: "https://", with: ""))
                                .font(.system(size: 10, design: .monospaced))
                                .foregroundColor(.secondary)
                                .lineLimit(1)
                                .truncationMode(.middle)
                                .onTapGesture {
                                    if let u = URL(string: url) { NSWorkspace.shared.open(u) }
                                }
                                .help("Open \(url)")
                            if index == 0, let onToggleDevServer {
                                Spacer(minLength: 4)
                                Button(action: onToggleDevServer) {
                                    Image(systemName: devRunning ? "stop.fill" : "play.fill")
                                        .font(.system(size: 8))
                                        .foregroundColor(devRunning ? .orange : .secondary)
                                }
                                .buttonStyle(.borderless)
                                .help(devRunning ? "Stop the dev server (kills its pane)" : "Start the dev server (spawns a pane)")
                            }
                        }
                        .padding(.leading, 4)
                    }
                }

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
                if hostedConnection != nil {
                    Image(systemName: "antenna.radiowaves.left.and.right")
                        .font(.system(size: 10))
                        .foregroundColor(.secondary)
                        .help("Hosted session")
                }

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

                if let hostedConnection {
                    ConnectionDot(state: hostedConnection)
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
    var hostedConnection: DaemonConnectionState?

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
            onFileClicked: onFileClicked,
            hostedConnection: hostedConnection
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

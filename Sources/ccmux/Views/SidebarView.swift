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
    /// Open a shared window this login has closed (v2): the same window
    /// everyone else sees, brought on screen here and woken if asleep.
    var onOpenSharedWindow: ((DaemonWindow) -> Void)?
    /// Daemon-wide health. Defaulted to the shared instance so no call site has
    /// to thread it through.
    @ObservedObject var health: DaemonHealthService = .shared

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
        remoteService.coldWorkspaces.filter { WindowManager.sameWindowName($0.group, name) }
    }

    /// Cold sessions in NO window (ungrouped): they render under AVAILABLE.
    /// Cold members of an open window render inside its section; cold members
    /// of a CLOSED shared window are that window's business — opening it wakes
    /// them — and rendering them here too would put one session in two places.
    private var ungroupedColdWorkspaces: [DaemonWorkspace] {
        remoteService.coldWorkspaces.filter { $0.group.isEmpty }
    }

    /// Live hosted sessions in NO window at all (ungrouped) — sessions in a
    /// closed shared window belong to that window and are reachable by opening
    /// it, not here. A click adds one to this window (a shared edit).
    private var availableLiveWorkspaces: [Workspace] {
        remoteService.workspaces
            .filter {
                (remoteService.groups[$0.id] ?? "").isEmpty
                    && !windowContext.ownedWorkspaceIds.contains($0.id)
                    && !windowContext.otherWindowWorkspaceIds.contains($0.id)
            }
            .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
    }

    /// Shared windows this login has closed — one row each in the Open Window
    /// menu; opening one brings the same window everyone else sees.
    private var closedSharedWindows: [DaemonWindow] {
        remoteService.sharedWindows.filter { !$0.open }
    }

    /// "patric" — whose session an ungrouped AVAILABLE row is. Empty when the
    /// owning host has no owner configured.
    private func ownerLabel(owner: String) -> String {
        owner.split(separator: "@").first.map(String.init) ?? owner
    }

    /// This window's ANSWERABLE name: the custom name, else the auto name
    /// ("Window N"). Never a cosmetic "This Window" — the name is matched
    /// against the shared membership, not just displayed.
    private var thisWindowName: String {
        windowContext.windowName ?? windowContext.autoName
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

            // Daemon-level warning. Rendered only when there is something to
            // say, so it never becomes furniture the eye learns to skip. Same
            // rule and same words as the web lens's #daemon-warning strip.
            if health.shouldWarn {
                Text(health.warningText)
                    .font(.system(size: 11))
                    .foregroundColor(.orange)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, 12)
                    .padding(.top, 6)
            }

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
                                    onRenameWindow?(windowId, thisWindowName)
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

                // AVAILABLE: everything not in our windows — views are per-user
                // on the daemon, so this holds other people's sessions (labeled
                // "who · their window") and our own put-away or unhomed ones.
                // A live row's click adds it to THIS window (writes only our
                // view row); a cold row's click claims it here and revives.
                // Cold rows whose group matches an open window still render
                // inside that window's section above.
                if !availableLiveWorkspaces.isEmpty || !ungroupedColdWorkspaces.isEmpty {
                    Section {
                        ForEach(availableLiveWorkspaces) { workspace in
                            availableRow(workspace)
                        }
                        ForEach(ungroupedColdWorkspaces, id: \.id) { cold in
                            coldRow(cold, claimHere: true)
                        }
                    } header: {
                        windowSectionHeader(name: "AVAILABLE", isCurrentWindow: false)
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

                    // Open Window: shared windows this login has closed — the
                    // same window everyone else sees; opening wakes it.
                    if !closedSharedWindows.isEmpty {
                        Divider()
                        Text("Open Window")

                        ForEach(closedSharedWindows, id: \.id) { win in
                            Button {
                                onOpenSharedWindow?(win)
                            } label: {
                                Label(
                                    "\(win.name) (\(win.workspaceIds.count))",
                                    systemImage: "macwindow.on.rectangle")
                            }
                        }
                    }

                    // Restore Window section (local-workspace windows only —
                    // hosted windows live in the shared list above)
                    if !manager.closedWindows.isEmpty {
                        Divider()
                        Text("Restore Window")

                        ForEach(manager.closedWindows) { cw in
                            // Hosted sessions keep running while their window is closed,
                            // so they are named from the live list rather than the
                            // closed one — without this an unnamed window holding only
                            // hosted sessions reads as a bare "Window".
                            let wsNames = cw.workspaceIds.compactMap { id in
                                manager.closedWorkspaces.first(where: { $0.id == id })?.name
                                    ?? remoteService.workspaces.first(where: { $0.id == id })?.name
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
            // A crashed server leaves its pane at a shell: both entries show
            // there (Start re-types the command into that pane, daemon-side).
            // A hand-started server keeps Start too.
            if !devIsAnswering(workspace.id) {
                Button("Start Dev Server") { setDevServer(workspace.id, start: true) }
            }
            if remoteService.devRunning[workspace.id] == true {
                Button("Stop Dev Server") { setDevServer(workspace.id, start: false) }
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
                Task {
                    guard let error = await remoteService.deleteWorkspace(workspace.id) else { return }
                    await MainActor.run {
                        reportFailure(action: "Remove “\(workspace.name)”", error: error)
                    }
                }
            }
        }
    }

    /// A live hosted session not in any of our windows: named, labeled with
    /// whose it is, and a click adds it to THIS window — which writes only our
    /// own view row on the daemon, never anyone else's arrangement.
    private func availableRow(_ workspace: Workspace) -> some View {
        HStack(spacing: 6) {
            Image(systemName: "antenna.radiowaves.left.and.right")
                .font(.system(size: 11))
            Text(workspace.name)
                .lineLimit(1)
            Spacer()
            Text(ownerLabel(owner: remoteService.owners[workspace.id] ?? ""))
                .font(.system(size: 10))
                .foregroundColor(Color.secondary.opacity(0.7))
                .lineLimit(1)
        }
        .foregroundColor(.secondary)
        .contentShape(Rectangle())
        .onTapGesture {
            onMoveToThisWindow(workspace.id)
        }
        .contextMenu {
            Button("Add to This Window") {
                onMoveToThisWindow(workspace.id)
            }
        }
    }

    /// A cold hosted session: dimmed, with a moon icon; click (or the menu's
    /// Revive) recreates the tmux session from the daemon's stored recipe.
    /// claimHere (the AVAILABLE bucket) first writes our view row for THIS
    /// window, so the revived session lands where the click happened — on this
    /// Mac and every other lens — instead of wherever a name match falls.
    private func coldRow(_ cold: DaemonWorkspace, claimHere: Bool = false) -> some View {
        HStack(spacing: 6) {
            Image(systemName: "moon.zzz")
                .font(.system(size: 11))
            Text(cold.name)
                .lineLimit(1)
            Spacer()
            if claimHere {
                Text(ownerLabel(owner: cold.owner))
                    .font(.system(size: 10))
                    .foregroundColor(Color.secondary.opacity(0.7))
                    .lineLimit(1)
            }
        }
        .foregroundColor(.secondary)
        .contentShape(Rectangle())
        .onTapGesture {
            revive(cold, claimHere: claimHere)
        }
        .contextMenu {
            Button(claimHere ? "Revive in This Window" : "Revive") {
                revive(cold, claimHere: claimHere)
            }
            Divider()
            Button("Remove Session…", role: .destructive) {
                confirmRemoveSession(name: cold.name) {
                    Task {
                        guard let error = await remoteService.deleteWorkspace(daemonId: cold.id)
                        else { return }
                        await MainActor.run {
                            reportFailure(action: "Remove “\(cold.name)”", error: error)
                        }
                    }
                }
            }
        }
    }

    /// Resurrect a cold session, reporting a refusal instead of swallowing it.
    /// The click has no other visible effect when it fails, so silence reads as a
    /// dead row. claimHere places it into this window BEFORE the revive (see
    /// coldRow) — after a failed claim the revive still runs, but with no row
    /// the session comes up under AVAILABLE instead of this window.
    private func revive(_ cold: DaemonWorkspace, claimHere: Bool = false) {
        let windowName = thisWindowName
        Task {
            if claimHere, !(await remoteService.setGroup(daemonId: cold.id, to: windowName)) {
                NSLog("[ccmux] revive: claiming %@ into %@ failed; it will come up under AVAILABLE",
                      cold.id, windowName)
            }
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

    /// Drive the workspace's dev server. Start with a present-but-dead pane
    /// re-types the command into that pane daemon-side, keeping its scrollback.
    private func setDevServer(_ id: UUID, start: Bool) {
        Task {
            guard let error = await remoteService.setDevServer(id, running: start) else { return }
            await MainActor.run {
                reportFailure(action: start ? "Start dev server" : "Stop dev server", error: error)
            }
        }
    }

    /// The one "dev server is running" rule for this view: the pane exists
    /// AND a mapped port answers. The context menu and the ▶/■ toggle both
    /// read it here so they can never disagree; WorkspaceRow.devShowsStop is
    /// the same rule over its own passed-in data.
    private func devIsAnswering(_ id: UUID) -> Bool {
        remoteService.devRunning[id] == true
            && (remoteService.hostnames[id] ?? []).contains { $0.listening }
    }

    /// The ▶/■ toggle's intent: ■ (stop) only while devIsAnswering; anything
    /// else — no pane, or a crashed pane — is ▶.
    private func toggleDevServer(_ id: UUID) {
        setDevServer(id, start: !devIsAnswering(id))
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
    /// Dev-server PANE presence + toggle (hosted only): ▶/■ on the first
    /// hostname row. Whether the server actually runs is judged from the
    /// hostnames' listening probe, not from this flag — see devShowsStop.
    var devRunning: Bool = false
    var onToggleDevServer: (() -> Void)?

    /// ■ only while the pane exists AND a mapped port answers. A crashed
    /// server's leftover pane shows ▶ again; pressing it re-types the command
    /// into that same pane (daemon-side) instead of doing nothing.
    private var devShowsStop: Bool {
        devRunning && hostnames.contains { $0.listening }
    }

    /// Tooltip for the ▶/■ button's three states, kept beside the rule that
    /// picks the glyph.
    private var devHelp: String {
        if devShowsStop { return "Stop the dev server (kills its pane)" }
        if devRunning { return "Restart the dev server in its pane (it isn't answering)" }
        return "Start the dev server (spawns a pane)"
    }

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
                                    Image(systemName: devShowsStop ? "stop.fill" : "play.fill")
                                        .font(.system(size: 8))
                                        .foregroundColor(devShowsStop ? .orange : .secondary)
                                }
                                .buttonStyle(.borderless)
                                .help(devHelp)
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
            // The label owns its own click. Without this the gap after the name
            // is dead space: a bare HStack hit-tests only its glyphs, and the
            // empty pixels fall through to the List row, where reaching the
            // AttentionRowBackground's tap is up to the AppKit List. That made
            // "click the row to switch" work on one Mac and not another on the
            // same build. Selecting here does not fight the disclosure triangle,
            // which DisclosureGroup draws outside this label.
            .frame(maxWidth: .infinity, alignment: .leading)
            .contentShape(Rectangle())
            .onTapGesture { onSelect?() }
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

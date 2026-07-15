import Foundation
import Combine

/// The lens-side face of ccmuxd: fetches hosted workspaces over REST, materializes
/// each as an app `Workspace` (`mode: .hosted`) + `SplitTreeController` rendered by
/// the *same* SplitTree/sidebar machinery as local ones, and keeps a live attach
/// connection per workspace for output + attention. The local driver path
/// (`WorkspaceManager`) is left entirely untouched — hosted workspaces live here.
///
/// All mutation happens on the main thread (network hops back via `MainActor.run`),
/// matching the app's AppKit-main-thread model.
final class RemoteSessionService: ObservableObject {
    /// App-wide instance, mirroring `TerminalStore.shared` — hosted pane views resolve
    /// their controller from here, exactly as local pane views use `TerminalStore`.
    static let shared = RemoteSessionService()

    /// Live hosted workspaces, in daemon order. Rendered like local workspaces.
    @Published private(set) var workspaces: [Workspace] = []
    /// Cold (revivable) daemon workspaces — surfaced for a one-click revive affordance.
    @Published private(set) var coldWorkspaces: [DaemonWorkspace] = []
    /// Per-workspace connection state, for the reconnect overlay.
    @Published private(set) var connectionStates: [UUID: DaemonConnectionState] = [:]

    /// Daemon pane ids whose shared tmux pane was driven to a size this app can't
    /// show 1:1 (another lens took over). Drives the hosted "Take over" control.
    @Published private(set) var stalePanes: Set<String> = []
    /// True once a `/v1/workspaces` fetch has succeeded (daemon reachable).
    @Published private(set) var reachable = false
    @Published private(set) var lastError: String?

    private(set) var controllers: [UUID: SplitTreeController] = [:]
    private(set) var attentionMonitors: [UUID: ClaudeAttentionMonitor] = [:]
    /// Remote-fed monitors driving the hosted rows' git dashboard and bolt icon —
    /// same observable types the local sidebar rows use, fed from daemon data.
    private(set) var gitMonitors: [UUID: GitStatusMonitor] = [:]
    private(set) var claudeMonitors: [UUID: ClaudeProcessMonitor] = [:]
    /// Last-known shared sidebar group per hosted workspace (from the daemon).
    /// WindowManager diffs its window names against this before pushing.
    private(set) var groups: [UUID: String] = [:]

    private var attachments: [UUID: WorkspaceAttachment] = [:]
    private var paneSignatures: [UUID: [String]] = [:]
    private var daemonIds: [UUID: String] = [:]

    /// One global firehose (`/v1/events`) drives the sidebar attention flash for
    /// every hosted workspace, attached or not — the single hosted-attention source.
    private let events = DaemonEventsClient()
    /// Latest firehose attention per daemon workspace id, retained so a workspace
    /// that materializes (or rebuilds) after its attention arrived still flashes.
    private var latestAttention: [String: DaemonAttention] = [:]

    // Layout-blob sync (Phase 7): each hosted workspace's split arrangement is
    // versioned by the daemon; we restore it on build and PUT real edits back.
    private var layoutVersions: [UUID: Int] = [:]
    private var lastLayoutBlob: [UUID: String] = [:]      // last blob we've reconciled with the daemon
    private var layoutObservers: [UUID: AnyCancellable] = [:]

    /// True while the user is watching this hosted workspace (suppresses the flash).
    var isWatched: (UUID) -> Bool = { _ in false }
    /// Fired for a needs-input/done transition on an unwatched hosted workspace.
    var onAttention: ((Workspace, AttentionState) -> Void)?
    /// A clicked file link (absolute local path) in a hosted pane, surfaced to the app.
    var onFileLink: ((UUID, String) -> Void)?
    /// Fired after each reconcile — the window layer adopts hosted workspaces that
    /// no window owns yet (created by another lens) into a sidebar group.
    var onWorkspacesChanged: (() -> Void)?
    /// Fired when a hosted workspace is genuinely gone from the daemon (removed by
    /// some lens) — NOT on a mere pane-change rebuild, which keeps the same id.
    var onWorkspaceRemoved: ((UUID) -> Void)?

    private let session = URLSession(configuration: .default)
    private var pollTimer: Timer?

    // MARK: - Lifecycle

    /// Begin polling the daemon for hosted workspaces (also fires once immediately)
    /// and open the global attention firehose.
    func start(pollInterval: TimeInterval = 4) {
        Task { await refresh() }
        let timer = Timer.scheduledTimer(withTimeInterval: pollInterval, repeats: true) { [weak self] _ in
            Task { await self?.refresh() }
        }
        pollTimer = timer

        events.onEvent = { [weak self] in self?.handleFirehose($0) }
        events.connect()
    }

    func stop() {
        pollTimer?.invalidate()
        pollTimer = nil
        events.disconnect()
        for id in Array(attachments.keys) { removeWorkspace(id) }
    }

    // MARK: - Attention firehose

    /// Route a decoded firehose event onto the sidebar flash. `hello` seeds current
    /// state silently (no notification burst on connect); a live `attention` change
    /// both flashes and notifies.
    private func handleFirehose(_ event: DaemonFirehoseEvent) {
        switch event {
        case .hello(let entries):
            for e in entries { applyAttention(daemonWsId: e.workspace, state: e.state, notify: false) }
        case .attention(let workspace, _, let state):
            applyAttention(daemonWsId: workspace, state: state, notify: true)
        case .workspaceChanged:
            // A workspace appeared/vanished/changed live↔cold elsewhere — pick it up
            // now rather than at the next poll.
            Task { await refresh() }
        case .unknown:
            break
        }
    }

    /// Drive one hosted workspace's flash from a firehose attention change — the
    /// single hosted-attention source, working whether or not the workspace has a
    /// live attach. Mirrors the local hook path (`ClaudeHookListener.handle`): a
    /// `.none` mapping clears; an actionable state either clears (already watching)
    /// or flashes, and — for a live change — posts a notification.
    private func applyAttention(daemonWsId: String, state: DaemonAttention, notify: Bool) {
        latestAttention[daemonWsId] = state
        let appId = RemoteWorkspaceBuilder.workspaceUUID(daemonWsId)
        guard let monitor = attentionMonitors[appId] else { return }
        let appState = state.appAttentionState
        guard appState != .none else { monitor.clear(); return }
        if isWatched(appId) { monitor.clear(); return }
        monitor.set(appState)
        if notify, let ws = workspaces.first(where: { $0.id == appId }) {
            onAttention?(ws, appState)
        }
    }

    // MARK: - View access

    func splitController(for id: UUID) -> SplitTreeController? { controllers[id] }

    func controller(forWorkspace id: UUID, paneId: String, workingDirectory: String) -> RemoteTermController? {
        attachments[id]?.controller(forPane: paneId, workingDirectory: workingDirectory)
    }

    func connectionState(for id: UUID) -> DaemonConnectionState {
        connectionStates[id] ?? .connecting
    }

    func isHosted(_ id: UUID) -> Bool { controllers[id] != nil }

    // MARK: - Reverse lookup by daemon pane id (hosted pane views)

    /// The controller for a hosted pane, found across all attachments (pane ids are
    /// globally unique). Returns nil only if the workspace isn't attached yet.
    func hostedController(paneId: String, workingDirectory: String) -> RemoteTermController? {
        for att in attachments.values where att.hasPane(paneId) {
            return att.controller(forPane: paneId, workingDirectory: workingDirectory)
        }
        return nil
    }

    /// Whether another lens drove `paneId`'s shared pane to a size this app can't
    /// show 1:1 — drives the hosted "Take over" control.
    func hostedIsStale(paneId: String) -> Bool { stalePanes.contains(paneId) }

    /// Connection state of the workspace owning `paneId`, for the reconnect overlay.
    func hostedConnectionState(paneId: String) -> DaemonConnectionState {
        for (wsId, att) in attachments where att.hasPane(paneId) {
            return connectionStates[wsId] ?? att.connectionState
        }
        return .connecting
    }

    // MARK: - REST

    func refresh() async {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces") else { return }
        do {
            let (data, resp) = try await session.data(from: url)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
                throw URLError(.badServerResponse)
            }
            let list = try JSONDecoder().decode([DaemonWorkspace].self, from: data)
            await MainActor.run { self.reconcile(list) }
        } catch {
            await MainActor.run {
                self.reachable = false
                self.lastError = error.localizedDescription
            }
        }
    }

    /// Fetch the selectable folders at `path` (relative to the daemon's projects
    /// root; "" = the root itself). Throws so the picker can say *why* the list
    /// is empty (daemon down, no root).
    func fetchProjects(path: String = "") async throws -> DaemonProjectList {
        var comps = URLComponents(string: "\(DaemonConfig.baseURL)/v1/projects")
        if !path.isEmpty {
            comps?.queryItems = [URLQueryItem(name: "path", value: path)]
        }
        guard let url = comps?.url else {
            throw URLError(.badURL)
        }
        let (data, resp) = try await session.data(from: url)
        guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
            throw URLError(.badServerResponse)
        }
        return try JSONDecoder().decode(DaemonProjectList.self, from: data)
    }

    /// Create a hosted workspace and return its app-side id (nil on failure), so
    /// the creating window can claim it into its sidebar group.
    @discardableResult
    func createWorkspace(name: String, repoPath: String, cwd: String? = nil, startupCommand: String? = nil) async -> UUID? {
        let body: [String: Any] = [
            "name": name, "repoPath": repoPath,
            "cwd": cwd ?? repoPath, "startupCommand": startupCommand ?? "",
            "createdBy": DaemonConfig.selfUser,
        ]
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces") else { return nil }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        do {
            let (data, resp) = try await session.data(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 201 else { return nil }
            let dw = try JSONDecoder().decode(DaemonWorkspace.self, from: data)
            await refresh()
            return RemoteWorkspaceBuilder.workspaceUUID(dw.id)
        } catch {
            await MainActor.run { self.lastError = error.localizedDescription }
            return nil
        }
    }

    @discardableResult
    func spawnPane(workspace id: UUID, cwd: String? = nil, startupCommand: String? = nil) async -> Bool {
        guard let daemonId = daemonIds[id] else { return false }
        let body: [String: Any] = [
            "cwd": cwd ?? "", "startupCommand": startupCommand ?? "",
            "createdBy": DaemonConfig.selfUser,
        ]
        let ok = await post("/v1/workspaces/\(daemonId)/panes", body: body, expect: 201)
        if ok { await refresh() }
        return ok
    }

    @discardableResult
    func reviveWorkspace(daemonId: String) async -> Bool {
        let ok = await post("/v1/workspaces/\(daemonId)/revive", body: [:], expect: 200)
        if ok { await refresh() }
        return ok
    }

    func deleteWorkspace(_ id: UUID) async {
        guard let daemonId = daemonIds[id] else { return }
        _ = await send("DELETE", path: "/v1/workspaces/\(daemonId)", body: nil, expect: 204)
        await refresh()
    }

    // MARK: - Layout sync

    /// Seed a workspace's layout version/blob and watch the controller for edits.
    /// The initial (built/restored) tree is the baseline — only later, genuine
    /// changes (split/move/close/resize) are pushed, coalesced to avoid churn.
    private func observeLayout(appId: UUID, controller: SplitTreeController, tree: SplitTree<PaneTabs>, version: Int) {
        layoutVersions[appId] = version
        lastLayoutBlob[appId] = HostedLayoutCodec.encode(tree)
        layoutObservers[appId] = controller.$tree
            .dropFirst() // skip the initial value; only real edits should push
            .debounce(for: .milliseconds(400), scheduler: RunLoop.main)
            .sink { [weak self] newTree in self?.onLayoutChanged(appId, newTree) }
    }

    private func onLayoutChanged(_ appId: UUID, _ tree: SplitTree<PaneTabs>) {
        let blob = HostedLayoutCodec.encode(tree)
        guard blob != lastLayoutBlob[appId] else { return } // same arrangement, no PUT
        Task { await self.pushLayout(appId: appId, blob: blob) }
    }

    /// PUT a changed layout with the version we last saw. On success we advance our
    /// version+blob; on 409 another lens won the race, so we adopt its version to
    /// rebase the next edit (live re-render of a remote arrangement is a follow-up).
    private func pushLayout(appId: UUID, blob: String) async {
        guard let daemonId = daemonIds[appId] else { return }
        let base = layoutVersions[appId] ?? 0
        let (code, version) = await putLayoutRequest(daemonId: daemonId, blob: blob, baseVersion: base)
        await MainActor.run {
            switch code {
            case 200:
                self.lastLayoutBlob[appId] = blob
                if let version { self.layoutVersions[appId] = version }
            case 409:
                if let version { self.layoutVersions[appId] = version }
            default:
                break
            }
        }
    }

    private func putLayoutRequest(daemonId: String, blob: String, baseVersion: Int) async -> (Int, Int?) {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/layout") else { return (0, nil) }
        var req = URLRequest(url: url)
        req.httpMethod = "PUT"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: ["blob": blob, "baseVersion": baseVersion])
        do {
            let (data, resp) = try await session.data(for: req)
            let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
            let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            return (code, obj?["version"] as? Int)
        } catch {
            return (0, nil)
        }
    }

    // MARK: - Reconciliation (main thread)

    private func reconcile(_ list: [DaemonWorkspace]) {
        reachable = true
        lastError = nil
        let live = list.filter { $0.isLive }
        // Prune retained attention for workspaces the daemon no longer lists live,
        // while keeping it across a mere pane-signature rebuild (still live).
        let liveDaemonIds = Set(live.map { $0.id })
        latestAttention = latestAttention.filter { liveDaemonIds.contains($0.key) }
        let liveIds = Set(live.map { RemoteWorkspaceBuilder.workspaceUUID($0.id) })
        for appId in Array(attachments.keys) where !liveIds.contains(appId) {
            removeWorkspace(appId)
            onWorkspaceRemoved?(appId)
        }
        var rebuilt: [Workspace] = []
        for dw in live {
            let appId = RemoteWorkspaceBuilder.workspaceUUID(dw.id)
            if let existing = workspaces.first(where: { $0.id == appId }),
               paneSignatures[appId] == RemoteWorkspaceBuilder.paneSignature(dw.panes) {
                rebuilt.append(existing)               // unchanged — keep the live connection
            } else {
                if attachments[appId] != nil { removeWorkspace(appId) }
                if let ws = addWorkspace(dw, appId: appId) { rebuilt.append(ws) }
            }
        }
        workspaces = rebuilt
        coldWorkspaces = list.filter { !$0.isLive }
        applyGitAndActivity(live)
        onWorkspacesChanged?()
    }

    /// Feed each live workspace's daemon-computed git dashboard + claude-running
    /// state into its remote-fed monitors. Runs every reconcile (poll or
    /// firehose-triggered), independent of pane-signature rebuilds — git status
    /// and attention change without the pane set changing.
    private func applyGitAndActivity(_ live: [DaemonWorkspace]) {
        for dw in live {
            let appId = RemoteWorkspaceBuilder.workspaceUUID(dw.id)
            groups[appId] = dw.group
            if let git = dw.git {
                let monitor = gitMonitors[appId] ?? {
                    let m = GitStatusMonitor.remoteFed()
                    gitMonitors[appId] = m
                    return m
                }()
                monitor.apply(git.asInfo)
            }
            let claude = claudeMonitors[appId] ?? {
                let m = ClaudeProcessMonitor.remoteFed()
                claudeMonitors[appId] = m
                return m
            }()
            claude.apply(isRunning: dw.panes.contains { $0.attention == .running })
        }
    }

    private func addWorkspace(_ dw: DaemonWorkspace, appId: UUID) -> Workspace? {
        guard let (tree, focused) = RemoteWorkspaceBuilder.buildTree(panes: dw.panes, repoPath: dw.repoPath, layoutBlob: dw.layoutJson)
        else { return nil }
        let monitor = ClaudeAttentionMonitor()
        attentionMonitors[appId] = monitor
        // Re-seed the flash from the firehose's retained state, so a workspace that
        // materializes (or rebuilds on a pane change) after its attention arrived
        // still shows it. Visual only — no notification when merely (re)building.
        if let retained = latestAttention[dw.id], retained.appAttentionState != .none, !isWatched(appId) {
            monitor.set(retained.appAttentionState)
        }
        let controller = SplitTreeController(workingDirectory: dw.repoPath)
        controller.tree = tree
        controller.focusedPaneId = focused
        controllers[appId] = controller
        daemonIds[appId] = dw.id
        paneSignatures[appId] = RemoteWorkspaceBuilder.paneSignature(dw.panes)
        observeLayout(appId: appId, controller: controller, tree: tree, version: dw.layoutVersion ?? 0)

        let attachment = WorkspaceAttachment(
            workspaceId: appId, daemonId: dw.id, repoPath: dw.repoPath, panes: dw.panes)
        attachment.onConnectionState = { [weak self] state in self?.connectionStates[appId] = state }
        attachment.onFileLink = { [weak self] rel in self?.onFileLink?(appId, rel) }
        attachment.onPaneStale = { [weak self] pane, stale in
            guard let self else { return }
            if stale { self.stalePanes.insert(pane) } else { self.stalePanes.remove(pane) }
        }
        attachments[appId] = attachment
        connectionStates[appId] = .connecting
        attachment.connect()

        return Workspace(
            id: appId, name: dw.name, repoPath: dw.repoPath,
            layout: tree, focusedPaneId: focused, subItems: [], lastOpened: Date(), mode: .hosted)
    }

    private func removeWorkspace(_ appId: UUID) {
        attachments[appId]?.disconnect()
        attachments.removeValue(forKey: appId)
        controllers.removeValue(forKey: appId)
        attentionMonitors[appId]?.stop()
        attentionMonitors.removeValue(forKey: appId)
        layoutObservers[appId]?.cancel()
        layoutObservers.removeValue(forKey: appId)
        layoutVersions.removeValue(forKey: appId)
        lastLayoutBlob.removeValue(forKey: appId)
        paneSignatures.removeValue(forKey: appId)
        daemonIds.removeValue(forKey: appId)
        connectionStates.removeValue(forKey: appId)
        gitMonitors.removeValue(forKey: appId)
        claudeMonitors.removeValue(forKey: appId)
        groups.removeValue(forKey: appId)
        workspaces.removeAll { $0.id == appId }
    }

    // MARK: - Shared sidebar group

    /// Push a hosted workspace's sidebar group to the daemon (the owning window's
    /// name — this Mac is the source of truth; web/phone render the same groups).
    /// The local cache updates optimistically so the diff-based sync stays quiet
    /// until the next reconcile confirms.
    func setGroup(_ appId: UUID, to name: String) async {
        guard let daemonId = daemonIds[appId] else { return }
        let body = try? JSONSerialization.data(withJSONObject: ["group": name])
        if await send("PUT", path: "/v1/workspaces/\(daemonId)/group", body: body, expect: 204) {
            await MainActor.run { self.groups[appId] = name }
        }
    }

    // MARK: - HTTP helpers

    private func post(_ path: String, body: [String: Any], expect: Int) async -> Bool {
        let data = try? JSONSerialization.data(withJSONObject: body)
        return await send("POST", path: path, body: data, expect: expect)
    }

    private func send(_ method: String, path: String, body: Data?, expect: Int) async -> Bool {
        guard let url = URL(string: "\(DaemonConfig.baseURL)\(path)") else { return false }
        var req = URLRequest(url: url)
        req.httpMethod = method
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        do {
            let (_, resp) = try await session.data(for: req)
            return (resp as? HTTPURLResponse)?.statusCode == expect
        } catch {
            await MainActor.run { self.lastError = error.localizedDescription }
            return false
        }
    }
}

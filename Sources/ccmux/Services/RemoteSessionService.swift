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
    /// True once a `/v1/workspaces` fetch has succeeded (daemon reachable).
    @Published private(set) var reachable = false
    @Published private(set) var lastError: String?

    private(set) var controllers: [UUID: SplitTreeController] = [:]
    private(set) var attentionMonitors: [UUID: ClaudeAttentionMonitor] = [:]

    private var attachments: [UUID: WorkspaceAttachment] = [:]
    private var paneSignatures: [UUID: [String]] = [:]
    private var daemonIds: [UUID: String] = [:]

    /// True while the user is watching this hosted workspace (suppresses the flash).
    var isWatched: (UUID) -> Bool = { _ in false }
    /// Fired for a needs-input/done transition on an unwatched hosted workspace.
    var onAttention: ((Workspace, AttentionState) -> Void)?
    /// A clicked file link (absolute local path) in a hosted pane, surfaced to the app.
    var onFileLink: ((UUID, String) -> Void)?

    private let session = URLSession(configuration: .default)
    private var pollTimer: Timer?

    // MARK: - Lifecycle

    /// Begin polling the daemon for hosted workspaces (also fires once immediately).
    func start(pollInterval: TimeInterval = 4) {
        Task { await refresh() }
        let timer = Timer.scheduledTimer(withTimeInterval: pollInterval, repeats: true) { [weak self] _ in
            Task { await self?.refresh() }
        }
        pollTimer = timer
    }

    func stop() {
        pollTimer?.invalidate()
        pollTimer = nil
        for id in Array(attachments.keys) { removeWorkspace(id) }
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

    @discardableResult
    func createWorkspace(name: String, repoPath: String, cwd: String? = nil, startupCommand: String? = nil) async -> Bool {
        let body: [String: Any] = [
            "name": name, "repoPath": repoPath,
            "cwd": cwd ?? repoPath, "startupCommand": startupCommand ?? "",
            "createdBy": DaemonConfig.selfUser,
        ]
        let ok = await post("/v1/workspaces", body: body, expect: 201)
        if ok { await refresh() }
        return ok
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

    // MARK: - Reconciliation (main thread)

    private func reconcile(_ list: [DaemonWorkspace]) {
        reachable = true
        lastError = nil
        let live = list.filter { $0.isLive }
        let liveIds = Set(live.map { RemoteWorkspaceBuilder.workspaceUUID($0.id) })
        for appId in Array(attachments.keys) where !liveIds.contains(appId) {
            removeWorkspace(appId)
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
    }

    private func addWorkspace(_ dw: DaemonWorkspace, appId: UUID) -> Workspace? {
        guard let (tree, focused) = RemoteWorkspaceBuilder.buildTree(panes: dw.panes, repoPath: dw.repoPath)
        else { return nil }
        let monitor = ClaudeAttentionMonitor()
        attentionMonitors[appId] = monitor
        let controller = SplitTreeController(workingDirectory: dw.repoPath)
        controller.tree = tree
        controller.focusedPaneId = focused
        controllers[appId] = controller
        daemonIds[appId] = dw.id
        paneSignatures[appId] = RemoteWorkspaceBuilder.paneSignature(dw.panes)

        let attachment = WorkspaceAttachment(
            workspaceId: appId, daemonId: dw.id, repoPath: dw.repoPath,
            panes: dw.panes, attentionMonitor: monitor)
        attachment.isWatched = { [weak self] in self?.isWatched(appId) ?? false }
        attachment.onConnectionState = { [weak self] state in self?.connectionStates[appId] = state }
        attachment.onAttention = { [weak self] appState in
            guard let self, let ws = self.workspaces.first(where: { $0.id == appId }) else { return }
            self.onAttention?(ws, appState)
        }
        attachment.onFileLink = { [weak self] rel in self?.onFileLink?(appId, rel) }
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
        paneSignatures.removeValue(forKey: appId)
        daemonIds.removeValue(forKey: appId)
        connectionStates.removeValue(forKey: appId)
        workspaces.removeAll { $0.id == appId }
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

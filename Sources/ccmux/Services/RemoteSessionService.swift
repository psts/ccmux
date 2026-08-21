import AppKit
import Combine
import Foundation

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
    /// Last-known SHARED window name per hosted workspace (v2: one arrangement
    /// for everyone). Empty = ungrouped (the AVAILABLE section).
    private(set) var groups: [UUID: String] = [:]
    /// The shared window list (GET /v1/windows) — names, members, and who has
    /// each open. `open` is this login's flag.
    @Published private(set) var sharedWindows: [DaemonWindow] = []
    /// Whose session each live hosted workspace is (owning host's owner
    /// login) — the ungrouped AVAILABLE row label.
    private(set) var owners: [UUID: String] = [:]
    /// Federation registry (GET /v1/hosts), label → host. Empty in single-host
    /// mode. Resolves a workspace's owning host for direct attach + the context line.
    @Published private(set) var hosts: [String: DaemonHost] = [:]
    /// Owning-host label per hosted workspace (from dw.host) — surfaced read-only
    /// in the sidebar context menu.
    private(set) var hostLabels: [UUID: String] = [:]
    /// Dev-hostname mappings per hosted workspace (daemon-stamped url/listening).
    @Published private(set) var hostnames: [UUID: [DaemonHostname]] = [:]
    /// Whether the workspace's dev-server pane is running (▶/■ state).
    @Published private(set) var devRunning: [UUID: Bool] = [:]
    /// Stored dev-command override per workspace ("" = daemon detects).
    private(set) var devCommands: [UUID: String] = [:]

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
    /// The hosted workspaces currently displayed in some window — combined with
    /// user presence to drive the daemon focus frames (phone-push suppression).
    var displayedHostedWorkspaceIds: () -> Set<UUID> = { [] }
    /// At-the-Mac detection (screen unlocked, no screensaver, displays awake).
    private let presenceMonitor = PresenceMonitor()
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
    /// In-flight coalescing window for workspace-change refetches (see
    /// scheduleCoalescedRefresh). nil = nothing pending.
    private var pendingRefresh: Timer?
    /// Wake observer, released in stop() (this service registers no others).
    private var wakeObserver: NSObjectProtocol?

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
        // Every fresh firehose socket is a fresh presence entry on the daemon
        // (starting as "unreported"), so presence must be re-declared on each
        // connect — frames sent while disconnected were dropped by the pump.
        events.onStateChange = { [weak self] state in
            guard let self, state == .connected else { return }
            self.events.reportPresent(self.presenceMonitor.isPresent)
        }
        events.connect()

        presenceMonitor.onChange = { [weak self] present in
            self?.events.reportPresent(present)
            self?.syncFocusFrames()
        }
        observeWake()
    }

    /// Re-dial every daemon socket when the machine wakes.
    ///
    /// The pumps detect a dead path on their own now, but only after a ping goes
    /// unanswered — up to about a minute of a blank sidebar and a frozen terminal.
    /// Waking is the one moment we KNOW the old connections are suspect, so we do
    /// not make the user wait for that.
    ///
    /// Separate from PresenceMonitor's observers, which also fire around sleep:
    /// a presence flip only re-reports focus on sockets it believes are healthy
    /// (syncFocusFrames), and never re-dials anything. Re-dialling is the whole
    /// point here.
    ///
    /// Deliberately no refresh() here. The tailnet is usually still coming up, and
    /// a /v1/workspaces call that answers with a short list makes `reconcile` treat
    /// every missing workspace as deleted — tearing down controllers and blanking
    /// panes. The 4s poll picks the list up a moment later, once the network is
    /// genuinely back.
    private func observeWake() {
        guard wakeObserver == nil else { return }
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(
            forName: NSWorkspace.didWakeNotification, object: nil, queue: .main
        ) { [weak self] _ in
            self?.reconnectAll()
        }
    }

    /// Bounce the firehose AND every workspace attach. Both matter: the firehose
    /// alone restores the sidebar flash while leaving every hosted terminal frozen.
    func reconnectAll() {
        events.forceReconnect()
        for attachment in attachments.values { attachment.forceReconnect() }
    }

    /// Tell the daemon two separate things about this Mac.
    ///
    /// PRESENCE is about the machine: screen awake and unlocked, so a notification
    /// posted here would actually be seen. It does not depend on which workspace is
    /// on display, which is the whole point. You can be three Spaces away in another
    /// app, or looking at a local workspace, and still be sitting right here — and
    /// the old code, which inferred presence from "a hosted workspace is displayed",
    /// called all of that nobody-is-home. The result was a silent Mac and a phone
    /// that buzzed at the desk.
    ///
    /// FOCUS stays per workspace: which pane this lens is actually looking at. It
    /// clears that workspace's flash and nothing more.
    ///
    /// Both ride the same frame, so one call keeps them consistent.
    func syncFocusFrames() {
        let present = presenceMonitor.isPresent
        let displayed = present ? displayedHostedWorkspaceIds() : []
        for (appId, attachment) in attachments {
            let focused = displayed.contains(appId)
            attachment.reportFocus(
                paneId: focused ? (attachment.anyPaneId ?? "") : "",
                present: present
            )
        }
    }

    func stop() {
        pollTimer?.invalidate()
        pollTimer = nil
        pendingRefresh?.invalidate()
        pendingRefresh = nil
        if let wakeObserver {
            NSWorkspace.shared.notificationCenter.removeObserver(wakeObserver)
            self.wakeObserver = nil
        }
        events.disconnect()
        for id in Array(attachments.keys) { removeWorkspace(id) }
    }

    /// A federation hub was adopted after the services connected (cold-boot
    /// discovery landed late): re-point the event firehose at it — a healthy
    /// socket never reconnects on its own — and refresh now instead of waiting
    /// out the poll interval. The REST calls need nothing; they read
    /// `DaemonConfig.baseURL` per request.
    func hubAdopted() {
        events.disconnect()
        events.connect()
        Task { await refresh() }
    }

    // MARK: - Attention firehose

    /// Route a decoded firehose event onto the sidebar flash. `hello` seeds current
    /// state silently (no notification burst on connect); a live `attention` change
    /// both flashes and notifies.
    private func handleFirehose(_ event: DaemonFirehoseEvent) {
        switch event {
        case .hello(let entries):
            for e in entries { applyAttention(daemonWsId: e.workspace, state: e.state, notify: false) }
        case .attention(let workspace, _, let state, let alert):
            applyAttention(daemonWsId: workspace, state: state, notify: alert)
        case .workspaceChanged:
            // A workspace appeared/vanished/changed live↔cold elsewhere — pick it up
            // now rather than at the next poll.
            scheduleCoalescedRefresh()
        case .unknown:
            break
        }
    }

    /// Coalesce workspace-change refetches into one fetch per short window.
    ///
    /// Each of these frames costs a full `/v1/workspaces` round trip, and on a hub
    /// that is every workspace on every host. They arrive in bursts, and a member
    /// was seen emitting one a second for minutes on end while a pane's status
    /// churned — one fetch of the whole fleet per second, indefinitely, to learn
    /// the same thing each time. What the frame actually says is "your list is
    /// stale", and that is worth exactly one fetch however many times it is said.
    ///
    /// Short enough (250ms) that a genuine change still feels instant, which is
    /// the whole reason the app does not simply wait for the 4s poll.
    private func scheduleCoalescedRefresh() {
        guard pendingRefresh == nil else { return }
        pendingRefresh = Timer.scheduledTimer(withTimeInterval: 0.25, repeats: false) { [weak self] _ in
            guard let self else { return }
            self.pendingRefresh = nil
            Task { await self.refresh() }
        }
    }

    /// Drive one hosted workspace's flash from a firehose attention change — the
    /// single hosted-attention source, working whether or not the workspace has a
    /// live attach. Mirrors the local hook path (`ClaudeHookListener.handle`): a
    /// `.none` mapping clears; an actionable state either clears (already watching)
    /// or flashes, and raises a notification only when the DAEMON asked for one.
    ///
    /// `notify` is the daemon's `alert` flag, not a local judgement. The app used
    /// to decide for itself here and drifted from the daemon twice — most recently
    /// alerting on every `done` long after the daemon had stopped pushing on them,
    /// which turned one burst of background agents into an alert per agent.
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

    /// Daemon id for a hosted workspace, live OR cold. A cold session has no
    /// attachment, so `daemonIds` has forgotten it; it is matched instead through the
    /// deterministic app UUID its daemon id maps to.
    func daemonId(forApp appId: UUID) -> String? {
        daemonIds[appId] ?? RemoteWorkspaceBuilder.coldDaemonId(forApp: appId, in: coldWorkspaces)
    }

    /// Whether the daemon knows this workspace at all. Distinguishes a session that
    /// merely went cold (archived — still on record, revivable) from one that is
    /// genuinely gone, which callers must not treat alike.
    func isKnownHosted(_ id: UUID) -> Bool { daemonId(forApp: id) != nil }

    /// The owning-host label for a hosted workspace ("" in single-host mode) —
    /// surfaced read-only in the sidebar context menu.
    func hostLabel(for id: UUID) -> String { hostLabels[id] ?? "" }

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

    /// Fetch the federation registry (hub mode). 404/empty in single-host mode
    /// leaves the map empty, so attach falls back to the configured base.
    func fetchHosts() async {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/hosts"),
              let (data, resp) = try? await session.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let list = try? JSONDecoder().decode([DaemonHost].self, from: data) else { return }
        let map = Dictionary(list.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
        await MainActor.run { self.hosts = map }
    }

    /// The WS origin for a workspace's terminal stream. Federated workspaces attach
    /// DIRECT to their owning host (wss://<host.addr>); an empty/unknown host falls
    /// back to the configured base (single-host, or the hub's own sessions).
    private func attachOrigin(for dw: DaemonWorkspace) -> String? {
        guard !dw.host.isEmpty, let h = hosts[dw.host], !h.addr.isEmpty else { return nil }
        return "wss://\(h.addr)"
    }

    func refresh() async {
        await fetchHosts() // resolve owning-host addresses before we (re)build attachments
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces") else { return }
        do {
            let (data, resp) = try await session.data(from: url)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
                throw URLError(.badServerResponse)
            }
            let list = try JSONDecoder().decode([DaemonWorkspace].self, from: data)
            let windows = await fetchSharedWindows()
            await MainActor.run {
                self.sharedWindows = windows
                self.reconcile(list)
            }
        } catch {
            await MainActor.run {
                self.reachable = false
                self.lastError = error.localizedDescription
            }
        }
    }

    /// The shared window list (GET /v1/windows). A failure keeps the last
    /// list rather than blanking every closed-window row on a blip.
    private func fetchSharedWindows() async -> [DaemonWindow] {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/windows") else { return sharedWindows }
        do {
            let (data, resp) = try await session.data(from: url)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else { return sharedWindows }
            return try JSONDecoder().decode([DaemonWindow].self, from: data)
        } catch {
            return sharedWindows
        }
    }

    /// Member hosts sorted self-first, for the New-session host picker. Empty in
    /// single-host mode (no federation).
    var hostList: [DaemonHost] {
        hosts.values.sorted { a, b in
            a.isSelf != b.isSelf ? a.isSelf : a.id < b.id
        }
    }

    /// The host a new session defaults to: the hub's own node (self), else "".
    var defaultCreateHost: String {
        hosts.values.first(where: { $0.isSelf })?.id ?? ""
    }

    /// Fetch the selectable folders at `path` (relative to the projects root of
    /// `host` — "" = the daemon this lens points at, i.e. the hub in federation).
    /// Throws so the picker can say *why* the list is empty (daemon down, no root).
    func fetchProjects(host: String = "", path: String = "") async throws -> DaemonProjectList {
        let base = host.isEmpty ? "/v1/projects" : "/v1/hosts/\(host)/projects"
        var comps = URLComponents(string: "\(DaemonConfig.baseURL)\(base)")
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

    /// Fetch the daemon-wide lens settings (new-workspace startup command +
    /// per-folder rules).
    func fetchSettings() async throws -> DaemonSettings {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/settings") else {
            throw URLError(.badURL)
        }
        let (data, _) = try await session.data(from: url)
        return try JSONDecoder().decode(DaemonSettings.self, from: data)
    }

    /// Update the daemon-wide settings; nil fields are left unchanged. Returns
    /// the daemon's resolved view (e.g. an empty command comes back as the
    /// built-in default) or nil on failure. Secrets (cloudflareToken,
    /// tailscaleAuthKey) are write-only: pass a value to replace, "" to clear.
    @discardableResult
    func updateSettings(
        startupCommand: String? = nil, startupRules: [DaemonStartupRule]? = nil,
        devDomain: String? = nil, lensHostname: String? = nil,
        cloudflareToken: String? = nil, tailscaleAuthKey: String? = nil
    ) async -> DaemonSettings? {
        var body: [String: Any] = [:]
        if let startupCommand { body["startupCommand"] = startupCommand }
        if let startupRules {
            body["startupRules"] = startupRules.map { ["pathPrefix": $0.pathPrefix, "command": $0.command] }
        }
        if let devDomain { body["devDomain"] = devDomain }
        if let lensHostname { body["lensHostname"] = lensHostname }
        if let cloudflareToken { body["cloudflareToken"] = cloudflareToken }
        if let tailscaleAuthKey { body["tailscaleAuthKey"] = tailscaleAuthKey }
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/settings") else { return nil }
        var req = URLRequest(url: url)
        req.httpMethod = "PUT"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        guard let (data, resp) = try? await session.data(for: req),
              (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
        return try? JSONDecoder().decode(DaemonSettings.self, from: data)
    }

    /// Create a hosted workspace and return its app-side id (nil on failure), so
    /// the creating window can claim it into its sidebar group.
    /// startupCommand nil = OMIT the field, so the daemon applies its configured
    /// default (the Settings-editable command); "" explicitly means a bare shell.
    @discardableResult
    func createWorkspace(host: String = "", name: String, repoPath: String, cwd: String? = nil, startupCommand: String? = nil) async -> UUID? {
        var body: [String: Any] = [
            "name": name, "repoPath": repoPath,
            "cwd": cwd ?? repoPath,
            "createdBy": DaemonConfig.selfUser,
        ]
        if let startupCommand { body["startupCommand"] = startupCommand }
        // Federation: create on the chosen host (self runs local at the hub).
        let base = host.isEmpty ? "/v1/workspaces" : "/v1/hosts/\(host)/workspaces"
        guard let url = URL(string: "\(DaemonConfig.baseURL)\(base)") else { return nil }
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
        guard await postSpawnPane(daemonId: daemonId, cwd: cwd ?? "", startupCommand: startupCommand ?? "") != nil else {
            return false
        }
        await refresh()
        return true
    }

    /// POST a new pane to the daemon and decode the created pane (nil on failure).
    private func postSpawnPane(daemonId: String, cwd: String = "", startupCommand: String = "") async -> DaemonPane? {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/panes") else { return nil }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: [
            "cwd": cwd, "startupCommand": startupCommand, "createdBy": DaemonConfig.selfUser,
        ])
        do {
            let (data, resp) = try await session.data(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 201 else { return nil }
            return try JSONDecoder().decode(DaemonPane.self, from: data)
        } catch {
            await MainActor.run { self.lastError = error.localizedDescription }
            return nil
        }
    }

    /// New Terminal Tab / Split in a hosted workspace: the terminal must live on
    /// the daemon (a tmux pane every lens sees), never as a local child shell.
    /// Spawn it, then land it exactly where the user asked. The final refresh
    /// converges the pane signature; the patch path makes it churn-free.
    private func placeSpawnedTerminal(appId: UUID, leafId: UUID, direction: SplitDirection?) async {
        guard let daemonId = daemonIds[appId],
              let pane = await postSpawnPane(daemonId: daemonId) else { return }
        await MainActor.run {
            guard let controller = controllers[appId], let attachment = attachments[appId] else { return }
            _ = attachment.controller(forPane: pane.id, workingDirectory: pane.cwd) // warm before the view asks
            if let placed = RemoteWorkspaceBuilder.insertingPane(
                pane, into: controller.tree, at: leafId, direction: direction, repoPath: attachment.repoPath) {
                controller.tree = placed.tree
                controller.focusedPaneId = placed.focusLeafId
            }
        }
        await refresh()
    }

    /// Kill a hosted pane on the daemon (a hosted tab's ✕ — the tab is already
    /// gone locally). If the kill fails, the next reconcile's merge resurfaces
    /// the still-running pane rather than leaving it silently headless.
    private func killHostedPane(appId: UUID, paneId: String) async {
        guard let daemonId = daemonIds[appId] else { return }
        _ = await send("DELETE", path: "/v1/workspaces/\(daemonId)/panes/\(paneId)", body: nil, expect: 204)
        await refresh()
    }

    /// Resurrect a cold workspace from its stored recipe. Returns nil on success or
    /// the daemon's error text — a refusal here is the answer to a click on a cold
    /// row, so it must reach the user rather than leaving the row looking inert.
    func reviveWorkspace(daemonId: String) async -> String? {
        let error = await sendReportingError(
            "POST", path: "/v1/workspaces/\(daemonId)/revive", body: [:], expect: 200)
        if error == nil { await refresh() }
        return error
    }

    @discardableResult
    func deleteWorkspace(_ id: UUID) async -> String? {
        guard let daemonId = daemonIds[id] else { return "workspace is not hosted" }
        return await deleteWorkspace(daemonId: daemonId)
    }

    /// Delete by daemon id — cold workspaces aren't materialized as app
    /// workspaces, so their rows act on the raw daemon id.
    @discardableResult
    func deleteWorkspace(daemonId: String) async -> String? {
        let error = await sendReportingError(
            "DELETE", path: "/v1/workspaces/\(daemonId)", body: nil, expect: 204)
        await refresh()
        // Announce it — but only on success. The reconcile sweep announces removals only
        // for workspaces it holds an ATTACHMENT for, and a cold one has none, which is
        // the normal state for anything in a closed window; without this, deleting a
        // cold session leaves its id in that window's record forever. Announcing a
        // delete that FAILED is worse than not announcing: for a live session it strips
        // ownership from every window and repoints whichever one displayed it, so the
        // session vanishes with no error and is re-adopted seconds later somewhere else.
        guard error == nil else { return error }
        await MainActor.run { onWorkspaceRemoved?(RemoteWorkspaceBuilder.workspaceUUID(daemonId)) }
        return nil
    }

    /// "Close Session": kill the tmux session but keep the recipe — the
    /// workspace goes cold (revivable with layout, hostnames, dev command).
    /// Kill the tmux session, keep the recipe. Returns nil on success or the daemon's
    /// error text — closing a window archives every session in it, and a silent failure
    /// there leaves one running with no window to show it.
    @discardableResult
    func archiveWorkspace(_ id: UUID) async -> String? {
        guard let daemonId = daemonIds[id] else { return "workspace is not hosted" }
        let error = await sendReportingError(
            "POST", path: "/v1/workspaces/\(daemonId)/archive", body: [:], expect: 200)
        await refresh()
        return error
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
        // Publish the cold set BEFORE announcing removals. `onWorkspaceRemoved` asks
        // whether the daemon still knows the workspace, to tell "archived" apart from
        // "deleted" — and it would read a stale list here and call every archive a
        // deletion, taking the closed-window record that restores it with it.
        // Sorted here as well as on the daemon: cold rows render in this array's
        // order, and an older daemon still serves an every-poll shuffle.
        coldWorkspaces = list.filter { !$0.isLive }
            .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
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
            } else if let patched = patchWorkspace(dw, appId: appId) {
                rebuilt.append(patched)                // pane set changed — patch the live tree in place
            } else {
                if attachments[appId] != nil { removeWorkspace(appId) }
                if let ws = addWorkspace(dw, appId: appId) {
                    rebuilt.append(ws)
                } else {
                    // A live workspace with no buildable tree (no panes) drops out of
                    // `workspaces` here. Without the callback, WindowManager keeps owning
                    // the id and persists it into WindowDescriptor, leaving a window
                    // pointed at a workspace that no longer exists. Logged because the
                    // daemon calls it live: a row reaching this branch is a registry the
                    // daemon-side last-pane fix has not cleaned up yet.
                    NSLog("[ccmux] daemon workspace %@ is live but has no buildable tree (%d panes) — dropping",
                          dw.id, dw.panes.count)
                    onWorkspaceRemoved?(appId)
                }
            }
        }
        workspaces = rebuilt
        applyGitAndActivity(live)
        syncFocusFrames() // fresh attachments start unfocused; keep them truthful
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
            owners[appId] = dw.owner
            hostLabels[appId] = dw.host
            hostnames[appId] = dw.hostnames
            devRunning[appId] = dw.panes.contains { $0.devServer }
            devCommands[appId] = dw.devCommand
            // Pane titles change without the pane set changing (the daemon re-derives
            // them from tmux as programs start/stop) — fold them into the tab chips.
            if let controller = controllers[appId],
               let retitled = RemoteWorkspaceBuilder.updatingTitles(controller.tree, panes: dw.panes) {
                controller.tree = retitled
            }
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

    /// Apply a daemon-side pane change (dev-server start/stop, teammate spawn,
    /// another lens) to an already-attached workspace without tearing it down:
    /// merge the live tree, sync the attachment's per-pane controllers, and advance
    /// the signature. The attach socket, monitors, and unrelated panes never blink,
    /// and sub-debounce local edits survive (the live tree beats the daemon blob).
    /// The layout observer then pushes the merged arrangement like any local edit.
    /// Nil (→ caller rebuilds from scratch) when the workspace isn't fully live
    /// here or the merge leaves no tree.
    private func patchWorkspace(_ dw: DaemonWorkspace, appId: UUID) -> Workspace? {
        guard var existing = workspaces.first(where: { $0.id == appId }),
              let controller = controllers[appId],
              let attachment = attachments[appId],
              let merged = RemoteWorkspaceBuilder.mergedTree(controller.tree, panes: dw.panes, repoPath: dw.repoPath)
        else { return nil }
        controller.tree = merged
        if let focused = controller.focusedPaneId, merged.findLeaf(id: focused) == nil {
            controller.focusedPaneId = merged.allLeaves.first?.id
        }
        stalePanes.subtract(attachment.syncPanes(dw.panes))
        paneSignatures[appId] = RemoteWorkspaceBuilder.paneSignature(dw.panes)
        existing.layout = merged
        existing.focusedPaneId = controller.focusedPaneId
        return existing
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
        // File explorers in this workspace read/write through the daemon — the
        // repo lives on its host, not this Mac.
        controller.fileSource = DaemonFileSource(daemonId: dw.id)
        controller.onHostedTerminalRequest = { [weak self] leafId, direction in
            Task { await self?.placeSpawnedTerminal(appId: appId, leafId: leafId, direction: direction) }
        }
        controller.onHostedPaneClosed = { [weak self] paneId in
            Task { await self?.killHostedPane(appId: appId, paneId: paneId) }
        }
        controllers[appId] = controller
        daemonIds[appId] = dw.id
        paneSignatures[appId] = RemoteWorkspaceBuilder.paneSignature(dw.panes)
        observeLayout(appId: appId, controller: controller, tree: tree, version: dw.layoutVersion ?? 0)

        let attachment = WorkspaceAttachment(
            workspaceId: appId, daemonId: dw.id, repoPath: dw.repoPath, panes: dw.panes,
            wsOrigin: attachOrigin(for: dw))
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
        // Removing first makes the ordering the detach depends on visible: this is
        // the last reference to the attachment, and therefore to its terminal views.
        if let attachment = attachments.removeValue(forKey: appId) {
            attachment.disconnect()
            attachment.detachAllTerminals()
        }
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
        owners.removeValue(forKey: appId)
        hostLabels.removeValue(forKey: appId)
        hostnames.removeValue(forKey: appId)
        devRunning.removeValue(forKey: appId)
        devCommands.removeValue(forKey: appId)
        workspaces.removeAll { $0.id == appId }
    }

    /// Open a clicked file link in the hosted workspace's file explorer,
    /// creating the explorer tab if the workspace has none. `path` is the
    /// remote absolute path the terminal link resolved to; the daemon's file
    /// routes accept it as long as it stays inside the repo root.
    func revealFile(_ appId: UUID, path: String) {
        controllers[appId]?.revealFileInExplorer(relativePath: path)
    }

    // MARK: - Shared sidebar group

    /// Push a hosted workspace's sidebar group to the daemon (the owning window's
    /// name — this Mac is the source of truth; web/phone render the same groups).
    /// The local cache updates optimistically so the diff-based sync stays quiet
    /// until the next reconcile confirms.
    @discardableResult
    func setGroup(_ appId: UUID, to name: String) async -> Bool {
        guard let daemonId = daemonIds[appId] else { return false }
        // Delegates: workspaceUUID(daemonId) inverts daemonIds, so the cache
        // write in the daemon-id variant lands on the same key.
        return await setGroup(daemonId: daemonId, to: name)
    }

    /// setGroup by raw daemon id — cold sessions aren't materialized as app
    /// workspaces, and the revive-claim path must place one BEFORE reviving it
    /// so it lands in the window the user clicked in. A SHARED edit: the
    /// assignment moves the session for everyone. Returns success.
    @discardableResult
    func setGroup(daemonId: String, to name: String) async -> Bool {
        let body = try? JSONSerialization.data(withJSONObject: ["group": name])
        guard await send("PUT", path: "/v1/workspaces/\(daemonId)/group", body: body, expect: 204) else {
            return false
        }
        await MainActor.run { self.groups[RemoteWorkspaceBuilder.workspaceUUID(daemonId)] = name }
        return true
    }

    /// Optimistic local note of a group change, so the reconcile that runs
    /// between a drag and the daemon's ack does not bounce the workspace back.
    func noteGroup(_ appId: UUID, to name: String) {
        groups[appId] = name
    }

    // MARK: - Shared windows (v2)

    /// Mark the shared window open for this login. Idempotent on the daemon.
    func openSharedWindow(id: String) async {
        if !(await send("POST", path: "/v1/windows/\(id)/open", body: Data(), expect: 200)) {
            NSLog("[ccmux] windows: open flag for %@ failed; the list will show it closed until the next open", id)
        }
        await refresh()
    }

    /// Clear this login's open flag on the shared window named `name`; when
    /// that made it nobody's, the window goes to sleep — archive the members
    /// the daemon reported (force: nobody has it open, which is the model's
    /// own permission).
    func closeSharedWindow(named name: String) async {
        guard let win = sharedWindows.first(where: { $0.name.caseInsensitiveCompare(name) == .orderedSame }) else {
            NSLog("[ccmux] windows: no shared window named %@ to close", name)
            return
        }
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/windows/\(win.id)/close") else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        do {
            let (data, resp) = try await session.data(for: req)
            guard (resp as? HTTPURLResponse)?.statusCode == 200 else {
                NSLog("[ccmux] windows: close of %@ failed (HTTP %d); it stays open on the daemon",
                      name, (resp as? HTTPURLResponse)?.statusCode ?? -1)
                return
            }
            struct CloseResp: Codable {
                var last: Bool
                var members: [String]?
            }
            let out = try JSONDecoder().decode(CloseResp.self, from: data)
            if out.last {
                for daemonId in out.members ?? [] {
                    if let error = await sendReportingError(
                        "POST", path: "/v1/workspaces/\(daemonId)/archive?force=1", body: [:], expect: 200) {
                        NSLog("[ccmux] windows: sleeping %@ failed (left running): %@", daemonId, error)
                    }
                }
            }
        } catch {
            NSLog("[ccmux] windows: close of %@ failed: %@", name, error.localizedDescription)
            return
        }
        await refresh()
    }

    /// Rename the shared window (everyone sees it). Returns nil on success or
    /// the daemon's error text (a case-insensitive name collision).
    func renameSharedWindow(id: String, to name: String) async -> String? {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/windows/\(id)") else { return "bad daemon URL" }
        var req = URLRequest(url: url)
        req.httpMethod = "PUT"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: ["name": name])
        do {
            let (data, resp) = try await session.data(for: req)
            guard let code = (resp as? HTTPURLResponse)?.statusCode else { return "no response" }
            if code == 204 {
                await refresh()
                return nil
            }
            return String(data: data, encoding: .utf8) ?? "HTTP \(code)"
        } catch {
            return error.localizedDescription
        }
    }

    // MARK: - Dev hostnames

    /// Replace a hosted workspace's dev-hostname mappings (the Hostnames…
    /// sheet's Save). Returns nil on success or the daemon's error text
    /// (invalid label, name taken by another workspace) for the sheet to show.
    func setHostnames(_ appId: UUID, hostnames: [DaemonHostname], devCommand: String? = nil) async -> String? {
        guard let daemonId = daemonIds[appId] else { return "workspace is not hosted" }
        var body: [String: Any] = [
            "hostnames": hostnames.map { ["name": $0.name, "port": $0.port] }
        ]
        if let devCommand { body["devCommand"] = devCommand }
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/hostnames") else {
            return "bad daemon URL"
        }
        var req = URLRequest(url: url)
        req.httpMethod = "PUT"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: body)
        do {
            let (data, resp) = try await session.data(for: req)
            guard let code = (resp as? HTTPURLResponse)?.statusCode else { return "no response" }
            if code == 200 {
                let dw = try? JSONDecoder().decode(DaemonWorkspace.self, from: data)
                await MainActor.run {
                    self.hostnames[appId] = dw?.hostnames ?? hostnames
                    if let devCommand { self.devCommands[appId] = devCommand }
                }
                return nil
            }
            struct APIError: Decodable { let error: String }
            return (try? JSONDecoder().decode(APIError.self, from: data))?.error ?? "HTTP \(code)"
        } catch {
            return error.localizedDescription
        }
    }

    /// Detected suggestions for the Hostnames sheet — port rows plus the
    /// resolved dev command (nil payload on any failure; the sheet starts blank).
    func fetchPortSuggestions(_ appId: UUID) async -> DaemonSuggestionsResponse? {
        guard let daemonId = daemonIds[appId],
              let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/port-suggestions"),
              let (data, resp) = try? await session.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
        return try? JSONDecoder().decode(DaemonSuggestionsResponse.self, from: data)
    }

    /// Start/stop the workspace's dev-server pane (the ▶/■ on its hostname
    /// row). The daemon spawns/kills a tmux pane running the resolved command —
    /// the pane itself is the log view. Returns the daemon's error text or nil.
    @discardableResult
    func setDevServer(_ appId: UUID, running: Bool) async -> String? {
        guard let daemonId = daemonIds[appId],
              let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/dev-server") else {
            return "workspace is not hosted"
        }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: ["action": running ? "start" : "stop"])
        do {
            let (data, resp) = try await session.data(for: req)
            guard let code = (resp as? HTTPURLResponse)?.statusCode else { return "no response" }
            if code == 200 {
                await MainActor.run { self.devRunning[appId] = running }
                await refresh() // the new/removed pane changes the workspace tree
                return nil
            }
            struct APIError: Decodable { let error: String }
            let message = (try? JSONDecoder().decode(APIError.self, from: data))?.error ?? "HTTP \(code)"
            await MainActor.run { self.lastError = message }
            return message
        } catch {
            return error.localizedDescription
        }
    }

    // MARK: - HTTP helpers

    private func post(_ path: String, body: [String: Any], expect: Int) async -> Bool {
        let data = try? JSONSerialization.data(withJSONObject: body)
        return await send("POST", path: path, body: data, expect: expect)
    }

    /// Like `send`, but returns the daemon's `{"error": …}` text instead of a bare
    /// false. `send` throws the body away, which is how a refused request ends up
    /// looking like nothing happened at all.
    private func sendReportingError(
        _ method: String, path: String, body: [String: Any]?, expect: Int
    ) async -> String? {
        guard let url = URL(string: "\(DaemonConfig.baseURL)\(path)") else { return "bad daemon URL" }
        var req = URLRequest(url: url)
        req.httpMethod = method
        if let body {
            // Sending the request anyway would ship an empty body under a JSON
            // content type, and the daemon's rejection would blame the daemon for a
            // request this app built wrong.
            guard let payload = try? JSONSerialization.data(withJSONObject: body) else {
                return "could not encode the request for \(path)"
            }
            req.httpBody = payload
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        do {
            let (data, resp) = try await session.data(for: req)
            guard let code = (resp as? HTTPURLResponse)?.statusCode else { return "no response" }
            if code == expect { return nil }
            struct APIError: Decodable { let error: String }
            let message = (try? JSONDecoder().decode(APIError.self, from: data))?.error ?? "HTTP \(code)"
            await MainActor.run { self.lastError = message }
            return message
        } catch {
            await MainActor.run { self.lastError = error.localizedDescription }
            return error.localizedDescription
        }
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

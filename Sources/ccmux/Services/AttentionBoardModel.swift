import Foundation
import Combine
import AppKit

/// This viewer's "not now" marks, shared by every window's board and its
/// sidebar row for the life of the app. In memory only, like the web lens's.
final class BoardSnoozes: ObservableObject {
    static let shared = BoardSnoozes()
    @Published private(set) var byPane: [String: BoardSnooze] = [:]

    func snooze(_ c: BoardClaim) {
        byPane[c.pane] = BoardSnooze(since: c.since, until: Date().addingTimeInterval(AttentionBoard.snoozeFor))
    }
}

/// What a passive tile shows: the pane's screen as text, or an agent's last
/// word and the card it is waiting on.
struct BoardPreview: Equatable {
    /// The claim this was read for; an agent is re-read only when it changes.
    var key = ""
    var text: String
    /// The pane's size for a screen picture (scaled to fit), 0 for an agent.
    var cols: Int
    var rows = 0
}

/// One window's attention board: which tiles are on show, which one is
/// active, and the previews. The rules themselves are AttentionBoard's.
@MainActor
final class AttentionBoardModel: ObservableObject {
    @Published private(set) var visible: [BoardClaim] = []
    @Published private(set) var waiting: [BoardClaim] = []
    @Published private(set) var active: String?
    @Published private(set) var previews: [String: BoardPreview] = [:]
    /// Moves the "waited" labels on; ticks over every half minute.
    @Published private(set) var clock = Date()
    var width: Double = 1200
    var members: Set<UUID> = []
    /// Where the claims come from and where "acted" goes. The service in the
    /// app; a test swaps both to drive the board without a daemon.
    var claimsFor: (Set<UUID>) -> [BoardClaim] = { RemoteSessionService.shared.boardClaims(members: $0) }
    var actedSink: (String) -> Void = { RemoteSessionService.shared.boardActed(paneId: $0) }
    /// Where a screen tile's pane size goes, and how it went. A test swaps
    /// it, and the cell, to check the rule without a daemon.
    var resizeSink: (String, BoardGrid, @escaping (BoardSizeOutcome) -> Void) -> Void = { pane, grid, done in
        Task { done(await BoardPaneSizer.resize(paneId: pane, grid: grid)) }
    }
    var cell = BoardPaneSizer.cell

    private let service = RemoteSessionService.shared
    private var timer: Timer?
    private var subscriptions: Set<AnyCancellable> = []
    private var fetching: Set<String> = []
    private var tabIds: [String: UUID] = [:]
    /// The size each screen tile last sent its pane.
    private var sized: [String: BoardGrid] = [:]
    /// Each tile's last reported text area, so a size that did not take can
    /// be sent again on a tick, not only when the tile changes size.
    private var areas: [String: (width: Double, height: Double)] = [:]

    func start() {
        guard timer == nil else { return }
        // receive(on:) because @Published announces BEFORE the value lands: a
        // refresh run inline would read the old claims.
        service.claims.$byWorkspace.receive(on: DispatchQueue.main)
            .sink { [weak self] _ in MainActor.assumeIsolated { self?.refresh() } }.store(in: &subscriptions)
        BoardSnoozes.shared.$byPane.receive(on: DispatchQueue.main)
            .sink { [weak self] _ in MainActor.assumeIsolated { self?.refresh() } }.store(in: &subscriptions)
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            MainActor.assumeIsolated { self?.tick() }
        }
        refresh()
        fetchPreviews()
    }

    /// Leaving the board while in a tile is moving on from it.
    func stop() {
        timer?.invalidate()
        timer = nil
        subscriptions.removeAll()
        sized.removeAll()
        areas.removeAll()
        deactivate(acted: true)
    }

    func refresh() {
        let out = AttentionBoard.layout(claimsFor(members), cap: AttentionBoard.cap(forWidth: width),
                                        active: active, previous: visible,
                                        snoozes: BoardSnoozes.shared.byPane, now: Date())
        if out.visible != visible { visible = out.visible }
        if out.waiting != waiting { waiting = out.waiting }
        forgetSizes()
    }

    // MARK: - Active tile

    /// Opens a tile. The tile left behind is moved on from, but the grid is
    /// not re-sorted in between: the set stays frozen, the old tile dims as
    /// handled, and the clicked one is found where it was, handled or not.
    /// Same order of steps as the web lens's activate.
    func activate(_ pane: String) {
        guard active != pane else { return }
        endActive(acted: true)
        guard let c = visible.first(where: { $0.pane == pane }) ?? waiting.first(where: { $0.pane == pane }) else { return }
        if !visible.contains(where: { $0.pane == pane }) { visible.insert(c, at: 0) }
        active = pane
        guard !c.chat else { return } // a chat focuses its own box
        // Ready to type: the live terminal takes the keyboard once embedded.
        Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: 300_000_000)
            guard let self, self.active == pane,
                  let view = self.service.hostedController(paneId: pane, workingDirectory: c.workingDirectory)?.terminalView
            else { return }
            view.window?.makeFirstResponder(view)
        }
    }

    /// Ends the active tile. acted says the person dealt with it: the daemon
    /// retires that pane's claim, on every lens.
    func deactivate(acted: Bool) {
        endActive(acted: acted)
        refresh()
    }

    private func endActive(acted: Bool) {
        guard let pane = active else { return }
        active = nil
        if acted { actedSink(pane) }
    }

    /// Moves on: the active tile is done with, and the next one (if any)
    /// becomes active.
    func next() {
        let done = active
        deactivate(acted: true)
        if let c = visible.first(where: { $0.pane != done && !$0.handled }) { activate(c.pane) }
    }

    func snooze(_ c: BoardClaim) {
        BoardSnoozes.shared.snooze(c)
        if active == c.pane { deactivate(acted: false) } else { refresh() }
    }

    var activeIsChat: Bool {
        visible.first(where: { $0.pane == active })?.chat ?? false
    }

    /// A stable tab id per pane for the live view (PaneFocusCoordinator's key).
    func tabId(for pane: String) -> UUID {
        if let id = tabIds[pane] { return id }
        let id = UUID()
        tabIds[pane] = id
        return id
    }

    // MARK: - Sizing

    /// A screen tile's text area showed or changed size: size its pane to it
    /// at the board's text size, as a lens sizes a pane to its screen. Once
    /// per tile size, not every tick, so if another lens takes the pane over
    /// the tile scales its text down rather than fight back. Leaving the board
    /// gives the pane back: a workspace re-asserts its own size when it shows
    /// the pane. A send that failed is forgotten, so the next tick tries again
    /// (retrySizing); one the daemon refused stands (sizeOutcome). Same rule
    /// as the web lens's sizePane.
    func tileArea(_ pane: String, width: Double, height: Double) {
        areas[pane] = (width, height)
        guard pane != active, let c = visible.first(where: { $0.pane == pane }), !c.chat,
              let grid = AttentionBoard.grid(width: width, height: height, cell: cell),
              sized[pane] != grid else { return }
        sized[pane] = grid
        resizeSink(pane, grid) { [weak self] outcome in
            Task { @MainActor in self?.resized(pane, grid, outcome) }
        }
    }

    /// Sized: the pane redraws at its new size, so read it again once it has,
    /// rather than show the old picture until the next tick.
    private func resized(_ pane: String, _ grid: BoardGrid, _ outcome: BoardSizeOutcome) {
        switch outcome {
        case .sized:
            Task { @MainActor [weak self] in
                try? await Task.sleep(nanoseconds: 400_000_000)
                self?.fetchPreviews()
            }
        case .failed:
            if sized[pane] == grid { sized[pane] = nil }
        case .refused:
            break
        }
    }

    /// A tile that left the board forgets its size and area, so a new claim on
    /// its pane sizes it again, whatever another lens did with it meanwhile.
    /// Same rule as the web lens's forgetSizes.
    private func forgetSizes() {
        let shown = Set(visible.map(\.pane))
        sized = sized.filter { shown.contains($0.key) }
        areas = areas.filter { shown.contains($0.key) }
    }

    /// Tiles whose last size did not take, sized again. Run every tick, as
    /// the web lens re-runs sizePane on every paint.
    func retrySizing() {
        for (pane, area) in areas where sized[pane] == nil {
            tileArea(pane, width: area.width, height: area.height)
        }
    }

    // MARK: - Previews

    private func tick() {
        refresh()
        retrySizing()
        fetchPreviews()
        if Date().timeIntervalSince(clock) >= 30 { clock = Date() }
    }

    /// A terminal tile re-reads its screen every few seconds (a capture is
    /// cheap). An agent tile reads its last turns once per claim: that read
    /// can wake a node process on the daemon for an asleep agent.
    private func fetchPreviews() {
        for c in visible where c.pane != active && !c.handled && !fetching.contains(c.pane) {
            let key = "\(c.state.rawValue)\(c.since)"
            if c.chat, previews[c.pane]?.key == key { continue }
            fetching.insert(c.pane)
            Task { [weak self] in
                let read = await BoardPreviewReader.read(paneId: c.pane, chat: c.chat)
                self?.fetching.remove(c.pane)
                guard var preview = read else { return }
                preview.key = key
                if self?.previews[c.pane] != preview { self?.previews[c.pane] = preview }
            }
        }
    }
}

/// The REST reads behind a passive tile: GET /v1/panes/{id}/snapshot?plain=1
/// for a terminal, GET /v1/panes/{id}/agent?tail=4 for an agent.
enum BoardPreviewReader {
    static func read(paneId: String, chat: Bool) async -> BoardPreview? {
        let path = chat ? "/v1/panes/\(paneId)/agent?tail=4" : "/v1/panes/\(paneId)/snapshot?plain=1"
        guard let url = URL(string: DaemonConfig.baseURL + path),
              let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200 else { return nil }
        return chat ? agentPreview(data) : screenPreview(data)
    }

    static func screenPreview(_ data: Data) -> BoardPreview? {
        struct Snapshot: Decodable { let data: String; let cols: Int?; let rows: Int? }
        guard let snap = try? JSONDecoder().decode(Snapshot.self, from: data),
              let raw = Data(base64Encoded: snap.data) else { return nil }
        let text = String(decoding: raw, as: UTF8.self)
            .replacingOccurrences(of: "\\s+$", with: "", options: .regularExpression)
        return BoardPreview(text: text, cols: snap.cols ?? 80, rows: snap.rows ?? 0)
    }

    /// The agent's last word (its tail, as the web lens shows it) and the
    /// card it is waiting on.
    static func agentPreview(_ data: Data) -> BoardPreview? {
        guard let hello = try? JSONDecoder().decode(AgentChatFrame.self, from: data) else { return nil }
        let last = (hello.turns ?? []).last { $0.role == "assistant" }
        let said = (last?.parts ?? []).filter { $0.type == "text" }.compactMap(\.text).joined(separator: "\n")
        var text = said.isEmpty ? "(no reply yet)" : String(said.suffix(800))
        let perm = hello.permissions?.first
        if let perm {
            text += "\n\n? \(perm.permission): \((perm.patterns ?? []).joined(separator: " "))"
        } else if let ask = hello.questions?.first {
            text += "\n\n? " + ask.questions.map(\.question).joined(separator: " / ")
        }
        return BoardPreview(text: text.trimmingCharacters(in: .whitespacesAndNewlines), cols: 0)
    }
}

/// Sizes a screen tile's pane: POST /v1/panes/{id}/resize, the same resize a
/// lens sends over its attach socket, for a tile that has none.
enum BoardPaneSizer {
    /// The terminal's font (RemoteTermController), so a tile reads like the
    /// pane it opens into.
    static let fontName = "Monaco"

    /// One character at the board's text size, in the font BoardPreviewView
    /// draws a screen with.
    static let cell: BoardCell = {
        let mono = NSFont(name: BoardPaneSizer.fontName, size: AttentionBoard.tileFont)
            ?? NSFont.monospacedSystemFont(ofSize: AttentionBoard.tileFont, weight: .regular)
        let width = ("M" as NSString).size(withAttributes: [.font: mono]).width
        return BoardCell(width: Double(width), height: Double(NSLayoutManager().defaultLineHeight(for: mono)))
    }()

    static func resize(paneId: String, grid: BoardGrid) async -> BoardSizeOutcome {
        guard let url = URL(string: DaemonConfig.baseURL + "/v1/panes/\(paneId)/resize") else { return .refused }
        var req = URLRequest(url: url)
        req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try? JSONSerialization.data(withJSONObject: ["cols": grid.cols, "rows": grid.rows])
        let reply: (Data, URLResponse)
        do {
            reply = try await URLSession.shared.data(for: req)
        } catch {
            NSLog("[ccmux board] could not size pane %@ to %dx%d: %@; its tile scales instead",
                  paneId, grid.cols, grid.rows, error.localizedDescription)
            return .failed
        }
        let status = (reply.1 as? HTTPURLResponse)?.statusCode ?? 0
        let outcome = AttentionBoard.sizeOutcome(status: status)
        if outcome != .sized {
            // The daemon's {"error": …} says which: an unknown pane, a host to upgrade, tmux.
            NSLog("[ccmux board] could not size pane %@ to %dx%d: HTTP %d %@; its tile scales instead",
                  paneId, grid.cols, grid.rows, status, String(decoding: reply.0.prefix(200), as: UTF8.self))
        }
        return outcome
    }
}

extension RemoteSessionService {
    /// Every claiming pane of the given (window's) hosted workspaces, as board
    /// tiles. Only panes the workspace still lists count: prunedAttention
    /// keeps the store to those, as the web lens's claimsIn does.
    func boardClaims(members: Set<UUID>) -> [BoardClaim] {
        var out: [BoardClaim] = []
        for ws in workspaces where members.contains(ws.id) {
            guard let daemonId = daemonId(forApp: ws.id), let panes = claims.byWorkspace[daemonId] else { continue }
            for entry in panes.values where entry.state.appAttentionState != .none {
                let ref = agentPanes[entry.pane]
                out.append(BoardClaim(
                    pane: entry.pane, state: entry.state, reason: entry.reason, since: entry.since, name: ws.name,
                    chat: ref.map { AgentPaneRef.hasChat($0.harness) } ?? false, workingDirectory: ws.repoPath))
            }
        }
        return out
    }
}

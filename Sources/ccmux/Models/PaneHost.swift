import Foundation

/// Where a terminal pane's process actually lives.
///
/// The driver→lens pivot introduces **hosted** panes: the process runs inside a
/// tmux session on the ccmuxd host, and this app merely attaches over WebSocket.
/// `.local` is the original driver behavior (a `LocalProcessTerminalView` owning a
/// child shell). The default is `.local` so every existing persisted `TerminalConfig`
/// — which predates this field — decodes back to the untouched local path.
enum PaneHost: Codable, Equatable {
    /// A locally-spawned child process (SwiftTerm `LocalProcessTerminalView`).
    case local
    /// A pane backed by a ccmuxd/tmux window; `paneId` is the daemon's stable pane id
    /// (`@ccmux_pane_id`), used as the `pane` field in attach WS frames.
    case hosted(paneId: String)

    var isHosted: Bool {
        if case .hosted = self { return true }
        return false
    }

    /// The daemon pane id when hosted, else nil.
    var hostedPaneId: String? {
        if case .hosted(let id) = self { return id }
        return nil
    }

    // MARK: - Codable (tagged union: {"kind":"local"} | {"kind":"hosted","paneId":"…"})

    private enum CodingKeys: String, CodingKey { case kind, paneId }
    private enum Kind: String, Codable { case local, hosted }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        switch try c.decode(Kind.self, forKey: .kind) {
        case .local:
            self = .local
        case .hosted:
            self = .hosted(paneId: try c.decode(String.self, forKey: .paneId))
        }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .local:
            try c.encode(Kind.local, forKey: .kind)
        case .hosted(let paneId):
            try c.encode(Kind.hosted, forKey: .kind)
            try c.encode(paneId, forKey: .paneId)
        }
    }
}

/// Whether a workspace's panes live locally (driver) or on the ccmuxd host (lens).
/// `.local` default keeps older `state.json` files decoding unchanged.
enum WorkspaceMode: String, Codable {
    case local
    case hosted
}

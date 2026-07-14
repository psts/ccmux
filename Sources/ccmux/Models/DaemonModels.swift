import Foundation

// Wire types mirroring ccmuxd's REST + WebSocket contract
// (daemon/internal/{model,api}). The daemon is the source of truth; these are the
// lens-side decoders. Kept deliberately tolerant (decodeIfPresent everywhere but
// `id`) so daemon field additions don't break an older native lens.

/// Lifecycle of a daemon workspace/pane. `live` = backing tmux exists; `cold` =
/// only the registry row survives (revivable).
enum DaemonStatus: String, Codable {
    case live
    case cold
    case unknown

    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = DaemonStatus(rawValue: raw) ?? .unknown
    }
}

/// A pane's activity state as computed by the daemon (Claude hooks + %output flow).
enum DaemonAttention: String, Codable {
    case running
    case idle
    case needsInput = "needs_input"
    case done
    case unknown

    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = DaemonAttention(rawValue: raw) ?? .unknown
    }

    /// Map the daemon's four-state activity onto the app's sidebar-flash signal.
    /// Only the blocking (`needsInput`) and turn-finished (`done`) states surface;
    /// running/idle are ambient and clear the flash.
    var appAttentionState: AttentionState {
        switch self {
        case .needsInput: return .needsInput
        case .done: return .done
        case .running, .idle, .unknown: return .none
        }
    }
}

/// A daemon workspace = one tmux session, N panes. GET /v1/workspaces returns these.
struct DaemonWorkspace: Codable, Identifiable {
    let id: String
    var name: String
    var repoPath: String
    var status: DaemonStatus
    var createdBy: String?
    var createdAt: Int64?
    var tmuxSession: String?
    var layoutJson: String?
    var layoutVersion: Int?
    var panes: [DaemonPane]

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        repoPath = try c.decodeIfPresent(String.self, forKey: .repoPath) ?? ""
        status = try c.decodeIfPresent(DaemonStatus.self, forKey: .status) ?? .unknown
        createdBy = try c.decodeIfPresent(String.self, forKey: .createdBy)
        createdAt = try c.decodeIfPresent(Int64.self, forKey: .createdAt)
        tmuxSession = try c.decodeIfPresent(String.self, forKey: .tmuxSession)
        layoutJson = try c.decodeIfPresent(String.self, forKey: .layoutJson)
        layoutVersion = try c.decodeIfPresent(Int.self, forKey: .layoutVersion)
        // Go marshals a nil slice as `null`, so tolerate both null and absent.
        panes = try c.decodeIfPresent([DaemonPane].self, forKey: .panes) ?? []
    }

    var isLive: Bool { status == .live }
}

/// A daemon pane = one tmux window (single tmux pane). `id` is the stable
/// `@ccmux_pane_id` used as the `pane` field in attach WS frames.
struct DaemonPane: Codable, Identifiable {
    let id: String
    var title: String
    var cwd: String
    var status: DaemonStatus?
    var attention: DaemonAttention?
    var startupCommand: String?
    var workspaceId: String?

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        cwd = try c.decodeIfPresent(String.self, forKey: .cwd) ?? ""
        status = try c.decodeIfPresent(DaemonStatus.self, forKey: .status)
        attention = try c.decodeIfPresent(DaemonAttention.self, forKey: .attention)
        startupCommand = try c.decodeIfPresent(String.self, forKey: .startupCommand)
        workspaceId = try c.decodeIfPresent(String.self, forKey: .workspaceId)
    }
}

/// One attached lens, for presence display (mirrors api.ClientInfo).
struct DaemonClient: Codable, Identifiable {
    let id: String
    var user: String
    var device: String?
    var focused: String?
    var readonly: Bool
    var driving: Bool
    var verified: Bool

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        user = try c.decodeIfPresent(String.self, forKey: .user) ?? "anon"
        device = try c.decodeIfPresent(String.self, forKey: .device)
        focused = try c.decodeIfPresent(String.self, forKey: .focused)
        readonly = try c.decodeIfPresent(Bool.self, forKey: .readonly) ?? false
        driving = try c.decodeIfPresent(Bool.self, forKey: .driving) ?? false
        verified = try c.decodeIfPresent(Bool.self, forKey: .verified) ?? false
    }
}

// MARK: - Attach WebSocket frames

/// The single JSON envelope for all attach traffic (api.wsMsg). Bytes travel
/// base64-encoded in `data`.
struct DaemonWSFrame: Decodable {
    let t: String
    let pane: String?
    let data: String?
    let state: DaemonAttention?
    let cols: Int?
    let rows: Int?
    let panes: [DaemonPaneInfo]?
    let clients: [DaemonClient]?
}

/// Pane summary carried in the `hello` frame.
struct DaemonPaneInfo: Decodable {
    let id: String
    let title: String
    let cwd: String
    let attention: DaemonAttention?

    // Explicit keys: a Decodable-only type with a custom init(from:) doesn't get
    // CodingKeys synthesized (the compiler only synthesizes them alongside a
    // synthesized init/encode).
    private enum CodingKeys: String, CodingKey { case id, title, cwd, attention }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        cwd = try c.decodeIfPresent(String.self, forKey: .cwd) ?? ""
        attention = try c.decodeIfPresent(DaemonAttention.self, forKey: .attention)
    }
}

/// Typed, decoded attach event handed to consumers — base64 already resolved to bytes.
enum DaemonEvent {
    case hello(panes: [DaemonPaneInfo])
    case snapshot(pane: String, bytes: [UInt8])
    case output(pane: String, bytes: [UInt8])
    case attention(pane: String, state: DaemonAttention)
    case presence(clients: [DaemonClient])
    case paneAdded(pane: String)
    case paneClosed(pane: String)
    case unknown(String)

    /// Pure mapping from a wire frame to a typed event (the testable codec seam).
    init(frame: DaemonWSFrame) {
        switch frame.t {
        case "hello":
            self = .hello(panes: frame.panes ?? [])
        case "snapshot":
            self = .snapshot(pane: frame.pane ?? "", bytes: Self.decodeBytes(frame.data))
        case "output":
            self = .output(pane: frame.pane ?? "", bytes: Self.decodeBytes(frame.data))
        case "attention":
            self = .attention(pane: frame.pane ?? "", state: frame.state ?? .unknown)
        case "presence":
            self = .presence(clients: frame.clients ?? [])
        case "pane-added":
            self = .paneAdded(pane: frame.pane ?? "")
        case "pane-closed":
            self = .paneClosed(pane: frame.pane ?? "")
        default:
            self = .unknown(frame.t)
        }
    }

    private static func decodeBytes(_ b64: String?) -> [UInt8] {
        guard let b64, let data = Data(base64Encoded: b64) else { return [] }
        return [UInt8](data)
    }

    /// Decode a raw text frame straight to a typed event; nil if the JSON is malformed.
    static func decode(text: String) -> DaemonEvent? {
        guard let data = text.data(using: .utf8),
              let frame = try? JSONDecoder().decode(DaemonWSFrame.self, from: data)
        else { return nil }
        return DaemonEvent(frame: frame)
    }
}

// MARK: - Firehose (/v1/events) frames

/// The JSON envelope for global-firehose frames (api.firehoseMsg). Unlike the
/// attach envelope it carries no pane bytes — only workspace-scoped attention, so
/// every frame names the workspace a sidebar row should flash.
struct DaemonFirehoseFrame: Decodable {
    let t: String
    let workspace: String?
    let pane: String?
    let state: DaemonAttention?
    let attention: [DaemonAttentionEntry]?  // hello only
}

/// One pane's current attention in the firehose `hello` snapshot.
struct DaemonAttentionEntry: Decodable {
    let workspace: String
    let pane: String
    let state: DaemonAttention

    private enum CodingKeys: String, CodingKey { case workspace, pane, state }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        workspace = try c.decodeIfPresent(String.self, forKey: .workspace) ?? ""
        pane = try c.decodeIfPresent(String.self, forKey: .pane) ?? ""
        state = try c.decodeIfPresent(DaemonAttention.self, forKey: .state) ?? .unknown
    }
}

/// Typed, decoded firehose event. `hello` seeds current attention for every live
/// pane; `attention` is a live change. Both name the daemon workspace id so the
/// lens can flash the right sidebar row without being attached to it.
enum DaemonFirehoseEvent {
    case hello(entries: [DaemonAttentionEntry])
    case attention(workspace: String, pane: String, state: DaemonAttention)
    /// A workspace was added/removed or changed live↔cold — the lens re-fetches the
    /// list instead of waiting for its poll.
    case workspaceChanged(kind: String, workspace: String)
    case unknown(String)

    init(frame: DaemonFirehoseFrame) {
        switch frame.t {
        case "hello":
            self = .hello(entries: frame.attention ?? [])
        case "attention":
            self = .attention(workspace: frame.workspace ?? "", pane: frame.pane ?? "", state: frame.state ?? .unknown)
        case "workspace-added", "workspace-removed", "workspace-status":
            self = .workspaceChanged(kind: frame.t, workspace: frame.workspace ?? "")
        default:
            self = .unknown(frame.t)
        }
    }

    /// Decode a raw text frame straight to a typed event; nil if the JSON is malformed.
    static func decode(text: String) -> DaemonFirehoseEvent? {
        guard let data = text.data(using: .utf8),
              let frame = try? JSONDecoder().decode(DaemonFirehoseFrame.self, from: data)
        else { return nil }
        return DaemonFirehoseEvent(frame: frame)
    }
}

/// Client→server attach command. Serializes to the same `wsMsg` envelope.
enum DaemonCommand {
    case input(pane: String, bytes: ArraySlice<UInt8>)
    case resize(pane: String, cols: Int, rows: Int)
    case focus(pane: String)

    func jsonData() -> Data? {
        var obj: [String: Any]
        switch self {
        case .input(let pane, let bytes):
            obj = ["t": "input", "pane": pane, "data": Data(bytes).base64EncodedString()]
        case .resize(let pane, let cols, let rows):
            obj = ["t": "resize", "pane": pane, "cols": cols, "rows": rows]
        case .focus(let pane):
            obj = ["t": "focus", "pane": pane]
        }
        return try? JSONSerialization.data(withJSONObject: obj)
    }
}

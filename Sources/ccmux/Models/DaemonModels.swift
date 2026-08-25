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

/// One member node of the federation (GET /v1/hosts, hub mode only). `id` is the
/// MagicDNS label (matches DaemonWorkspace.host); `addr` is the dialable authority
/// used to attach the terminal stream direct to the owning host.
struct DaemonHost: Decodable, Identifiable {
    let id: String
    let addr: String
    let healthy: Bool
    let compat: String
    let reason: String
    let isSelf: Bool

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        addr = try c.decodeIfPresent(String.self, forKey: .addr) ?? ""
        healthy = try c.decodeIfPresent(Bool.self, forKey: .healthy) ?? false
        compat = try c.decodeIfPresent(String.self, forKey: .compat) ?? "unknown"
        reason = try c.decodeIfPresent(String.self, forKey: .reason) ?? ""
        isSelf = try c.decodeIfPresent(Bool.self, forKey: .isSelf) ?? false
    }

    private enum CodingKeys: String, CodingKey {
        case id, addr, healthy, compat, reason
        case isSelf = "self"
    }
}

/// Daemon-wide lens settings (GET/PUT /v1/settings): the dev-hostname serving
/// config, llm accounts, and the harness registry — the single source of what
/// a pane runs — with its per-folder preselect rules (longest matching
/// pathPrefix wins). Secrets are write-only on the daemon: GET carries only
/// the `…Set` presence flags.
struct DaemonSettings: Codable {
    /// Custom dev domain (e.g. "dev.sanlabs.io"); "" = ts.net fallback mode.
    var devDomain: String
    /// Reserved label serving the ccmux web lens under the dev domain
    /// (e.g. "ccmux" → https://ccmux.dev.sanlabs.io); "" = off.
    var lensHostname: String
    var cloudflareTokenSet: Bool
    var tailscaleAuthKeySet: Bool
    /// Wildcard-cert lifecycle: unset | pending | ready | error: <cause> | unknown.
    var devCertStatus: String
    /// LLM routing: which account answers pane LLM traffic ("" = direct
    /// Anthropic with the user's own login) and the configured accounts
    /// (redacted — key presence only). Absent on daemons without the proxy.
    var llmRoute: String
    var llmAccounts: [DaemonLLMAccount]
    /// Live per-account health from the proxy (limits, usage percentages);
    /// absent on older daemons.
    var llmAccountStatus: [DaemonLLMAccountStatus]
    /// The daemon-resolved harness list (builtin + detected + user entries)
    /// and the per-folder preselect rules.
    var harnesses: [DaemonHarness]
    var harnessRules: [DaemonHarnessRule]
    /// Whether this daemon's settings carried the llm/harness keys at all —
    /// an older daemon omits them and silently DROPS a write that includes
    /// them (unknown JSON keys are ignored, not 400d), so the editors must
    /// not send what the daemon never offered: the save would fake success.
    var supportsLLM: Bool
    var supportsHarnesses: Bool
    var supportsHarnessRules: Bool

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        supportsLLM = c.contains(.llmRoute) || c.contains(.llmAccounts)
        supportsHarnesses = c.contains(.harnesses)
        supportsHarnessRules = c.contains(.harnessRules)
        devDomain = try c.decodeIfPresent(String.self, forKey: .devDomain) ?? ""
        lensHostname = try c.decodeIfPresent(String.self, forKey: .lensHostname) ?? ""
        cloudflareTokenSet = try c.decodeIfPresent(Bool.self, forKey: .cloudflareTokenSet) ?? false
        tailscaleAuthKeySet = try c.decodeIfPresent(Bool.self, forKey: .tailscaleAuthKeySet) ?? false
        devCertStatus = try c.decodeIfPresent(String.self, forKey: .devCertStatus) ?? "unset"
        llmRoute = try c.decodeIfPresent(String.self, forKey: .llmRoute) ?? ""
        llmAccounts = try c.decodeIfPresent([DaemonLLMAccount].self, forKey: .llmAccounts) ?? []
        llmAccountStatus = try c.decodeIfPresent([DaemonLLMAccountStatus].self, forKey: .llmAccountStatus) ?? []
        harnesses = try c.decodeIfPresent([DaemonHarness].self, forKey: .harnesses) ?? []
        harnessRules = try c.decodeIfPresent([DaemonHarnessRule].self, forKey: .harnessRules) ?? []
    }
}

/// Live health for one LLM account, as the proxy learned it from responses:
/// state is "ok" | "limited" | "unauthorized" | "untried"; percentages are
/// -1 until an upstream that reports usage (Anthropic subscriptions) has
/// answered through the proxy.
struct DaemonLLMAccountStatus: Codable, Identifiable {
    let name: String
    var state: String
    var limitedUntil: String?
    var sessionPct: Double
    var weeklyPct: Double
    var sessionReset: String?
    var weeklyReset: String?
    var id: String { name }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        state = try c.decodeIfPresent(String.self, forKey: .state) ?? "untried"
        limitedUntil = try c.decodeIfPresent(String.self, forKey: .limitedUntil)
        sessionPct = try c.decodeIfPresent(Double.self, forKey: .sessionPct) ?? -1
        weeklyPct = try c.decodeIfPresent(Double.self, forKey: .weeklyPct) ?? -1
        sessionReset = try c.decodeIfPresent(String.self, forKey: .sessionReset)
        weeklyReset = try c.decodeIfPresent(String.self, forKey: .weeklyReset)
    }
}

/// One LLM account as GET /v1/settings reports it: key PRESENCE only, never
/// the key. Model aliases round-trip in full (not secrets).
struct DaemonLLMAccount: Codable, Identifiable {
    let name: String
    var kind: String
    var baseURL: String
    var apiKeySet: Bool
    var modelAliases: [DaemonModelAlias]
    var id: String { name }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        kind = try c.decodeIfPresent(String.self, forKey: .kind) ?? "anthropic"
        baseURL = try c.decodeIfPresent(String.self, forKey: .baseURL) ?? ""
        apiKeySet = try c.decodeIfPresent(Bool.self, forKey: .apiKeySet) ?? false
        modelAliases = try c.decodeIfPresent([DaemonModelAlias].self, forKey: .modelAliases) ?? []
    }

    private enum CodingKeys: String, CodingKey { case name, kind, baseURL, apiKeySet, modelAliases }
}

struct DaemonModelAlias: Codable {
    let from: String
    let to: String
}

/// One detected port suggestion for the Hostnames sheet (GET
/// /v1/workspaces/{id}/port-suggestions): name/port prefilled from the repo's
/// compose / package.json / Dockerfile, `source` says which file.
struct DaemonPortSuggestion: Decodable {
    let name: String
    let port: Int
    let source: String
}

/// The full port-suggestions payload: prefill rows plus the resolved dev-server
/// command (stored override or detection) with its provenance.
struct DaemonSuggestionsResponse: Decodable {
    let suggestions: [DaemonPortSuggestion]?
    let devCommand: String?
    let devCommandSource: String?
}

/// One dev-hostname mapping: https://<name>.<dev domain or ts.net suffix> on
/// the tailnet → localhost:<port> on the daemon host. `url`/`listening` are
/// runtime-only, stamped by the daemon's devhost server.
struct DaemonHostname: Codable, Identifiable, Equatable {
    var name: String
    var port: Int
    var url: String?
    var listening: Bool

    var id: String { name }

    init(name: String, port: Int, url: String? = nil, listening: Bool = false) {
        self.name = name
        self.port = port
        self.url = url
        self.listening = listening
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        port = try c.decodeIfPresent(Int.self, forKey: .port) ?? 0
        url = try c.decodeIfPresent(String.self, forKey: .url)
        listening = try c.decodeIfPresent(Bool.self, forKey: .listening) ?? false
    }
}

/// One per-folder preselect rule: workspaces created under pathPrefix suggest
/// this harness on their harness bar. A suggestion only — nothing auto-starts.
struct DaemonHarnessRule: Codable {
    var pathPrefix: String
    var harness: String
}

/// One SHARED window from GET /v1/windows: the same arrangement for everyone;
/// `open` is caller-relative (does THIS login have it open), `openBy` lists
/// everyone who does. See docs/multitenant-plan.md ("v2: shared windows").
struct DaemonWindow: Codable, Identifiable {
    let id: String
    var name: String
    var workspaceIds: [String]
    var openBy: [String]
    var open: Bool

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        workspaceIds = try c.decodeIfPresent([String].self, forKey: .workspaceIds) ?? []
        openBy = try c.decodeIfPresent([String].self, forKey: .openBy) ?? []
        open = try c.decodeIfPresent(Bool.self, forKey: .open) ?? false
    }
}

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
    /// Daemon-computed git dashboard (the repo lives on the daemon's host).
    /// nil until the daemon's first collection (~5s after a workspace goes live).
    var git: DaemonGitStatus?
    /// The SHARED window this session sits in — one arrangement for everyone
    /// (see docs/multitenant-plan.md, "v2: shared windows"). "" = ungrouped:
    /// the sidebar shows it under AVAILABLE.
    var group: String
    /// Whose session this is (the owning host's configured owner login).
    /// "" when the host has no owner set.
    var owner: String
    /// Dev-hostname mappings (right-click → Hostnames…).
    var hostnames: [DaemonHostname]
    /// Dev-server command override ("" = daemon detects from repo config).
    var devCommand: String
    /// Federation: the MagicDNS label of the ccmuxd node this workspace lives on,
    /// stamped by the hub. "" in single-host mode (or the hub's own sessions).
    /// Used to attach the terminal stream direct to the owning host.
    var host: String

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
        git = try c.decodeIfPresent(DaemonGitStatus.self, forKey: .git)
        group = try c.decodeIfPresent(String.self, forKey: .group) ?? ""
        owner = try c.decodeIfPresent(String.self, forKey: .owner) ?? ""
        hostnames = try c.decodeIfPresent([DaemonHostname].self, forKey: .hostnames) ?? []
        devCommand = try c.decodeIfPresent(String.self, forKey: .devCommand) ?? ""
        host = try c.decodeIfPresent(String.self, forKey: .host) ?? ""
    }

    var isLive: Bool { status == .live }
}

/// One changed file in a hosted repo's dashboard (gitstatus.File).
struct DaemonGitFile: Codable {
    let path: String
    let status: String

    private enum CodingKeys: String, CodingKey { case path, status }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        path = try c.decodeIfPresent(String.self, forKey: .path) ?? ""
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? "M"
    }

    var asFileChange: GitStatusInfo.FileChange {
        .init(path: path, status: GitStatusInfo.Status(rawValue: status) ?? .modified)
    }
}

/// The daemon-computed git dashboard for a hosted workspace (gitstatus.Status).
/// Field names mirror `GitStatusInfo` 1:1, so mapping is mechanical — hosted
/// rows render through the exact same dashboard views as local ones.
struct DaemonGitStatus: Codable {
    var isGitRepo = false
    var branch = ""
    var trackingBranch: String?
    var ahead = 0
    var behind = 0
    var defaultBranch: String?
    var aheadOfDefault = 0
    var behindDefault = 0
    var stagedFiles: [DaemonGitFile] = []
    var modifiedFiles: [DaemonGitFile] = []
    var deletedFiles: [DaemonGitFile] = []
    var untrackedFiles: [DaemonGitFile] = []

    private enum CodingKeys: String, CodingKey {
        case isGitRepo, branch, trackingBranch, ahead, behind
        case defaultBranch, aheadOfDefault, behindDefault
        case stagedFiles, modifiedFiles, deletedFiles, untrackedFiles
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        isGitRepo = try c.decodeIfPresent(Bool.self, forKey: .isGitRepo) ?? false
        branch = try c.decodeIfPresent(String.self, forKey: .branch) ?? ""
        trackingBranch = try c.decodeIfPresent(String.self, forKey: .trackingBranch)
        ahead = try c.decodeIfPresent(Int.self, forKey: .ahead) ?? 0
        behind = try c.decodeIfPresent(Int.self, forKey: .behind) ?? 0
        defaultBranch = try c.decodeIfPresent(String.self, forKey: .defaultBranch)
        aheadOfDefault = try c.decodeIfPresent(Int.self, forKey: .aheadOfDefault) ?? 0
        behindDefault = try c.decodeIfPresent(Int.self, forKey: .behindDefault) ?? 0
        stagedFiles = try c.decodeIfPresent([DaemonGitFile].self, forKey: .stagedFiles) ?? []
        modifiedFiles = try c.decodeIfPresent([DaemonGitFile].self, forKey: .modifiedFiles) ?? []
        deletedFiles = try c.decodeIfPresent([DaemonGitFile].self, forKey: .deletedFiles) ?? []
        untrackedFiles = try c.decodeIfPresent([DaemonGitFile].self, forKey: .untrackedFiles) ?? []
    }

    /// Map onto the app's `GitStatusInfo` so the local dashboard renders it as-is.
    var asInfo: GitStatusInfo {
        var info = GitStatusInfo()
        info.isGitRepo = isGitRepo
        info.branch = branch
        info.trackingBranch = trackingBranch
        info.ahead = ahead
        info.behind = behind
        info.defaultBranch = defaultBranch
        info.aheadOfDefault = aheadOfDefault
        info.behindDefault = behindDefault
        info.stagedFiles = stagedFiles.map(\.asFileChange)
        info.modifiedFiles = modifiedFiles.map(\.asFileChange)
        info.deletedFiles = deletedFiles.map(\.asFileChange)
        info.untrackedFiles = untrackedFiles.map(\.asFileChange)
        return info
    }
}

/// A harness the daemon offers: a named way of working in a pane (claude is
/// built in; installed programs like pi/opencode are auto-detected). Read
/// from GET /v1/settings "harnesses"; the daemon resolves the list, this is
/// display + spawn-by-name only.
struct DaemonHarness: Codable, Identifiable {
    let name: String
    var icon: String?
    var command: String?
    /// Whether the startup-prompt autoconfirm watcher is armed for it.
    var autoconfirm: Bool
    /// "builtin" | "detected" | "" (user-configured) — stamped by the daemon.
    var source: String
    /// Which llm account KINDS this harness can use — stamped resolved by the
    /// daemon (an entry that says nothing gets its name's default).
    var accountKinds: [String]
    var id: String { name }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decode(String.self, forKey: .name)
        icon = try c.decodeIfPresent(String.self, forKey: .icon)
        command = try c.decodeIfPresent(String.self, forKey: .command)
        autoconfirm = try c.decodeIfPresent(Bool.self, forKey: .autoconfirm) ?? false
        source = try c.decodeIfPresent(String.self, forKey: .source) ?? ""
        accountKinds = try c.decodeIfPresent([String].self, forKey: .accountKinds) ?? []
    }

    private enum CodingKeys: String, CodingKey { case name, icon, command, autoconfirm, source, accountKinds }
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
    /// True for the workspace's dev-server pane (spawned by ▶).
    var devServer: Bool
    /// True when this pane was started to run a Claude session whose Claude has
    /// since exited, leaving a bare shell. The pane is perfectly alive — which is
    /// exactly why it needs saying: nothing else tells it apart from a working
    /// session, so a dead teammate reads as a live one until you click into it.
    var dormant: Bool
    /// The harness this pane runs ("" for a plain shell) and whether its
    /// foreground is a bare shell right now — together they drive the harness
    /// bar: a shell pane with no harness gets offered "start here".
    var harness: String
    var atShell: Bool

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        cwd = try c.decodeIfPresent(String.self, forKey: .cwd) ?? ""
        status = try c.decodeIfPresent(DaemonStatus.self, forKey: .status)
        attention = try c.decodeIfPresent(DaemonAttention.self, forKey: .attention)
        startupCommand = try c.decodeIfPresent(String.self, forKey: .startupCommand)
        workspaceId = try c.decodeIfPresent(String.self, forKey: .workspaceId)
        devServer = try c.decodeIfPresent(Bool.self, forKey: .devServer) ?? false
        dormant = try c.decodeIfPresent(Bool.self, forKey: .dormant) ?? false
        harness = try c.decodeIfPresent(String.self, forKey: .harness) ?? ""
        atShell = try c.decodeIfPresent(Bool.self, forKey: .atShell) ?? false
    }
}

/// One selectable folder under the daemon's projects root (api.projectEntry).
/// Hosted workspaces are always created from one of these — the folders live on
/// the daemon's filesystem, which may be a remote server.
struct DaemonProject: Decodable, Identifiable, Hashable {
    let name: String
    let path: String
    let git: Bool

    var id: String { path }

    private enum CodingKeys: String, CodingKey { case name, path, git }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        path = try c.decode(String.self, forKey: .path)
        git = try c.decodeIfPresent(Bool.self, forKey: .git) ?? false
    }
}

/// GET /v1/projects response: one browsable folder inside the projects root.
/// `path` is the listed folder relative to the root ("" at the root); `parent`
/// is one level up (nil at the root) — together they drive picker navigation.
struct DaemonProjectList: Decodable {
    let root: String
    let path: String
    let parent: String?
    let projects: [DaemonProject]

    private enum CodingKeys: String, CodingKey { case root, path, parent, projects }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        root = try c.decodeIfPresent(String.self, forKey: .root) ?? ""
        path = try c.decodeIfPresent(String.self, forKey: .path) ?? ""
        parent = try c.decodeIfPresent(String.self, forKey: .parent)
        projects = try c.decodeIfPresent([DaemonProject].self, forKey: .projects) ?? []
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
    let cols: Int   // authoritative tmux width (0 if the daemon omitted it)
    let rows: Int

    // Explicit keys: a Decodable-only type with a custom init(from:) doesn't get
    // CodingKeys synthesized (the compiler only synthesizes them alongside a
    // synthesized init/encode).
    private enum CodingKeys: String, CodingKey { case id, title, cwd, attention, cols, rows }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        title = try c.decodeIfPresent(String.self, forKey: .title) ?? ""
        cwd = try c.decodeIfPresent(String.self, forKey: .cwd) ?? ""
        attention = try c.decodeIfPresent(DaemonAttention.self, forKey: .attention)
        cols = try c.decodeIfPresent(Int.self, forKey: .cols) ?? 0
        rows = try c.decodeIfPresent(Int.self, forKey: .rows) ?? 0
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
    case paneSize(pane: String, cols: Int, rows: Int)
    /// tmux copy-mode copied text in this pane (selection = copy); the lens
    /// writes it to the OS clipboard.
    case clipboard(pane: String, bytes: [UInt8])
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
        case "pane-size":
            self = .paneSize(pane: frame.pane ?? "", cols: frame.cols ?? 0, rows: frame.rows ?? 0)
        case "clipboard":
            self = .clipboard(pane: frame.pane ?? "", bytes: Self.decodeBytes(frame.data))
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
    /// Whether the daemon wants a notification raised for this attention, as
    /// opposed to only a sidebar flash. Absent on older daemons, which is why it
    /// is optional and defaults to false — an old daemon simply never alerts.
    let alert: Bool?
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
    case attention(workspace: String, pane: String, state: DaemonAttention, alert: Bool)
    /// A workspace was added/removed or changed live↔cold — the lens re-fetches the
    /// list instead of waiting for its poll.
    case workspaceChanged(kind: String, workspace: String)
    case unknown(String)

    init(frame: DaemonFirehoseFrame) {
        switch frame.t {
        case "hello":
            self = .hello(entries: frame.attention ?? [])
        case "attention":
            self = .attention(workspace: frame.workspace ?? "", pane: frame.pane ?? "",
                              state: frame.state ?? .unknown, alert: frame.alert ?? false)
        case "workspace-added", "workspace-removed", "workspace-status", "workspace-git":
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
/// Equatable so a test spy can assert the exact command sequence a controller sent.
enum DaemonCommand: Equatable {
    case input(pane: String, bytes: ArraySlice<UInt8>)
    case resize(pane: String, cols: Int, rows: Int)
    /// present says whether THIS screen is awake and unlocked, which is what the
    /// daemon needs to decide who to alert and whose phone to leave alone. It is
    /// a property of the device, not of the pane, and rides the focus frame only
    /// because that is the channel the lens already has.
    case focus(pane: String, present: Bool)
    /// Ask the daemon to re-capture this pane's screen and send it back as a
    /// snapshot, with no resize attached. The daemon only repaints on a resize
    /// that CHANGED the pane, and an activated pane usually re-asserts the size
    /// it already has — so a drifted local emulator would keep its stale screen
    /// forever. Older daemons ignore the frame (unknown `t` falls through their
    /// read switch), which degrades to the pre-repaint behaviour.
    case repaint(pane: String)

    func jsonData() -> Data? {
        var obj: [String: Any]
        switch self {
        case .input(let pane, let bytes):
            obj = ["t": "input", "pane": pane, "data": Data(bytes).base64EncodedString()]
        case .resize(let pane, let cols, let rows):
            obj = ["t": "resize", "pane": pane, "cols": cols, "rows": rows]
        case .focus(let pane, let present):
            obj = ["t": "focus", "pane": pane, "present": present]
        case .repaint(let pane):
            obj = ["t": "repaint", "pane": pane]
        }
        return try? JSONSerialization.data(withJSONObject: obj)
    }
}

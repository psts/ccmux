import Foundation

/// What lives in a leaf pane.
enum PaneContent: Identifiable, Codable {
    case terminal(TerminalConfig)
    case browser(BrowserConfig)
    case editor(EditorConfig)
    case diff(DiffConfig)
    case scratchpad(ScratchpadConfig)
    case fileExplorer(FileExplorerConfig)

    var id: UUID {
        switch self {
        case .terminal(let c): return c.id
        case .browser(let c): return c.id
        case .editor(let c): return c.id
        case .diff(let c): return c.id
        case .scratchpad(let c): return c.id
        case .fileExplorer(let c): return c.id
        }
    }

    var displayName: String {
        switch self {
        case .terminal(let c): return c.title ?? "Terminal"
        case .browser: return "Browser"
        case .editor(let c): return (c.filePath as NSString).lastPathComponent
        case .diff: return "Diff"
        case .scratchpad(let c): return c.title
        case .fileExplorer: return "Files"
        }
    }

    /// True when this tab is a hosted terminal whose Claude has exited.
    var isDormant: Bool {
        if case .terminal(let c) = self { return c.dormant }
        return false
    }

    var iconName: String {
        switch self {
        case .terminal: return "terminal"
        case .browser: return "globe"
        case .editor: return "doc.text"
        case .diff: return "arrow.left.arrow.right"
        case .scratchpad: return "note.text"
        case .fileExplorer: return "folder.fill"
        }
    }

    /// Create a default terminal config for a given working directory.
    static func defaultTerminal(workingDirectory: String) -> PaneContent {
        .terminal(TerminalConfig(
            id: UUID(),
            shell: ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh",
            workingDirectory: workingDirectory
        ))
    }

    /// Create a default file explorer config for a given directory.
    static func defaultFileExplorer(rootPath: String) -> PaneContent {
        .fileExplorer(FileExplorerConfig(
            id: UUID(),
            rootPath: rootPath,
            openFilePaths: [],
            activeFilePath: nil
        ))
    }
}

struct TerminalConfig: Codable, Identifiable {
    let id: UUID
    var shell: String
    var workingDirectory: String
    var title: String?
    var startupCommand: String?
    /// Where this pane's process lives. Default `.local` preserves the original
    /// driver behavior for every persisted config written before the lens pivot.
    var host: PaneHost = .local
    /// Hosted panes only: the pane's Claude has exited and left a shell. Purely
    /// presentational — the pane still works, it just has nobody in it.
    var dormant: Bool = false
}

extension TerminalConfig {
    // Custom decoder so adding `host` doesn't break older state.json files. Swift's
    // synthesized init(from:) calls `decode` (not `decodeIfPresent`) for non-Optional
    // properties even with a default value, throwing keyNotFound when the key is absent.
    // Declaring this in an extension preserves the synthesized memberwise initializer.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(UUID.self, forKey: .id)
        shell = try c.decode(String.self, forKey: .shell)
        workingDirectory = try c.decode(String.self, forKey: .workingDirectory)
        title = try c.decodeIfPresent(String.self, forKey: .title)
        startupCommand = try c.decodeIfPresent(String.self, forKey: .startupCommand)
        host = try c.decodeIfPresent(PaneHost.self, forKey: .host) ?? .local
        dormant = try c.decodeIfPresent(Bool.self, forKey: .dormant) ?? false
    }
}

struct BrowserConfig: Codable, Identifiable {
    let id: UUID
    var urlString: String
    var title: String?

    var url: URL? { URL(string: urlString) }
}

struct EditorConfig: Codable, Identifiable {
    let id: UUID
    var filePath: String
    var cursorLine: Int?
    var cursorColumn: Int?
}

struct DiffConfig: Codable, Identifiable {
    let id: UUID
    var repoPath: String
    var diffTarget: DiffTarget
}

enum DiffTarget: Codable {
    case staged
    case unstaged
    case commit(String)
    case range(String, String)
}

struct ScratchpadConfig: Codable, Identifiable {
    let id: UUID
    var title: String
    var content: String
}

struct FileExplorerConfig: Codable, Identifiable {
    let id: UUID
    var rootPath: String
    var openFilePaths: [String]     // Relative to rootPath
    var activeFilePath: String?     // Relative to rootPath
}

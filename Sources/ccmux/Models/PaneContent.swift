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
        case .terminal: return "Terminal"
        case .browser: return "Browser"
        case .editor(let c): return (c.filePath as NSString).lastPathComponent
        case .diff: return "Diff"
        case .scratchpad(let c): return c.title
        case .fileExplorer: return "Files"
        }
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

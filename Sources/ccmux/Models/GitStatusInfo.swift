import Foundation

/// Parsed git status for a workspace. Runtime-only, not persisted.
struct GitStatusInfo {
    var branch: String = ""
    var trackingBranch: String? = nil
    var ahead: Int = 0
    var behind: Int = 0

    /// Default branch comparison (e.g., current branch vs main)
    var defaultBranch: String? = nil
    var aheadOfDefault: Int = 0
    var behindDefault: Int = 0

    /// Whether we're on the default branch itself
    var isOnDefaultBranch: Bool {
        guard let def = defaultBranch else { return false }
        return branch == def
    }

    var stagedFiles: [FileChange] = []
    var modifiedFiles: [FileChange] = []
    var untrackedFiles: [FileChange] = []
    var deletedFiles: [FileChange] = []
    var isGitRepo: Bool = false

    var totalChanges: Int {
        stagedFiles.count + modifiedFiles.count + untrackedFiles.count + deletedFiles.count
    }

    var isClean: Bool { totalChanges == 0 }

    struct FileChange {
        let path: String
        let status: Status

        /// Just the filename without directory path
        var filename: String {
            (path as NSString).lastPathComponent
        }
    }

    enum Status: String {
        case modified = "M"
        case added = "A"
        case deleted = "D"
        case renamed = "R"
        case untracked = "?"
    }
}

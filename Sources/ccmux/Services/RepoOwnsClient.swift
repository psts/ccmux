import Foundation

/// A repo's "owns" line, as every session in a window is told it:
/// GET/PUT /v1/workspaces/{id}/owns (daemon/internal/api/owns.go). Auto is
/// Haiku's summary of the repo's CLAUDE.md and README, manual a typed line
/// that wins over it, line what sessions see. Same route as the web lens's
/// owns.js.
struct DaemonRepoOwns: Decodable, Equatable {
    var auto: String
    var manual: String
    var line: String
}

enum RepoOwnsClient {
    /// The workspace's line, or the reason it could not be read.
    static func read(daemonId: String) async -> Result<DaemonRepoOwns, RepoOwnsError> {
        await call(daemonId: daemonId, method: "GET", body: nil)
    }

    /// Sets the typed line ("" hands the repo back to the summary).
    static func setManual(daemonId: String, _ manual: String) async -> Result<DaemonRepoOwns, RepoOwnsError> {
        await call(daemonId: daemonId, method: "PUT", body: try? JSONEncoder().encode(["manual": manual]))
    }

    private static func call(daemonId: String, method: String, body: Data?) async -> Result<DaemonRepoOwns, RepoOwnsError> {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/owns") else {
            return .failure(RepoOwnsError(message: "bad daemon URL"))
        }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = body
        do {
            let (data, resp) = try await URLSession.shared.data(for: req)
            let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
            if code == 200, let owns = try? JSONDecoder().decode(DaemonRepoOwns.self, from: data) {
                return .success(owns)
            }
            struct Failure: Decodable { let error: String }
            let why = (try? JSONDecoder().decode(Failure.self, from: data))?.error ?? "HTTP \(code)"
            return .failure(RepoOwnsError(message: why))
        } catch {
            return .failure(RepoOwnsError(message: error.localizedDescription))
        }
    }
}

struct RepoOwnsError: Error, Equatable {
    let message: String
}

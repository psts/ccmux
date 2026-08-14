import Foundation

/// A request to spawn a "teammate" claude in a workspace, delivered by claude-peers
/// via the `ccmux://spawn?repo=…&prompt=…&requester=…` URL scheme.
///
/// The teammate runs in an ephemeral split pane (see `WorkspaceManager.spawnTeammate`)
/// whose seed prompt fires exactly once and is never persisted/replayed.
struct SpawnRequest: Equatable {
    /// Absolute, tilde-expanded repo path the teammate should run in.
    let repoPath: String
    /// The seed / "birth" prompt handed to claude on launch.
    let prompt: String
    /// Peer id of the requester (so the birth prompt can tell the teammate who to
    /// report back to). Optional — informational on the ccmux side.
    let requester: String?

    /// Parse a `ccmux://spawn` URL. Returns nil for any other host/scheme or when a
    /// required field (`repo`, `prompt`) is missing or empty.
    static func parse(from url: URL) -> SpawnRequest? {
        guard url.scheme == "ccmux", url.host == "spawn" else { return nil }
        // Use the RAW (percent-encoded) items and decode them as application/
        // x-www-form-urlencoded ourselves. JS `URLSearchParams` (what claude-peers
        // uses) encodes spaces as `+`, but `URLComponents.queryItems` only undoes
        // `%XX` and leaves `+` literal — so a multi-word prompt would arrive with `+`
        // instead of spaces. Decoding `+`→space *before* percent-decoding also keeps a
        // genuine `+` (sent as `%2B`) intact.
        guard let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let items = components.percentEncodedQueryItems else { return nil }

        func value(_ name: String) -> String? {
            guard let raw = items.first(where: { $0.name == name })?.value else { return nil }
            guard let decoded = raw.replacingOccurrences(of: "+", with: " ").removingPercentEncoding,
                  !decoded.isEmpty else { return nil }
            return decoded
        }

        guard let repoRaw = value("repo"), let prompt = value("prompt") else { return nil }
        let repoPath = (repoRaw as NSString).expandingTildeInPath
        return SpawnRequest(repoPath: repoPath, prompt: prompt, requester: value("requester"))
    }

    /// The shell command that launches an interactive claude seeded with `prompt`.
    ///
    /// Frozen contract with claude-peers so a spawned teammate registers with the broker:
    /// - the claude-peers channel is loaded (`--dangerously-load-development-channels
    ///   server:claude-peers`), a hidden variadic flag (verified to be accepted);
    /// - claude derives its peer name/project from the cwd, so we must NOT override
    ///   `CLAUDE_PEERS_NAME`/`CLAUDE_PEERS_PROJECT` — `env -u` strips any inherited ones
    ///   defensively (a no-op when absent);
    /// - `--` ends option parsing so the variadic flag doesn't swallow the prompt;
    /// - the birth `prompt` is the positional prompt, single-quoted for the shell.
    /// The cwd itself is set by the spawning pane (see `WorkspaceManager.spawnTeammate`).
    func claudeStartupCommand() -> String {
        "env -u CLAUDE_PEERS_NAME -u CLAUDE_PEERS_PROJECT "
            + Self.peersEnabledClaude + " -- "
            + Self.shellSingleQuote(prompt)
    }

    /// Launching claude so the claude-peers channel is loaded — and therefore so
    /// pushed peer messages actually reach the session.
    ///
    /// Named once because it is needed in two places that had drifted: the spawn
    /// above, and the command persisted for a seeded pane so a later restart
    /// replays it (see WorkspaceManager). That persisted command used to be a
    /// bare `claude`, which relaunches the pane without the flag — and Claude
    /// Code drops channel pushes for a server the session did not load, silently
    /// and with no error to the sender. The pane came back registered, listed as
    /// online, able to send, and unable to hear a thing.
    static let peersEnabledClaude = "claude --dangerously-load-development-channels server:claude-peers"

    /// POSIX-safe single-quote escaping: wrap in single quotes, and replace each
    /// embedded single quote with the `'\''` idiom (close-quote, escaped-quote,
    /// reopen-quote). Handles spaces, `$`, backticks, quotes, etc.
    static func shellSingleQuote(_ s: String) -> String {
        "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }
}

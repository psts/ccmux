import Foundation

/// Read-only client for ccmuxd's built-in peers bus (the old external broker
/// on :7899 is gone — the daemon now hosts history, peers, and the live
/// listen stream under /v1/peers/*, keyed by window group name).
class PeerBrokerService {
    static let shared = PeerBrokerService()

    private var baseURL: String { DaemonConfig.baseURL }
    private var wsBaseURL: String { DaemonConfig.wsBaseURL }

    /// Shared pane-less bearer token from the daemon's info file — authorizes
    /// the local-pane group push (0600, same-user only). Cached after first read;
    /// cleared on auth failure so a daemon re-mint gets picked up.
    private var cachedToken: String?
    private func panelessToken() -> String? {
        if let t = cachedToken { return t }
        let path = ("~/Library/Application Support/ccmuxd/peers.json" as NSString).expandingTildeInPath
        guard let data = FileManager.default.contents(atPath: path),
              let info = try? JSONDecoder().decode([String: String].self, from: data),
              let token = info["token"] else { return nil }
        cachedToken = token
        return token
    }

    /// Push the complete local-pane→window-name map (window grouping for
    /// driver-mode panes ccmuxd doesn't host). Fire-and-forget; the caller
    /// re-pushes periodically so a lost push self-heals.
    func pushLocalGroups(_ groups: [String: String]) async {
        guard let token = panelessToken(),
              let url = URL(string: "\(baseURL)/v1/peers/local-groups") else { return }
        var req = URLRequest(url: url)
        req.httpMethod = "PUT"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        req.httpBody = try? JSONEncoder().encode(["groups": groups])
        guard let (_, resp) = try? await URLSession.shared.data(for: req) else { return }
        if (resp as? HTTPURLResponse)?.statusCode == 401 { cachedToken = nil }
    }

    func fetchMessages(group: String, limit: Int = 50, since: Date? = nil) async throws -> [PeerMessage] {
        var components = URLComponents(string: "\(baseURL)/v1/peers/messages")!
        var queryItems = [URLQueryItem(name: "group", value: group)]
        queryItems.append(URLQueryItem(name: "limit", value: "\(limit)"))
        if let since {
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            queryItems.append(URLQueryItem(name: "since", value: formatter.string(from: since)))
        }
        components.queryItems = queryItems

        let (data, _) = try await URLSession.shared.data(from: components.url!)
        return try JSONDecoder().decode([PeerMessage].self, from: data)
    }

    func fetchPeers(group: String) async throws -> [PeerInfo] {
        var components = URLComponents(string: "\(baseURL)/v1/peers")!
        components.queryItems = [URLQueryItem(name: "group", value: group)]

        let (data, _) = try await URLSession.shared.data(from: components.url!)
        return try JSONDecoder().decode([PeerInfo].self, from: data)
    }

    func connectWebSocket(group: String) -> (stream: AsyncStream<PeerWSMessage>, cancel: () -> Void) {
        var components = URLComponents(string: "\(wsBaseURL)/v1/peers/ws")!
        components.queryItems = [
            URLQueryItem(name: "mode", value: "listen"),
            URLQueryItem(name: "group", value: group),
        ]

        let task = URLSession.shared.webSocketTask(with: components.url!)
        task.resume()

        let stream = AsyncStream<PeerWSMessage> { continuation in
            func receiveNext() {
                task.receive { result in
                    switch result {
                    case .success(let message):
                        switch message {
                        case .string(let text):
                            if let data = text.data(using: .utf8),
                               let wsMessage = try? JSONDecoder().decode(PeerWSMessage.self, from: data) {
                                continuation.yield(wsMessage)
                            }
                        case .data(let data):
                            if let wsMessage = try? JSONDecoder().decode(PeerWSMessage.self, from: data) {
                                continuation.yield(wsMessage)
                            }
                        @unknown default:
                            break
                        }
                        receiveNext()
                    case .failure:
                        continuation.finish()
                    }
                }
            }

            continuation.onTermination = { _ in
                task.cancel(with: .goingAway, reason: nil)
            }

            receiveNext()
        }

        return (stream: stream, cancel: { task.cancel(with: .goingAway, reason: nil) })
    }
}

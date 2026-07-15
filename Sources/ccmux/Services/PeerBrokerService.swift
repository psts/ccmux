import Foundation

/// Read-only client for ccmuxd's built-in peers bus (the old external broker
/// on :7899 is gone — the daemon now hosts history, peers, and the live
/// listen stream under /v1/peers/*, keyed by window group name).
class PeerBrokerService {
    static let shared = PeerBrokerService()

    private var baseURL: String { DaemonConfig.baseURL }
    private var wsBaseURL: String { DaemonConfig.wsBaseURL }

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

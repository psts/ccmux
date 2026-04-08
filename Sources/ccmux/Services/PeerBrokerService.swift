import Foundation

class PeerBrokerService {
    static let shared = PeerBrokerService()

    var basePortValue: Int {
        if let portStr = ProcessInfo.processInfo.environment["CLAUDE_PEERS_PORT"],
           let port = Int(portStr) {
            return port
        }
        return 7899
    }

    private var baseURL: String { "http://127.0.0.1:\(basePortValue)" }
    private var wsBaseURL: String { "ws://127.0.0.1:\(basePortValue)" }

    func fetchMessages(project: String, limit: Int = 50, since: Date? = nil) async throws -> [PeerMessage] {
        var components = URLComponents(string: "\(baseURL)/project-messages")!
        var queryItems = [URLQueryItem(name: "project", value: project)]
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

    func fetchPeers(project: String) async throws -> [PeerInfo] {
        var components = URLComponents(string: "\(baseURL)/list-peers")!
        components.queryItems = [
            URLQueryItem(name: "project", value: project),
            URLQueryItem(name: "scope", value: "project"),
        ]

        let (data, _) = try await URLSession.shared.data(from: components.url!)
        return try JSONDecoder().decode([PeerInfo].self, from: data)
    }

    func connectWebSocket(project: String) -> (stream: AsyncStream<PeerWSMessage>, cancel: () -> Void) {
        var components = URLComponents(string: "\(wsBaseURL)/ws")!
        components.queryItems = [
            URLQueryItem(name: "project", value: project),
            URLQueryItem(name: "mode", value: "listen"),
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

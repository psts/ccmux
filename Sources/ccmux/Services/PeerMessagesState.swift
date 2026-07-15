import Foundation

@MainActor
class PeerMessagesState: ObservableObject {
    @Published var messages: [PeerMessage] = []
    @Published var peers: [PeerInfo] = []
    @Published var isConnected: Bool = false
    @Published var error: String? = nil

    private let service = PeerBrokerService.shared
    private var wsCancel: (() -> Void)?
    private var listenTask: Task<Void, Never>?
    private var nextLocalId = -1

    func start(group: String) {
        error = nil
        isConnected = false

        // Fetch history + peers in parallel, then start WebSocket
        listenTask = Task {
            do {
                async let fetchedMessages = service.fetchMessages(group: group)
                async let fetchedPeers = service.fetchPeers(group: group)
                let (msgs, prs) = try await (fetchedMessages, fetchedPeers)
                messages = msgs
                peers = prs
                isConnected = true
            } catch {
                self.error = "Cannot reach ccmuxd at \(DaemonConfig.baseURL)"
                return
            }

            // Start WebSocket for live updates
            let (stream, cancel) = service.connectWebSocket(group: group)
            wsCancel = cancel

            for await wsMessage in stream {
                guard !Task.isCancelled else { break }
                let msg = PeerMessage(
                    id: nextLocalId,
                    from_id: wsMessage.from_id,
                    to_id: wsMessage.to_id,
                    from_name: wsMessage.from_name,
                    to_name: wsMessage.to_name,
                    text: wsMessage.text,
                    sent_at: wsMessage.sent_at
                )
                nextLocalId -= 1
                messages.append(msg)
            }

            // WebSocket disconnected
            if !Task.isCancelled {
                isConnected = false
            }
        }
    }

    func stop() {
        listenTask?.cancel()
        listenTask = nil
        wsCancel?()
        wsCancel = nil
        messages = []
        peers = []
        isConnected = false
        error = nil
        nextLocalId = -1
    }
}

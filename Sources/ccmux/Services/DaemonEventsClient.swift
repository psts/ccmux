import Foundation

/// One WebSocket to the daemon's global firehose (`GET /v1/events`): a single
/// connection carrying attention changes for *every* hosted workspace, so the app
/// flashes a sidebar row without holding a full per-workspace attach on it.
///
/// Read-only (the firehose accepts no client commands) and stateless beyond the
/// socket. Auto-reconnects with capped backoff until `disconnect()`. Deliberately
/// a sibling of `DaemonAttachClient` rather than a shared base: the attach client
/// is per-workspace, bidirectional, and read-only-gated, while this is a global,
/// receive-only sidebar feed — the small reconnect pump is clearer duplicated than
/// abstracted across those differences.
final class DaemonEventsClient {
    /// Called on the main thread for every decoded firehose event.
    var onEvent: ((DaemonFirehoseEvent) -> Void)?
    /// Called on the main thread whenever the connection state changes.
    var onStateChange: ((DaemonConnectionState) -> Void)?

    private(set) var state: DaemonConnectionState = .connecting {
        didSet {
            guard state != oldValue else { return }
            let s = state
            DispatchQueue.main.async { [weak self] in self?.onStateChange?(s) }
        }
    }

    private let session: URLSession
    private var task: URLSessionWebSocketTask?
    private var closed = false
    private var reconnectAttempts = 0

    init() { self.session = URLSession(configuration: .default) }

    // MARK: - Lifecycle

    func connect() {
        closed = false
        openSocket()
    }

    func disconnect() {
        closed = true
        state = .closed
        task?.cancel(with: .goingAway, reason: nil)
        task = nil
    }

    // MARK: - Socket plumbing

    private var url: URL? { URL(string: "\(DaemonConfig.wsBaseURL)/v1/events") }

    private func openSocket() {
        guard !closed, let url else { return }
        state = reconnectAttempts == 0 ? .connecting : .reconnecting
        let task = session.webSocketTask(with: url)
        self.task = task
        task.resume()
        // The first successful frame (the hello) confirms the connection is live.
        receiveLoop(on: task, firstFrame: true)
    }

    private func receiveLoop(on task: URLSessionWebSocketTask, firstFrame: Bool) {
        task.receive { [weak self] result in
            guard let self, !self.closed, task === self.task else { return }
            switch result {
            case .success(let message):
                if firstFrame {
                    self.reconnectAttempts = 0
                    self.state = .connected
                }
                self.handle(message)
                self.receiveLoop(on: task, firstFrame: false)
            case .failure:
                self.scheduleReconnect()
            }
        }
    }

    private func handle(_ message: URLSessionWebSocketTask.Message) {
        let text: String?
        switch message {
        case .string(let s): text = s
        case .data(let d): text = String(data: d, encoding: .utf8)
        @unknown default: text = nil
        }
        guard let text, let event = DaemonFirehoseEvent.decode(text: text) else { return }
        DispatchQueue.main.async { [weak self] in self?.onEvent?(event) }
    }

    private func scheduleReconnect() {
        guard !closed else { return }
        state = .reconnecting
        reconnectAttempts += 1
        // 0.5s, 1s, 2s, 4s, capped at 5s — matches the attach client.
        let delay = min(0.5 * pow(2.0, Double(reconnectAttempts - 1)), 5.0)
        DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, !self.closed else { return }
            self.openSocket()
        }
    }
}

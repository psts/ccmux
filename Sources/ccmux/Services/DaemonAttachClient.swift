import Foundation

/// Connection state of a workspace attach, surfaced so a hosted pane can show a
/// reconnect overlay.
enum DaemonConnectionState: Equatable {
    case connecting
    case connected
    case reconnecting
    case closed
}

/// One WebSocket attach to a hosted workspace (`GET /v1/attach?workspace=…`),
/// multiplexing every pane in that workspace. Decodes `DaemonEvent`s and forwards
/// them on the main thread; accepts `DaemonCommand`s to send back. Auto-reconnects
/// with capped backoff until `disconnect()`.
///
/// Modeled on `PeerBrokerService`'s URLSessionWebSocketTask pump, but stateful:
/// the daemon must stay the sole tmux client, so the lens speaks only this contract.
final class DaemonAttachClient {
    let workspaceId: String
    private let readonly: Bool

    /// Called on the main thread for every decoded event.
    var onEvent: ((DaemonEvent) -> Void)?
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

    /// WS origin for this workspace's terminal stream. Federation: a workspace on
    /// a remote host attaches DIRECT to that host (wss://<host>.ts.net), never via
    /// the hub — the Mac resolves the name over the tailnet's system MagicDNS.
    /// Defaults to the configured base (single-host, or the hub's own sessions).
    private let wsOrigin: String

    init(workspaceId: String, readonly: Bool = false, wsOrigin: String? = nil) {
        self.workspaceId = workspaceId
        self.readonly = readonly
        self.wsOrigin = wsOrigin ?? DaemonConfig.wsBaseURL
        self.session = URLSession(configuration: .default)
    }

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

    // MARK: - Sending

    /// Send a command to the daemon. Read-only observers skip input/resize locally
    /// (the daemon enforces this too) but may still report focus.
    func send(_ command: DaemonCommand) {
        if readonly, case .focus = command {} else if readonly { return }
        guard let data = command.jsonData(),
              let text = String(data: data, encoding: .utf8) else { return }
        task?.send(.string(text)) { _ in }
    }

    // MARK: - Socket plumbing

    private var url: URL? {
        var components = URLComponents(string: "\(wsOrigin)/v1/attach")
        var items = [
            URLQueryItem(name: "workspace", value: workspaceId),
            URLQueryItem(name: "user", value: DaemonConfig.selfUser),
            URLQueryItem(name: "device", value: DaemonConfig.selfDevice),
        ]
        if readonly { items.append(URLQueryItem(name: "readonly", value: "1")) }
        components?.queryItems = items
        return components?.url
    }

    private func openSocket() {
        guard !closed, let url else { return }
        state = reconnectAttempts == 0 ? .connecting : .reconnecting
        let task = session.webSocketTask(with: url)
        self.task = task
        task.resume()
        // The first successful frame confirms the connection is live.
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
        guard let text, let event = DaemonEvent.decode(text: text) else { return }
        DispatchQueue.main.async { [weak self] in self?.onEvent?(event) }
    }

    private func scheduleReconnect() {
        guard !closed else { return }
        state = .reconnecting
        reconnectAttempts += 1
        // 0.5s, 1s, 2s, 4s, capped at 5s.
        let delay = min(0.5 * pow(2.0, Double(reconnectAttempts - 1)), 5.0)
        DispatchQueue.main.asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, !self.closed else { return }
            self.openSocket()
        }
    }
}

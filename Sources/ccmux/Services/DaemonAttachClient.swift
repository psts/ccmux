import Foundation

/// Connection state of a workspace attach, surfaced so a hosted pane can show a
/// reconnect overlay.
enum DaemonConnectionState: Equatable {
    case connecting
    case connected
    case reconnecting
    case closed
}

/// The slice of the attach connection a pane controller needs: somewhere to send
/// commands, and the workspace they belong to. A protocol so a test can stand a
/// spy in for the real client, which owns a live socket and offers nothing to
/// observe — the untestable seam that left the activation wiring unpinned.
protocol AttachCommandSink: AnyObject {
    var workspaceId: String { get }
    func send(_ command: DaemonCommand)
}

/// One WebSocket attach to a hosted workspace (`GET /v1/attach?workspace=…`),
/// multiplexing every pane in that workspace. Decodes `DaemonEvent`s and forwards
/// them on the main thread; accepts `DaemonCommand`s to send back. Auto-reconnects
/// with capped backoff until `disconnect()`.
///
/// Modeled on `PeerBrokerService`'s URLSessionWebSocketTask pump, but stateful:
/// the daemon must stay the sole tmux client, so the lens speaks only this contract.
final class DaemonAttachClient: AttachCommandSink {
    let workspaceId: String
    private let readonly: Bool

    /// Called on the main thread for every decoded event.
    var onEvent: ((DaemonEvent) -> Void)?
    /// Called on the main thread whenever the connection state changes.
    var onStateChange: ((DaemonConnectionState) -> Void)?

    private var pump: WebSocketPump!

    /// WS origin for this workspace's terminal stream. Federation: a workspace on
    /// a remote host attaches DIRECT to that host (wss://<host>.ts.net), never via
    /// the hub — the Mac resolves the name over the tailnet's system MagicDNS.
    /// Defaults to the configured base (single-host, or the hub's own sessions).
    private let wsOrigin: String

    init(workspaceId: String, readonly: Bool = false, wsOrigin: String? = nil) {
        self.workspaceId = workspaceId
        self.readonly = readonly
        self.wsOrigin = wsOrigin ?? DaemonConfig.wsBaseURL
        pump = WebSocketPump(label: "attach-\(workspaceId)") { [weak self] in self?.url }
        pump.onText = { [weak self] text in
            guard let self else { return }
            // A frame the daemon wrote and this build cannot read is the last
            // silent drop in this path: the socket stays healthy, so every
            // indicator says the lens is fine while an attention change simply
            // vanishes. Unknown KEYS decode fine; a changed type or a removed
            // required field does not, which is the fleet-skew case.
            guard let event = DaemonEvent.decode(text: text) else {
                NSLog("[ccmux attach] dropped an undecodable frame (%d bytes): %@",
                      text.utf8.count, String(text.prefix(120)))
                return
            }
            DispatchQueue.main.async { self.onEvent?(event) }
        }
        pump.onState = { [weak self] s in self?.onStateChange?(s) }
    }

    // MARK: - Lifecycle

    func connect() { pump.connect() }

    func disconnect() { pump.disconnect() }

    /// Dial again now, without waiting for the socket to admit it is dead (wake).
    func forceReconnect() { pump.forceReconnect() }

    // MARK: - Sending

    /// Send a command to the daemon. Read-only observers skip input/resize locally
    /// (the daemon enforces this too) but may still report focus.
    func send(_ command: DaemonCommand) {
        if readonly, case .focus = command {} else if readonly { return }
        guard let data = command.jsonData(),
              let text = String(data: data, encoding: .utf8) else { return }
        pump.send(text)
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

}

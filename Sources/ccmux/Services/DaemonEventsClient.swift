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

    private let pump: WebSocketPump

    init() {
        pump = WebSocketPump(label: "events") {
            // The identity matters: the daemon stamps each frame's alert flag for
            // the lens it is writing to, so an unidentified firehose would never be
            // told to notify. It has to be the SAME key the attach socket presents,
            // or presence and alerting are talking about two different people.
            var components = URLComponents(string: "\(DaemonConfig.wsBaseURL)/v1/events")
            components?.queryItems = [
                URLQueryItem(name: "user", value: DaemonConfig.selfUser),
                URLQueryItem(name: "device", value: DaemonConfig.selfDevice),
            ]
            return components?.url
        }
        pump.onText = { [weak self] text in
            guard let self, let event = DaemonFirehoseEvent.decode(text: text) else { return }
            DispatchQueue.main.async { self.onEvent?(event) }
        }
        pump.onState = { [weak self] s in self?.onStateChange?(s) }
    }

    // MARK: - Lifecycle

    func connect() { pump.connect() }

    func disconnect() { pump.disconnect() }

    /// Dial again now, without waiting for the socket to admit it is dead. Used on
    /// wake, where the pre-sleep connection is usually a corpse that would
    /// otherwise be detected only at the next ping timeout.
    func forceReconnect() { pump.forceReconnect() }
}

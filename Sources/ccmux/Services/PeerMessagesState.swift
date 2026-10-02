import Foundation

@MainActor
class PeerMessagesState: ObservableObject {
    @Published var messages: [PeerMessage] = []
    @Published var peers: [PeerInfo] = []
    @Published var isConnected: Bool = false
    @Published var error: String? = nil
    /// Set when the daemon could not say which bus to read. What follows may be
    /// the local registry rather than the bus the sessions are on, so an empty
    /// list is not evidence of silence and must not be drawn as one.
    @Published var busUnconfirmed: Bool = false
    /// Set when the history read after a hello failed: the list may be missing
    /// what was sent while the stream was down. Stays until a read succeeds;
    /// live messages arriving do not clear it, since they say nothing about
    /// the gap.
    @Published var historyNote: String? = nil

    private let service = PeerBrokerService.shared
    private var pump: WebSocketPump?
    private var listenTask: Task<Void, Never>?
    private var readBackTask: Task<Void, Never>?
    /// Bumped by every start() and stop(). A read-back that lands after either,
    /// including a close and reopen on the same group, sees a different number
    /// and writes nothing.
    private var openID = 0
    private var nextLocalId = -1

    func start(group: String) {
        openID &+= 1
        let id = openID
        error = nil
        isConnected = false

        // Which bus first, then history + peers in parallel, then the WebSocket.
        // Asking every time the overlay opens is what keeps it pointed at the bus
        // the sessions are actually on: a hub can appear or move while the app is
        // running, and reading the local registry after that shows an empty panel
        // rather than the truth.
        //
        // After every await, a stop() or reopen since (openID moved on) means
        // this open is over: its rows, its error and its pump would all land on
        // a panel that is no longer it, and a close mid-read cancels the read,
        // which is not a failure to log.
        listenTask = Task {
            let bus = await service.refreshBus()
            guard id == openID else { return }
            busUnconfirmed = !bus
            do {
                let (msgs, prs) = try await readHistory(group: group)
                guard id == openID else { return }
                (messages, peers) = (msgs, prs)
                isConnected = true
            } catch {
                guard id == openID else { return }
                NSLog("[ccmux peers] reading history on open failed: %@", "\(error)")
                self.error = Self.describe(error)
                return
            }
            listen(group: group)
        }
    }

    /// The live stream, on the same pump the lens sockets use: it redials with
    /// backoff after any drop (a daemon restart, a network blip, a half-open
    /// path its pings catch), as the web lens's peers.js redials. The socket it
    /// replaces ended its stream on the first error and left the list frozen
    /// until the overlay was reopened. What a drop skipped is read back on the
    /// next hello (readBack).
    private func listen(group: String) {
        guard let request = service.listenRequest(group: group) else {
            NSLog("[ccmux peers] cannot build the listen request for group %@", group)
            return
        }
        let p = WebSocketPump.requesting(label: "peers-\(group)") { request }
        p.onText = { [weak self] text in
            guard let m = PeerWSMessage.fromListenFrame(text) else { return }
            // The main queue keeps frames in order, as in AgentChatState.
            DispatchQueue.main.async { MainActor.assumeIsolated { self?.append(m) } }
        }
        p.onState = { [weak self] s in
            DispatchQueue.main.async { MainActor.assumeIsolated { self?.connectionChanged(s, group: group) } }
        }
        pump = p
        p.connect()
    }

    /// The dot follows the socket once it has spoken: the daemon's hello (or
    /// any frame) turns it on, a drop turns it off until the redial hears back.
    /// `.connecting` leaves it alone, so the first dial does not blink out what
    /// the history read just set, and a daemon too old to send a hello does not
    /// leave a quiet group on "Connecting...".
    private func connectionChanged(_ s: DaemonConnectionState, group: String) {
        switch s {
        case .connected:
            isConnected = true
            readBack(group: group)
        case .reconnecting, .closed:
            isConnected = false
        case .connecting:
            break
        }
    }

    /// Every hello reads history and the peer list again and merges them into
    /// what is on screen. The hello is written only after the listener is
    /// registered, and the daemon saves a message before it broadcasts it, so
    /// between this read and the live stream nothing is missed: not what a
    /// drop skipped, and not what was sent between the open's first read and
    /// the first dial. The merge is by message number, so a message that
    /// arrives live while this read is in flight is neither lost nor doubled.
    /// The web lens does the same on every hello (peers.js).
    private func readBack(group: String) {
        readBackTask?.cancel()
        let id = openID
        readBackTask = Task {
            do {
                let (msgs, prs) = try await readHistory(group: group)
                guard id == openID, !Task.isCancelled else { return }
                messages = PeerMessage.merged(history: msgs, onScreen: messages)
                peers = prs
                historyNote = nil
            } catch {
                guard id == openID, !Task.isCancelled else { return }
                NSLog("[ccmux peers] reading history back after a hello failed: %@", "\(error)")
                historyNote = "Couldn't read back the history (\(Self.describe(error))). " +
                    "Messages sent while disconnected may be missing; reopen to reload."
            }
        }
    }

    private func readHistory(group: String) async throws -> ([PeerMessage], [PeerInfo]) {
        async let fetchedMessages = service.fetchMessages(group: group, limit: PeerMessage.historyLimit)
        async let fetchedPeers = service.fetchPeers(group: group)
        return try await (fetchedMessages, fetchedPeers)
    }

    /// A refusal names itself; saying "cannot reach ccmuxd" for one would send
    /// the reader to the one thing that is working. Nor for a reply this app
    /// cannot decode: the daemon answered, and the two are out of step.
    private static func describe(_ error: Error) -> String {
        if let refusal = error as? PeerBrokerError { return refusal.message }
        if error is DecodingError {
            return "ccmuxd answered, but this app can't read the reply; the app and the daemon may be out of step"
        }
        return "Cannot reach ccmuxd at \(DaemonConfig.localURL)"
    }

    private func append(_ m: PeerWSMessage) {
        guard let row = PeerMessage.liveRow(m, onScreen: messages, nextLocalId: &nextLocalId) else { return }
        messages.append(row)
    }

    func stop() {
        openID &+= 1
        listenTask?.cancel()
        listenTask = nil
        readBackTask?.cancel()
        readBackTask = nil
        pump?.disconnect()
        pump = nil
        messages = []
        peers = []
        isConnected = false
        error = nil
        busUnconfirmed = false
        historyNote = nil
        nextLocalId = -1
    }
}

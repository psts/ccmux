import Foundation

/// A read that reached a daemon and was refused, as opposed to one that never
/// arrived. Worth its own type: once reads travel through the hub relay, "no
/// messages" and "the hub is unreachable" look identical in an empty panel, and
/// the empty panel is the bug this whole path exists to stop telling.
enum PeerBrokerError: Error {
    case refused(status: Int)

    var message: String {
        switch self {
        case .refused(503):
            return "ccmuxd can't reach the hub right now — peers live there, so this list would be wrong."
        case .refused(401):
            return "ccmuxd rejected the lens credential. Restarting the daemon re-mints it."
        case .refused(let status):
            return "The peers bus answered HTTP \(status)."
        }
    }
}

/// Client for ccmuxd's built-in peers bus (the old external broker on :7899 is
/// gone — the daemon now hosts history, peers, and the live listen stream under
/// /v1/peers/*, keyed by window group name). Reads are read-only; the one write
/// is this Mac's own local-pane group map.
class PeerBrokerService {
    static let shared = PeerBrokerService()

    // Writes (the local-pane group map) always go to the LOCAL daemon: that route
    // is loopback-gated server-side, and it is this Mac's own map to give.
    private var baseURL: String { DaemonConfig.localURL }

    // Reads go to whatever bus this Mac's sessions are ON, which since hub
    // federation is usually not the local registry: sessions resolve onto the
    // hub and register there, leaving the local daemon holding nobody. The
    // daemon answers with its own /v1/hubbus relay rather than the hub's URL, so
    // the app still talks only to 127.0.0.1 and the tailnet hop wears the
    // daemon's identity — the app's own tailscaled is a DIFFERENT node, which is
    // the split that made panes unable to reach the hub in the first place.
    private var busURL: String = DaemonConfig.localURL
    private var wsBusURL: String { DaemonConfig.wsOrigin(busURL) }

    /// The read-only credential for `busURL`, handed out by the daemon to a
    /// caller that proved same-user. nil when the daemon withheld it or was not
    /// reachable; the local routes do not need it.
    private var viewerToken: String?

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

    /// Ask the daemon which bus to read and for the credential to read it with
    /// (GET /v1/peers/viewer). The DAEMON decides — the same answer it gives the
    /// browser lens — so the two cannot drift into looking at different buses.
    /// An empty `bus` means "no hub, I am the bus".
    ///
    /// Re-asked every time the overlay opens: a hub can appear, move, or go away
    /// while the app is running, and a value cached at launch would outlive it.
    ///
    /// Returns false when the answer could not be obtained, so the caller can say
    /// the list may be incomplete instead of drawing a confident empty panel.
    @discardableResult
    func refreshBus() async -> Bool {
        busURL = DaemonConfig.localURL
        viewerToken = nil
        guard let token = panelessToken(),
              let url = URL(string: "\(baseURL)/v1/peers/viewer") else {
            NSLog("[ccmux peers] no daemon credential — cannot ask which bus to read")
            return false
        }
        var req = URLRequest(url: url)
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        guard let (data, resp) = try? await URLSession.shared.data(for: req) else {
            NSLog("[ccmux peers] could not reach ccmuxd to ask which bus to read")
            return false
        }
        let status = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if status == 401 { cachedToken = nil }
        guard status == 200,
              let answer = try? JSONDecoder().decode([String: String].self, from: data) else {
            NSLog("[ccmux peers] bus resolve answered HTTP \(status)")
            return false
        }
        // A relative prefix ("/v1/hubbus") or "" — the daemon names a path on
        // itself, never a hub URL, so the app still talks only to 127.0.0.1 and
        // the tailnet hop wears the daemon's identity rather than this app's.
        busURL = DaemonConfig.localURL + (answer["bus"] ?? "")
        viewerToken = answer["token"].flatMap { $0.isEmpty ? nil : $0 }
        return true
    }

    /// Authorizes a viewer read. The local routes take no token and ignore it;
    /// through the relay it is the whole test, since the caller's address proves
    /// nothing about which local account it belongs to.
    private func authorize(_ req: inout URLRequest) {
        if let viewerToken {
            req.setValue("Bearer \(viewerToken)", forHTTPHeaderField: "Authorization")
        }
    }

    func fetchMessages(group: String, limit: Int = 50, since: Date? = nil) async throws -> [PeerMessage] {
        var components = URLComponents(string: "\(busURL)/v1/peers/messages")!
        var queryItems = [URLQueryItem(name: "group", value: group)]
        queryItems.append(URLQueryItem(name: "limit", value: "\(limit)"))
        if let since {
            let formatter = ISO8601DateFormatter()
            formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
            queryItems.append(URLQueryItem(name: "since", value: formatter.string(from: since)))
        }
        components.queryItems = queryItems

        var req = URLRequest(url: components.url!)
        authorize(&req)
        let (data, resp) = try await URLSession.shared.data(for: req)
        try check(resp)
        return try JSONDecoder().decode([PeerMessage].self, from: data)
    }

    /// Turns a refusal into a refusal. Without this a 503 from the relay decodes
    /// as "no messages" and the overlay reports silence it cannot vouch for.
    private func check(_ resp: URLResponse) throws {
        guard let http = resp as? HTTPURLResponse else { return }
        if http.statusCode == 401 { cachedToken = nil }
        if http.statusCode != 200 {
            throw PeerBrokerError.refused(status: http.statusCode)
        }
    }

    func fetchPeers(group: String) async throws -> [PeerInfo] {
        var components = URLComponents(string: "\(busURL)/v1/peers")!
        components.queryItems = [URLQueryItem(name: "group", value: group)]

        var req = URLRequest(url: components.url!)
        authorize(&req)
        let (data, resp) = try await URLSession.shared.data(for: req)
        try check(resp)
        return try JSONDecoder().decode([PeerInfo].self, from: data)
    }

    func connectWebSocket(group: String) -> (stream: AsyncStream<PeerWSMessage>, cancel: () -> Void) {
        var components = URLComponents(string: "\(wsBusURL)/v1/peers/ws")!
        components.queryItems = [
            URLQueryItem(name: "mode", value: "listen"),
            URLQueryItem(name: "group", value: group),
        ]

        var request = URLRequest(url: components.url!)
        authorize(&request)
        let task = URLSession.shared.webSocketTask(with: request)
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

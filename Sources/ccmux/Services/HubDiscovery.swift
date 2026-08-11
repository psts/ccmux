import Foundation

/// Hub discovery: ask the LOCAL daemon which tailnet node is the federation
/// hub (`GET /v1/hub` — the daemon watches for `tag:ccmux-hub`), health-check
/// the answer, and adopt it as `DaemonConfig.discoveredHubURL`. Every failure
/// path leaves the config on the local daemon, so single-host setups and
/// offline launches behave exactly as before.
enum HubDiscovery {
    /// Per-request ceiling: discovery must never hold up app launch noticeably.
    static let timeout: TimeInterval = 2
    /// Matches the daemon's own tailnet tag-scan cadence.
    static let retryInterval: TimeInterval = 15

    /// Adopt the hub as the app-wide daemon base. The first attempt completes
    /// before the caller starts the daemon services (fast when the daemon is
    /// warm). When it misses — the common cold-boot case: app and daemon launch
    /// together and the daemon hasn't scanned the tailnet yet — a retry timer
    /// keeps trying and fires `onLateAdopt` so the caller can rewire
    /// already-running services. It is one loopback GET per interval, so it just
    /// runs for the app's lifetime in single-host setups (and adopts a hub stood
    /// up mid-session). Once adopted, discovery stops for good: hub-down
    /// fallback is out of scope by design (multihost-plan).
    ///
    /// No-op when `CCMUXD_URL` is set — an explicit pin means the user chose a
    /// host; silently rerouting it would be worse than wrong.
    static func adoptHub(onLateAdopt: @escaping @MainActor () -> Void) async {
        guard ProcessInfo.processInfo.environment["CCMUXD_URL"] == nil else { return }
        if await attempt() { return }
        await MainActor.run { startRetrying(onLateAdopt: onLateAdopt) }
    }

    /// The retry, on a run-loop timer rather than a detached `Task` sleeping in a
    /// loop. The Task version was observed to stop retrying — for three days, on
    /// a launch that beat its daemon up — and the cause was never found, while a
    /// Timer-driven poll in the same process kept running throughout. Hence the
    /// timer, and hence `logMiss`: a retry that says nothing cannot be told apart
    /// from one that is not running, which is why that took three days to notice.
    @MainActor
    private static func startRetrying(onLateAdopt: @escaping @MainActor () -> Void) {
        retryTimer?.invalidate()
        let timer = Timer(timeInterval: retryInterval, repeats: true) { _ in
            Task { @MainActor in
                guard await attempt() else { return }
                retryTimer?.invalidate()
                retryTimer = nil
                onLateAdopt()
            }
        }
        // .common, not the default mode: a timer scheduled the usual way stops
        // firing while a menu is open or a scroll is tracking, and adoption
        // should not wait on the user letting go of the mouse.
        RunLoop.main.add(timer, forMode: .common)
        retryTimer = timer
        NSLog("[ccmux hub] no hub yet — retrying every \(Int(retryInterval))s")
    }

    @MainActor private static var retryTimer: Timer?
    @MainActor private static var attempts = 0

    /// One attempt: resolve, and on success move the app-wide base URL. Returns
    /// whether the hub was adopted, which is also what stops the retry timer —
    /// so an attempt that reports success without moving `baseURL` would strand
    /// the app on the local daemon believing it had federated. `fetch` is
    /// injectable for tests; the default hits the network.
    @discardableResult
    static func attempt(fetch: (String) async -> Data? = { await httpGet($0) }) async -> Bool {
        guard let hub = await resolve(fetch: fetch) else {
            await MainActor.run { logMiss() }
            return false
        }
        DaemonConfig.discoveredHubURL = hub
        NSLog("[ccmux hub] using hub \(hub)")
        return true
    }

    /// Report the first miss, then one in every 40 — about once every ten minutes
    /// at the 15s interval. Often enough that a stuck lens says so, rarely enough
    /// that a single-host Mac (where "no hub" is simply the truth, forever) does
    /// not fill the log.
    @MainActor
    private static func logMiss() {
        attempts += 1
        if attempts == 1 || attempts % 40 == 0 {
            NSLog("[ccmux hub] still no hub after \(attempts) attempt(s) — staying on \(DaemonConfig.localURL)")
        }
    }

    /// One resolution attempt: the local daemon's answer to "who is the hub?",
    /// gated on that hub actually answering — a laptop off the tailnet must
    /// keep its local sessions rather than silently pointing at a dead hub.
    /// Returns nil on: no local daemon, old daemon without the route, no hub
    /// found (yet), this machine IS the hub, or an unhealthy hub.
    /// `fetch` is injectable for tests; the default hits the network.
    static func resolve(fetch: (String) async -> Data? = { await httpGet($0) }) async -> String? {
        guard let data = await fetch("\(DaemonConfig.localURL)/v1/hub"),
              let info = try? JSONDecoder().decode(HubInfo.self, from: data),
              !info.url.isEmpty
        else { return nil }
        guard let health = await fetch("\(info.url)/v1/health"),
              (try? JSONDecoder().decode(Health.self, from: health))?.ok == true
        else { return nil }
        return info.url
    }

    static func httpGet(_ urlString: String) async -> Data? {
        guard let url = URL(string: urlString) else { return nil }
        var request = URLRequest(url: url, timeoutInterval: timeout)
        request.httpMethod = "GET"
        guard let (data, response) = try? await URLSession.shared.data(for: request),
              (response as? HTTPURLResponse)?.statusCode == 200
        else { return nil }
        return data
    }

    private struct HubInfo: Decodable { let url: String }
    private struct Health: Decodable { let ok: Bool }
}

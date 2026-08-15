import Foundation

/// Where the ccmuxd daemon lives and how this lens identifies itself to it.
///
/// Defaults to the local daemon (`http://127.0.0.1:7900`). At startup the app
/// asks that daemon who the federation hub is (`HubDiscovery`) and, if one is
/// found and healthy, retargets every service here via `discoveredHubURL`.
/// `CCMUXD_URL` is the explicit override that beats both (e.g.
/// `http://mbp.tailb9053d.ts.net:7900` to pin a specific host). The daemon
/// resolves verified identity via `tailscale whois`, so the `user`/`device`
/// query params are only a fallback for loopback/LAN.
enum DaemonConfig {
    /// The local daemon's origin — where hub discovery itself is asked, and the
    /// fallback when there is no hub. Some services pin themselves here even
    /// after a hub is adopted (see `PeerBrokerService`).
    static let localURL = "http://127.0.0.1:7900"

    /// The hub's base URL found by `HubDiscovery`; nil in single-host mode.
    /// Lock-guarded: discovery can adopt a hub while services are already
    /// reading `baseURL` from background tasks (cold boot retries land late).
    static var discoveredHubURL: String? {
        get { hubLock.withLock { _discoveredHubURL } }
        set { hubLock.withLock { _discoveredHubURL = newValue } }
    }
    private static let hubLock = NSLock()
    private nonisolated(unsafe) static var _discoveredHubURL: String?

    /// Base HTTP origin, no trailing slash. Priority: `CCMUXD_URL` env (explicit
    /// pin, no rebuild needed), then the discovered hub, then the local daemon.
    static var baseURL: String {
        resolvedBase(env: ProcessInfo.processInfo.environment["CCMUXD_URL"],
                     hub: discoveredHubURL)
    }

    /// Pure core of `baseURL`, split out for tests.
    static func resolvedBase(env: String?, hub: String?) -> String {
        let raw = env ?? hub ?? localURL
        return raw.hasSuffix("/") ? String(raw.dropLast()) : raw
    }

    /// WebSocket origin derived from `baseURL` (http→ws, https→wss).
    static var wsBaseURL: String { wsOrigin(baseURL) }

    /// http→ws / https→wss for any daemon origin (services pinned to
    /// `localURL` derive their socket origin from it with this).
    static func wsOrigin(_ base: String) -> String {
        if base.hasPrefix("https://") {
            return "wss://" + base.dropFirst("https://".count)
        }
        if base.hasPrefix("http://") {
            return "ws://" + base.dropFirst("http://".count)
        }
        return base
    }

    /// UserDefaults key for the developer identity override (Settings window).
    static let identityKey = "developerIdentity"

    /// The stored developer identity, "" when unset. Always already trimmed —
    /// `setIdentity` is the only writer.
    static var identity: String {
        UserDefaults.standard.string(forKey: identityKey) ?? ""
    }

    /// Persist a new identity (trimmed; empty clears the override). Returns
    /// whether it changed so the caller knows to re-dial the daemon sockets —
    /// the identity travels as a query param, so only a fresh dial presents it.
    /// Lives here, next to `resolvedUser`, so the write-side trim can never
    /// drift from the read-side trim.
    @discardableResult
    static func setIdentity(_ raw: String) -> Bool {
        let trimmed = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        guard trimmed != identity else { return false }
        if trimmed.isEmpty {
            UserDefaults.standard.removeObject(forKey: identityKey)
        } else {
            UserDefaults.standard.set(trimmed, forKey: identityKey)
        }
        return true
    }

    /// Self-declared name used only when the daemon can't verify identity over
    /// the tailnet (loopback). The configured developer identity — the Tailscale
    /// login email — wins when set: push subscriptions are keyed on that verified
    /// email, and suppression only works when this string and that key are the
    /// SAME string. `NSFullUserName()` is the fallback, which is why an
    /// unconfigured Mac needs a daemon-side identity alias to mute the phone.
    static var selfUser: String {
        resolvedUser(configured: UserDefaults.standard.string(forKey: identityKey),
                     fullName: NSFullUserName(), userName: NSUserName())
    }

    /// Pure core of `selfUser`, split out for tests (same shape as `resolvedBase`):
    /// configured identity (trimmed) beats the macOS full name beats the account name.
    static func resolvedUser(configured: String?, fullName: String, userName: String) -> String {
        let trimmed = configured?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        if !trimmed.isEmpty { return trimmed }
        return fullName.isEmpty ? userName : fullName
    }

    /// Device label for presence (this Mac's host name).
    static var selfDevice: String {
        ProcessInfo.processInfo.hostName
    }
}

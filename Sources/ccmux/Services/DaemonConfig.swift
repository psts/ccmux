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

    /// Self-declared name used only when the daemon can't verify identity over the
    /// tailnet (loopback). `NSFullUserName()` matches how a human reads presence.
    static var selfUser: String {
        let name = NSFullUserName()
        return name.isEmpty ? NSUserName() : name
    }

    /// Device label for presence (this Mac's host name).
    static var selfDevice: String {
        ProcessInfo.processInfo.hostName
    }
}

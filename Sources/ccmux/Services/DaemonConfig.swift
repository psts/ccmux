import Foundation

/// Where the ccmuxd daemon lives and how this lens identifies itself to it.
///
/// v1 defaults to the local daemon (`http://127.0.0.1:7900`); point `CCMUXD_URL`
/// at a tailnet MagicDNS origin (e.g. `http://mbp.tailb9053d.ts.net:7900`) to attach
/// to a remote host. The daemon resolves verified identity via `tailscale whois`, so
/// the `user`/`device` query params are only a fallback for loopback/LAN.
enum DaemonConfig {
    /// Base HTTP origin, no trailing slash. Env-overridable so a dev can retarget
    /// hosts without a rebuild.
    static var baseURL: String {
        let raw = ProcessInfo.processInfo.environment["CCMUXD_URL"] ?? "http://127.0.0.1:7900"
        return raw.hasSuffix("/") ? String(raw.dropLast()) : raw
    }

    /// WebSocket origin derived from `baseURL` (http→ws, https→wss).
    static var wsBaseURL: String {
        var url = baseURL
        if url.hasPrefix("https://") {
            url = "wss://" + url.dropFirst("https://".count)
        } else if url.hasPrefix("http://") {
            url = "ws://" + url.dropFirst("http://".count)
        }
        return url
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

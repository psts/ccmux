import XCTest
@testable import ccmux

/// The base-URL priority decides which daemon EVERY service talks to — an env
/// pin must beat the discovered hub, and the hub must beat the loopback default,
/// or hub discovery would either hijack an explicit override or do nothing.
final class DaemonConfigTests: XCTestCase {
    func testPriorityEnvBeatsHubBeatsLocal() {
        XCTAssertEqual(
            DaemonConfig.resolvedBase(env: "http://pinned:7900", hub: "https://hub.ts.net"),
            "http://pinned:7900")
        XCTAssertEqual(
            DaemonConfig.resolvedBase(env: nil, hub: "https://hub.ts.net"),
            "https://hub.ts.net")
        XCTAssertEqual(
            DaemonConfig.resolvedBase(env: nil, hub: nil),
            DaemonConfig.localURL)
    }

    func testTrailingSlashStrippedFromEverySource() {
        XCTAssertEqual(
            DaemonConfig.resolvedBase(env: "http://pinned:7900/", hub: nil),
            "http://pinned:7900")
        XCTAssertEqual(
            DaemonConfig.resolvedBase(env: nil, hub: "https://hub.ts.net/"),
            "https://hub.ts.net")
    }

    /// The identity string is what push suppression matches against the phone's
    /// verified login — a wrong resolution here fails silently (phone buzzes at
    /// the desk, or never buzzes), so its priority order is pinned like the URL's.
    func testResolvedUserPriorityConfiguredBeatsFullNameBeatsUserName() {
        XCTAssertEqual(
            DaemonConfig.resolvedUser(configured: "dev@example.com", fullName: "Patric S", userName: "patric"),
            "dev@example.com")
        XCTAssertEqual(
            DaemonConfig.resolvedUser(configured: " dev@example.com\n", fullName: "Patric S", userName: "patric"),
            "dev@example.com")
        XCTAssertEqual(
            DaemonConfig.resolvedUser(configured: nil, fullName: "Patric S", userName: "patric"),
            "Patric S")
        XCTAssertEqual(
            DaemonConfig.resolvedUser(configured: "   ", fullName: "", userName: "patric"),
            "patric")
    }
}

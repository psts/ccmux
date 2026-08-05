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
}

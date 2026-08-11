import XCTest
@testable import ccmux

/// The retry is what the `resolve()` tests never covered, and it is the half
/// that failed in the field: an app that launched before its daemon missed the
/// first attempt and then stayed on the local daemon for three days, blind to
/// every session on the rest of the fleet.
@MainActor
final class HubDiscoveryRetryTests: XCTestCase {
    override func tearDown() {
        DaemonConfig.discoveredHubURL = nil
        super.tearDown()
    }

    /// A hub that appears AFTER the first attempt must still be adopted, and the
    /// caller must be told so it can re-point services already running against
    /// the local daemon.
    func testHubAppearingLateIsStillAdopted() async {
        DaemonConfig.discoveredHubURL = nil
        var hubIsUp = false
        let fetch: (String) async -> Data? = { url in
            if url.hasSuffix("/v1/hub") {
                return #"{"url":"https://hub.ts.net"}"#.data(using: .utf8)
            }
            guard hubIsUp else { return nil }
            return #"{"ok":true}"#.data(using: .utf8)
        }

        // Before: the hub is unreachable, so the app stays local.
        var adopted = await HubDiscovery.attempt(fetch: fetch)
        XCTAssertFalse(adopted)
        XCTAssertNil(DaemonConfig.discoveredHubURL, "an unreachable hub must not be adopted")

        // The hub comes up. The very next attempt must take it.
        hubIsUp = true
        adopted = await HubDiscovery.attempt(fetch: fetch)
        XCTAssertTrue(adopted, "a hub that appeared after the first miss was never adopted")
        XCTAssertEqual(DaemonConfig.discoveredHubURL, "https://hub.ts.net")
    }

    /// Adoption is what stops the retry. If an attempt could report success
    /// without setting the base URL, the timer would stop and the app would sit
    /// on the local daemon forever believing it had federated.
    func testSuccessAlwaysSetsTheBaseURL() async {
        DaemonConfig.discoveredHubURL = nil
        let fetch: (String) async -> Data? = { url in
            url.hasSuffix("/v1/hub")
                ? #"{"url":"https://hub.ts.net"}"#.data(using: .utf8)
                : #"{"ok":true}"#.data(using: .utf8)
        }
        let adopted = await HubDiscovery.attempt(fetch: fetch)
        XCTAssertTrue(adopted)
        XCTAssertEqual(DaemonConfig.baseURL, "https://hub.ts.net",
                       "every service reads baseURL — adoption that does not move it is a no-op")
    }
}

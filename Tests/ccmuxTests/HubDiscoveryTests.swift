import XCTest
@testable import ccmux

/// resolve() decides whether the whole app silently retargets to another
/// machine — every failure path must land on "stay local" (nil), and only a
/// found-AND-healthy hub may be adopted.
final class HubDiscoveryTests: XCTestCase {
    private func canned(_ responses: [String: String?]) -> (String) async -> Data? {
        { url in
            for (suffix, body) in responses where url.hasSuffix(suffix) {
                return body?.data(using: .utf8)
            }
            return nil
        }
    }

    func testFoundAndHealthyHubIsAdopted() async {
        let fetch = canned([
            "/v1/hub": #"{"url":"https://hub.ts.net"}"#,
            "/v1/health": #"{"ok":true,"version":"0.1.4","contract":1}"#,
        ])
        let hub = await HubDiscovery.resolve(fetch: fetch)
        XCTAssertEqual(hub, "https://hub.ts.net")
    }

    func testNoHubYetStaysLocal() async {
        let hub = await HubDiscovery.resolve(fetch: canned(["/v1/hub": #"{"url":""}"#]))
        XCTAssertNil(hub)
    }

    func testUnreachableLocalDaemonStaysLocal() async {
        let hub = await HubDiscovery.resolve(fetch: canned([:]))
        XCTAssertNil(hub)
    }

    func testUnreachableHubIsNotAdopted() async {
        let fetch = canned(["/v1/hub": #"{"url":"https://hub.ts.net"}"#])
        let hub = await HubDiscovery.resolve(fetch: fetch)
        XCTAssertNil(hub)
    }

    func testUnhealthyHubIsNotAdopted() async {
        let fetch = canned([
            "/v1/hub": #"{"url":"https://hub.ts.net"}"#,
            "/v1/health": #"{"ok":false}"#,
        ])
        let hub = await HubDiscovery.resolve(fetch: fetch)
        XCTAssertNil(hub)
    }

    func testOldDaemonNonJSONAnswerStaysLocal() async {
        // A pre-/v1/hub daemon would 404; the transport maps that to nil, but a
        // proxy could still hand back HTML with a 200 — decode must reject it.
        let hub = await HubDiscovery.resolve(fetch: canned(["/v1/hub": "<html>not found</html>"]))
        XCTAssertNil(hub)
    }
}

import XCTest
@testable import ccmux

@MainActor
final class PaneFocusCoordinatorTests: XCTestCase {
    private var clock = Date(timeIntervalSince1970: 1_000)

    private func makeCoordinator() -> PaneFocusCoordinator {
        PaneFocusCoordinator.makeForTesting(now: { self.clock })
    }

    func testNoRequestMeansNoClaim() {
        let coordinator = makeCoordinator()
        XCTAssertFalse(coordinator.claim(tabId: UUID()))
    }

    func testTheRequestedTabClaims() {
        let coordinator = makeCoordinator()
        let tab = UUID()
        coordinator.requestFocus(tabId: tab)
        XCTAssertTrue(coordinator.claim(tabId: tab))
    }

    func testAnotherTabCannotClaimAndTheRequestSurvives() {
        let coordinator = makeCoordinator()
        let tab = UUID()
        coordinator.requestFocus(tabId: tab)
        XCTAssertFalse(coordinator.claim(tabId: UUID()))
        XCTAssertTrue(coordinator.claim(tabId: tab), "a bystander must not consume the request")
    }

    func testClaimingIsOneShot() {
        let coordinator = makeCoordinator()
        let tab = UUID()
        coordinator.requestFocus(tabId: tab)
        XCTAssertTrue(coordinator.claim(tabId: tab))
        XCTAssertFalse(coordinator.claim(tabId: tab), "a re-embed must not pull focus again")
    }

    func testAStaleRequestExpires() {
        let coordinator = makeCoordinator()
        let tab = UUID()
        coordinator.requestFocus(tabId: tab)
        clock += PaneFocusCoordinator.requestLifetime + 0.1
        XCTAssertFalse(coordinator.claim(tabId: tab))
    }

    func testARequestStillLiveAtTheDeadlineClaims() {
        let coordinator = makeCoordinator()
        let tab = UUID()
        coordinator.requestFocus(tabId: tab)
        clock += PaneFocusCoordinator.requestLifetime
        XCTAssertTrue(coordinator.claim(tabId: tab))
    }

    func testTheNewestRequestWins() {
        let coordinator = makeCoordinator()
        let first = UUID()
        let second = UUID()
        // The tab bar fires onFocus then onActivateTab; the tab actually clicked is last.
        coordinator.requestFocus(tabId: first)
        coordinator.requestFocus(tabId: second)
        XCTAssertFalse(coordinator.claim(tabId: first))
        XCTAssertTrue(coordinator.claim(tabId: second))
    }

    func testRequestIsBroadcastForAlreadyEmbeddedTerminals() {
        let coordinator = makeCoordinator()
        let tab = UUID()
        let received = expectation(description: "didRequestFocus posted with the tab id")
        let observer = NotificationCenter.default.addObserver(
            forName: PaneFocusCoordinator.didRequestFocus, object: nil, queue: nil
        ) { note in
            if note.object as? UUID == tab { received.fulfill() }
        }
        defer { NotificationCenter.default.removeObserver(observer) }

        coordinator.requestFocus(tabId: tab)
        wait(for: [received], timeout: 1)
    }
}

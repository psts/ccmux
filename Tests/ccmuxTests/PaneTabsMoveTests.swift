import XCTest
@testable import ccmux

/// Pins the slot arithmetic both lenses share: a slot is the number of chips
/// whose middle lies left of the pointer, the moving tab counted too.
final class PaneTabsMoveTests: XCTestCase {
    private func strip() -> (PaneTabs, [UUID]) {
        let tabs = (0..<4).map { _ in PaneContent.defaultTerminal(workingDirectory: "/repo") }
        return (PaneTabs(id: UUID(), tabs: tabs, activeTabId: tabs[1].id), tabs.map(\.id))
    }

    func testMoveRightSkipsItsOwnSlot() {
        var (pane, ids) = strip()
        XCTAssertTrue(pane.moveTab(tabId: ids[0], to: 3)) // before the 4th chip
        XCTAssertEqual(pane.tabs.map(\.id), [ids[1], ids[2], ids[0], ids[3]])
    }

    func testMoveLeftLandsBeforeTheSlot() {
        var (pane, ids) = strip()
        XCTAssertTrue(pane.moveTab(tabId: ids[3], to: 1))
        XCTAssertEqual(pane.tabs.map(\.id), [ids[0], ids[3], ids[1], ids[2]])
    }

    func testMoveToEndAndNoOps() {
        var (pane, ids) = strip()
        XCTAssertTrue(pane.moveTab(tabId: ids[0], to: 4)) // past the last chip
        XCTAssertEqual(pane.tabs.map(\.id), [ids[1], ids[2], ids[3], ids[0]])
        XCTAssertFalse(pane.moveTab(tabId: ids[0], to: 4)) // already last
        XCTAssertFalse(pane.moveTab(tabId: ids[0], to: 3)) // its own slot
        XCTAssertFalse(pane.moveTab(tabId: UUID(), to: 0)) // not in this strip
        XCTAssertEqual(pane.activeTabId, ids[1], "moving never changes which tab is active")
    }
}

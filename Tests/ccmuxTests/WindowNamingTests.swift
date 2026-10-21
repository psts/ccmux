import XCTest
@testable import ccmux

/// A window's name is its identity on the daemon: membership, open flags and
/// sizes all match on it. These pin that the name is fixed once, never
/// derived from where the window sits in the list (closing an earlier window
/// used to renumber the later ones, and a "Window 4" that became "Window 3"
/// shed every repo it held).
final class WindowNamingTests: XCTestCase {
    func testFreeNameIsTheLowestUnusedNumber() {
        XCTAssertEqual(WindowNaming.freeName(taken: []), "Window 1")
        XCTAssertEqual(WindowNaming.freeName(taken: ["Window 1", "Window 3"]), "Window 2")
        XCTAssertEqual(WindowNaming.freeName(taken: ["ChartLabs", "Window 1", "Window 2"]), "Window 3")
    }

    func testFreeNameIgnoresCase() {
        XCTAssertEqual(WindowNaming.freeName(taken: ["window 1", "WINDOW 2"]), "Window 3")
    }

    func testLaunchKeepsSavedNames() {
        XCTAssertEqual(WindowNaming.launchNames(saved: ["ChartLabs", "Dasha"]), ["ChartLabs", "Dasha"])
    }

    /// State saved by a build that named windows by position: each unnamed
    /// window takes the name it answered to then, which is the name its repos
    /// are grouped under on the daemon. Counted over the saved list, so a
    /// saved window that launch skips does not shift the ones after it.
    func testLaunchNamesAnOldUnnamedWindowByItsSavedPlace() {
        XCTAssertEqual(
            WindowNaming.launchNames(saved: ["ChartLabs", "Dasha", "Mixed", nil]),
            ["ChartLabs", "Dasha", "Mixed", "Window 4"])
        XCTAssertEqual(WindowNaming.launchNames(saved: [nil, nil]), ["Window 1", "Window 2"])
    }

    /// Positional names could collide with a custom one ("Window 2" typed by
    /// hand); two windows answering to one name means only the first gets the
    /// repos, so the unnamed one takes the next free number instead.
    func testLaunchNeverGivesTwoWindowsOneName() {
        XCTAssertEqual(
            WindowNaming.launchNames(saved: ["window 2", nil]),
            ["window 2", "Window 1"])
        XCTAssertEqual(
            WindowNaming.launchNames(saved: [nil, "Window 1"]),
            ["Window 2", "Window 1"])
    }

    /// The clashing window must not take a name a later unnamed window
    /// answered to: "Window 3" is the third window's, so the first one, whose
    /// "Window 1" is taken by hand, gets "Window 4".
    func testAClashNeverTakesALaterWindowsName() {
        XCTAssertEqual(
            WindowNaming.launchNames(saved: [nil, "Window 1", nil, "Window 2"]),
            ["Window 4", "Window 1", "Window 3", "Window 2"])
    }

    func testRenameRefusesAnEmptyName() {
        XCTAssertNotNil(WindowNaming.renameRefusal("", others: []))
        XCTAssertNotNil(WindowNaming.renameRefusal("   ", others: []))
    }

    func testRenameRefusesAnotherOpenWindowsName() {
        XCTAssertNotNil(WindowNaming.renameRefusal("dasha", others: ["Dasha"]))
        XCTAssertNil(WindowNaming.renameRefusal("Dasha 2", others: ["Dasha"]))
    }

    func testRestoreKeepsItsNameUnlessAnOpenWindowHasIt() {
        XCTAssertEqual(WindowNaming.restoreName(saved: "Mixed", open: ["ChartLabs"]), "Mixed")
        XCTAssertNil(WindowNaming.restoreName(saved: "mixed", open: ["Mixed"]))
        XCTAssertNil(WindowNaming.restoreName(saved: nil, open: []))
    }
}

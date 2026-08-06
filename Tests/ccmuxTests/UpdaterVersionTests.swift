import XCTest
@testable import ccmux

/// The version gate decides whether "Check for Updates…" offers anything at
/// all — and must never offer a "downgrade" to a source build that is ahead.
final class UpdaterVersionTests: XCTestCase {
    func testReleaseComparisons() {
        XCTAssertTrue(UpdaterService.isNewer("0.1.5", than: "0.1.4"))
        XCTAssertTrue(UpdaterService.isNewer("0.2.0", than: "0.1.9"))
        XCTAssertTrue(UpdaterService.isNewer("1.0.0", than: "0.9.9"))
        XCTAssertFalse(UpdaterService.isNewer("0.1.4", than: "0.1.4"))
        XCTAssertFalse(UpdaterService.isNewer("0.1.4", than: "0.1.5"))
    }

    func testSourceBuildStamps() {
        // git-describe stamps: numeric prefix decides; suffix never inflates.
        XCTAssertFalse(UpdaterService.isNewer("0.1.4", than: "0.1.4-2-gabc123-dirty"))
        XCTAssertTrue(UpdaterService.isNewer("0.1.5", than: "0.1.4-2-gabc123"))
        // A "dev" build has no numeric segments and any release beats it.
        XCTAssertTrue(UpdaterService.isNewer("0.1.4", than: "dev"))
    }

    func testShortAndPaddedSegments() {
        XCTAssertTrue(UpdaterService.isNewer("0.1.4.1", than: "0.1.4"))
        XCTAssertFalse(UpdaterService.isNewer("0.1", than: "0.1.0"))
    }

    func testAutoCheckSkipsSourceBuilds() {
        // "dev" has no numeric segment, so every release would "beat" it and the
        // automatic check would offer a downgrade on every launch — skip it.
        XCTAssertFalse(UpdaterService.autoCheckEligible("dev"))
        // git-describe stamps are local build-app.sh builds: a stamp behind the
        // latest tag would get nagged to overwrite itself with the release.
        XCTAssertFalse(UpdaterService.autoCheckEligible("0.1.4-2-gabc123"))
        XCTAssertFalse(UpdaterService.autoCheckEligible("0.1.4-dirty"))
        XCTAssertFalse(UpdaterService.autoCheckEligible("0.1.4-2-gabc123-dirty"))
        // Only release-shaped versions stay eligible.
        XCTAssertTrue(UpdaterService.autoCheckEligible("0.1.10"))
    }
}

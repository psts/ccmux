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

    /// Daemon-sync decision: only a clean release-shaped daemon OLDER than a
    /// release-shaped app gets upgraded — source builds on either side are
    /// never touched, and equal/newer daemons are left alone.
    func testShouldSyncDaemon() {
        XCTAssertTrue(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15", daemonVersion: "0.1.13"))
        XCTAssertFalse(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15", daemonVersion: "0.1.15"), "equal = in sync")
        XCTAssertFalse(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15", daemonVersion: "0.1.16"), "never downgrade a newer daemon")
        XCTAssertFalse(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15", daemonVersion: "0.1.13-dirty"), "dirty source build is sacred")
        XCTAssertFalse(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15", daemonVersion: "0.1.13-2-gabc123"), "git-describe stamp is a source build")
        XCTAssertFalse(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15", daemonVersion: "dev"), "dev build is a source build")
        XCTAssertFalse(UpdaterService.shouldSyncDaemon(appVersion: "0.1.15-dirty", daemonVersion: "0.1.13"), "a dev APP must not drive upgrades")
    }

    /// Fleet selection: healthy + release-shaped + strictly older, nothing else.
    func testHostsNeedingUpgrade() {
        typealias H = UpdaterService.FleetHost
        let hosts = [
            H(id: "sanlabs", version: "0.1.13", healthy: true),      // older → upgrade
            H(id: "current", version: "0.1.15", healthy: true),      // equal → skip
            H(id: "newer", version: "0.1.16", healthy: true),        // newer → skip
            H(id: "dev-box", version: "0.1.13-dirty", healthy: true),// source build → skip
            H(id: "downbox", version: "0.1.13", healthy: false),     // unreachable → skip
            H(id: "asleep", version: nil, healthy: true),            // no probed version → skip
        ]
        XCTAssertEqual(UpdaterService.hostsNeedingUpgrade(appVersion: "0.1.15", hosts: hosts), ["sanlabs"])
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

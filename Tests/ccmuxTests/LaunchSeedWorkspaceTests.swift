import XCTest
@testable import ccmux

/// The launch-time "seed a workspace from cwd" decision. Getting this wrong is what
/// produced a phantom workspace named "/" in the first window's sidebar on every reopen.
final class LaunchSeedWorkspaceTests: XCTestCase {

    private func descriptor() -> WindowDescriptor {
        WindowDescriptor(id: UUID(), workspaceId: UUID(), frame: WindowFrame(x: 0, y: 0, width: 800, height: 600))
    }

    private func workspace(_ path: String) -> Workspace {
        Workspace.create(name: (path as NSString).lastPathComponent, repoPath: path)
    }

    func testFirstRunSeedsFromCwd() {
        XCTAssertEqual(
            AppDelegate.launchSeedRepoPath(savedWindows: [], localWorkspaces: [], cwd: "/Users/me/proj"),
            "/Users/me/proj"
        )
    }

    func testRootCwdNeverSeeds() {
        // LaunchServices (Dock/Finder/login item) hands the app cwd "/". Seeding from it
        // makes a workspace named "/" rooted at the filesystem root.
        XCTAssertNil(AppDelegate.launchSeedRepoPath(savedWindows: [], localWorkspaces: [], cwd: "/"))
    }

    func testRestoredSessionNeverSeeds() {
        // Hosted workspaces arrive from ccmuxd after launch, so zero local workspaces is
        // normal for a returning user. Saved windows are the "not a first run" signal.
        XCTAssertNil(
            AppDelegate.launchSeedRepoPath(savedWindows: [descriptor()], localWorkspaces: [], cwd: "/Users/me/proj")
        )
    }

    func testExistingLocalWorkspaceNeverSeeds() {
        XCTAssertNil(
            AppDelegate.launchSeedRepoPath(
                savedWindows: [], localWorkspaces: [workspace("/Users/me/proj")], cwd: "/Users/me/other")
        )
    }
}

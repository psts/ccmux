import XCTest
@testable import ccmux

final class WorkspaceCwdResolutionTests: XCTestCase {

    func testExactMatch() {
        let repos = ["/Users/me/proj-a", "/Users/me/proj-b"]
        XCTAssertEqual(WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/proj-b", repoPaths: repos), 1)
    }

    func testSubdirectoryMatchesRepo() {
        // A Claude session running deep inside the repo still maps to it.
        let repos = ["/Users/me/proj-a"]
        XCTAssertEqual(
            WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/proj-a/src/deep/dir", repoPaths: repos),
            0
        )
    }

    func testLongestPrefixWinsForNestedWorkspaces() {
        // Parent and a nested child are both open; the more specific one wins.
        let repos = ["/Users/me/proj", "/Users/me/proj/packages/inner"]
        XCTAssertEqual(
            WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/proj/packages/inner/src", repoPaths: repos),
            1
        )
        // A path only under the parent maps to the parent.
        XCTAssertEqual(
            WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/proj/other", repoPaths: repos),
            0
        )
    }

    func testNoMatchReturnsNil() {
        let repos = ["/Users/me/proj-a"]
        XCTAssertNil(WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/elsewhere", repoPaths: repos))
    }

    func testSiblingPrefixIsNotAFalseMatch() {
        // "/Users/me/proj" must NOT match cwd "/Users/me/proj-x" (component boundary).
        let repos = ["/Users/me/proj"]
        XCTAssertNil(WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/proj-x", repoPaths: repos))
    }

    func testTrailingSlashAndTildeNormalized() {
        let repos = ["/Users/me/proj/"]
        XCTAssertEqual(WorkspaceManager.bestRepoMatchIndex(cwd: "/Users/me/proj", repoPaths: repos), 0)

        let home = NSHomeDirectory()
        let tildeRepos = ["~/proj"]
        XCTAssertEqual(
            WorkspaceManager.bestRepoMatchIndex(cwd: "\(home)/proj/sub", repoPaths: tildeRepos),
            0
        )
    }
}

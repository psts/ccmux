import XCTest
@testable import ccmux

/// A canned in-memory FileSource for explorer tests.
private struct StubFileSource: FileSource {
    var files: [String: String] = [:]
    var watchesDisk: Bool { false }
    func read(path: String) async -> String? { files[path] }
    func write(path: String, content: String) async -> Bool { files[path] != nil }
    func list(path: String) async -> [FileSourceEntry]? { nil }
}

/// Pins remote link classification (`RemoteTermController.linkAction`) and the
/// reveal-in-explorer flow: a clicked link must never leave a junk Files tab
/// behind when the file doesn't exist on the daemon's host.
final class FileExplorerSourceTests: XCTestCase {

    // MARK: - linkAction

    func testHttpLinksOpenExternally() {
        let action = RemoteTermController.linkAction("https://example.com/a", workingDirectory: "/r")
        XCTAssertEqual(action, .openExternal(URL(string: "https://example.com/a")!))
    }

    func testCustomSchemeOpensExternally() {
        let action = RemoteTermController.linkAction("vscode://file/x", workingDirectory: "/r")
        XCTAssertEqual(action, .openExternal(URL(string: "vscode://file/x")!))
    }

    func testRelativePathWithLineSuffixResolvesAgainstCwd() {
        let action = RemoteTermController.linkAction("docs/plan.md:12", workingDirectory: "/repo")
        XCTAssertEqual(action, .openFile("/repo/docs/plan.md"))
    }

    func testBareFilenameWithColonIsNotMisreadAsScheme() {
        // URL(string:) would parse "file.md:3" as scheme "file.md" — must stay a file.
        let action = RemoteTermController.linkAction("file.md:3", workingDirectory: "/repo")
        XCTAssertEqual(action, .openFile("/repo/file.md"))
    }

    func testFileURLStripsSchemeAndTrailingPunctuation() {
        let action = RemoteTermController.linkAction("file:///repo/readme.md.", workingDirectory: "/r")
        XCTAssertEqual(action, .openFile("/repo/readme.md"))
    }

    // MARK: - revealFileInExplorer

    private func waitUntil(_ cond: @escaping () -> Bool) async {
        for _ in 0..<200 {
            if cond() { return }
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
    }

    private func explorerTabCount(_ c: SplitTreeController) -> Int {
        c.tree.allLeaves.flatMap { $0.content.tabs }.filter {
            if case .fileExplorer = $0 { return true } else { return false }
        }.count
    }

    func testRevealMissingFileCreatesNoTab() async {
        let c = SplitTreeController(workingDirectory: "/repo")
        c.fileSource = StubFileSource()
        c.revealFileInExplorer(relativePath: "/repo/nope.md")
        // Give the probe task time to complete, then confirm nothing appeared.
        try? await Task.sleep(nanoseconds: 100_000_000)
        XCTAssertEqual(explorerTabCount(c), 0, "a failed read must not leave a junk Files tab")
    }

    func testRevealExistingFileCreatesExplorerAndOpensIt() async {
        let c = SplitTreeController(workingDirectory: "/repo")
        c.fileSource = StubFileSource(files: ["/repo/readme.md": "# hi"])
        c.revealFileInExplorer(relativePath: "/repo/readme.md")
        await waitUntil { self.explorerTabCount(c) == 1 }
        XCTAssertEqual(explorerTabCount(c), 1)
        let state = c.fileExplorerStates.values.first
        await waitUntil { state?.openTabs.isEmpty == false }
        XCTAssertEqual(state?.openTabs.first?.relativePath, "/repo/readme.md")
        XCTAssertEqual(state?.openTabs.first?.content, "# hi")
    }

    func testRevealReusesExistingExplorerTab() async {
        let c = SplitTreeController(workingDirectory: "/repo")
        c.fileSource = StubFileSource(files: ["/repo/a.md": "a", "/repo/b.md": "b"])
        c.revealFileInExplorer(relativePath: "/repo/a.md")
        await waitUntil { self.explorerTabCount(c) == 1 }
        c.revealFileInExplorer(relativePath: "/repo/b.md")
        let state = c.fileExplorerStates.values.first
        await waitUntil { state?.openTabs.count == 2 }
        XCTAssertEqual(explorerTabCount(c), 1, "second reveal must reuse the existing Files tab")
        XCTAssertEqual(state?.openTabs.count, 2)
    }

    // MARK: - FileExplorerState restore ordering

    func testRestoreOpensFilesAndReappliesActiveTab() async {
        let source = StubFileSource(files: ["a.md": "A", "b.md": "B"])
        let state = FileExplorerState(rootPath: "/repo", source: source)
        let config = FileExplorerConfig(
            id: UUID(), rootPath: "/repo",
            openFilePaths: ["a.md", "b.md"], activeFilePath: "a.md")
        state.restoreFromConfig(config)
        await waitUntil { state.openTabs.count == 2 }
        XCTAssertEqual(state.openTabs.map(\.relativePath), ["a.md", "b.md"])
        let active = state.openTabs.first(where: { $0.id == state.activeTabId })
        XCTAssertEqual(active?.relativePath, "a.md", "active tab must survive async restore")
    }

    func testFailedSaveMarksTabAndKeepsItModified() async {
        let source = StubFileSource(files: ["a.md": "A"])
        let state = FileExplorerState(rootPath: "/repo", source: source)
        state.openFile(relativePath: "a.md")
        await waitUntil { !state.openTabs.isEmpty }
        state.updateContent(tabId: state.openTabs[0].id, newContent: "changed")
        // Stub write succeeds only for known paths; remove to force failure.
        // (StubFileSource.write checks files[path] != nil — use a fresh state
        // pointing at an empty stub to simulate the daemon rejecting the save.)
        let failing = FileExplorerState(rootPath: "/repo", source: StubFileSource())
        failing.openTabs = state.openTabs
        failing.activeTabId = state.openTabs[0].id
        failing.saveActiveFile()
        await waitUntil { failing.openTabs.first?.saveErrored == true }
        XCTAssertEqual(failing.openTabs.first?.saveErrored, true)
        XCTAssertEqual(failing.openTabs.first?.isModified, true, "content must not be marked clean on a failed save")
    }
}

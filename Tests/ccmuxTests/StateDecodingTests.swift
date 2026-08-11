import XCTest
@testable import ccmux

/// state.json decoding. The behavior under test: one corrupt entry costs you that
/// entry, not the whole file (every other workspace, both closed lists, and the
/// entire window/Space layout).
final class StateDecodingTests: XCTestCase {

    // MARK: - Fixtures

    private func encoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        return e
    }

    private func makeState(workspaceNames: [String]) -> AppState {
        AppState(
            workspaces: workspaceNames.map { Workspace.create(name: $0, repoPath: "/tmp/\($0)") },
            closedWorkspaces: [Workspace.create(name: "closed", repoPath: "/tmp/closed")],
            closedWindows: [ClosedWindow(
                id: UUID(), windowName: "Old", workspaceIds: [UUID()],
                displayedWorkspaceId: nil, frame: WindowFrame(x: 0, y: 0, width: 10, height: 10))],
            activeWorkspaceId: nil,
            version: 2,
            windowFrame: nil,
            windows: [WindowDescriptor(
                id: UUID(), workspaceId: nil, ownedWorkspaceIds: [], windowName: "W",
                frame: WindowFrame(x: 1, y: 2, width: 3, height: 4))]
        )
    }

    /// Encode a state, then replace `layout` with a non-object on the workspaces at
    /// `indices` — the realistic corruption (a pane type an older build can't decode).
    private func encodeCorrupting(_ state: AppState, workspaceIndices indices: [Int]) throws -> Data {
        let data = try encoder().encode(state)
        var json = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        var workspaces = try XCTUnwrap(json["workspaces"] as? [[String: Any]])
        for i in indices { workspaces[i]["layout"] = "not-a-tree" }
        json["workspaces"] = workspaces
        return try JSONSerialization.data(withJSONObject: json)
    }

    private func names(_ outcome: PersistenceService.LoadOutcome) -> [String] {
        (outcome.state?.workspaces ?? []).map(\.name)
    }

    /// Which case came back. `LoadOutcome` is deliberately not Equatable — its
    /// payload isn't — so assertions compare this instead.
    private func caseName(_ outcome: PersistenceService.LoadOutcome) -> String {
        switch outcome {
        case .clean: return "clean"
        case .partial: return "partial"
        case .legacy: return "legacy"
        case .failed: return "failed"
        }
    }

    // MARK: - Tests

    func testCleanStateRoundTrips() throws {
        let data = try encoder().encode(makeState(workspaceNames: ["a", "b"]))
        let outcome = PersistenceService.decode(data)
        XCTAssertEqual(caseName(outcome), "clean")
        XCTAssertEqual(names(outcome), ["a", "b"])
        XCTAssertEqual(outcome.state?.closedWorkspaces.count, 1)
        XCTAssertEqual(outcome.state?.windows.count, 1)
    }

    func testOneCorruptWorkspaceDoesNotTakeTheFile() throws {
        let data = try encodeCorrupting(makeState(workspaceNames: ["a", "bad", "c"]), workspaceIndices: [1])
        let outcome = PersistenceService.decode(data)

        XCTAssertEqual(names(outcome), ["a", "c"], "the two intact workspaces must survive")
        guard case .partial(_, let summary) = outcome else {
            return XCTFail("expected .partial, got \(outcome)")
        }
        XCTAssertEqual(summary, "workspaces: 1 of 3 dropped")
        // The rest of the file must be untouched by one bad workspace.
        XCTAssertEqual(outcome.state?.closedWorkspaces.count, 1)
        XCTAssertEqual(outcome.state?.closedWindows.count, 1)
        XCTAssertEqual(outcome.state?.windows.first?.windowName, "W")
    }

    func testEveryWorkspaceCorruptStillKeepsWindowLayout() throws {
        // A whole-array loss first retries the legacy migration; when that fails too,
        // the surviving window/closed state is still better than returning nil.
        let data = try encodeCorrupting(makeState(workspaceNames: ["a", "b"]), workspaceIndices: [0, 1])
        let outcome = PersistenceService.decode(data)

        XCTAssertEqual(names(outcome), [])
        XCTAssertEqual(outcome.state?.windows.count, 1)
        XCTAssertEqual(outcome.state?.closedWorkspaces.count, 1)
    }

    func testCorruptWindowDescriptorKeepsWorkspaces() throws {
        let data = try encoder().encode(makeState(workspaceNames: ["a"]))
        var json = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        json["windows"] = [["id": "not-a-uuid"]]
        let outcome = PersistenceService.decode(try JSONSerialization.data(withJSONObject: json))

        XCTAssertEqual(names(outcome), ["a"])
        XCTAssertEqual(outcome.state?.windows.count, 0)
        guard case .partial(_, let summary) = outcome else {
            return XCTFail("expected .partial, got \(outcome)")
        }
        XCTAssertEqual(summary, "windows: 1 of 1 dropped")
    }

    func testMissingWorkspacesKeyIsAFailureNotAnEmptyState() throws {
        // "workspaces": absent is a file we failed to understand, not "you have no
        // workspaces". Reading it as empty would let the launch autosave overwrite the
        // original with nothing, and skip the backup — the exact loss this guards.
        let data = try encoder().encode(makeState(workspaceNames: ["a"]))
        var json = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        json.removeValue(forKey: "workspaces")
        XCTAssertEqual(caseName(PersistenceService.decode(try JSONSerialization.data(withJSONObject: json))), "failed")

        json["workspaces"] = NSNull()
        XCTAssertEqual(caseName(PersistenceService.decode(try JSONSerialization.data(withJSONObject: json))), "failed")
    }

    func testGarbageFails() {
        XCTAssertEqual(caseName(PersistenceService.decode(Data("{".utf8))), "failed")
        XCTAssertNil(PersistenceService.decode(Data("[]".utf8)).state)
    }

    func testLegacyLayoutTakesTheMigrationPathNotALossyEmptyDecode() throws {
        // The pre-tabs schema stored one PaneContent per leaf. Under a lenient decode
        // that shape drops every workspace, which must route to the migration rather
        // than being accepted as "you have no workspaces".
        let leafId = UUID()
        let legacyTree: SplitTree<PaneContent> = .leaf(
            id: leafId, content: .defaultTerminal(workingDirectory: "/tmp/legacy"))
        let treeJSON = try JSONSerialization.jsonObject(with: try encoder().encode(legacyTree))

        let json: [String: Any] = [
            "workspaces": [[
                "id": UUID().uuidString,
                "name": "legacy",
                "repoPath": "/tmp/legacy",
                "layout": treeJSON,
                "subItems": [],
                "lastOpened": "2026-01-01T00:00:00Z",
            ]],
            "closedWorkspaces": [],
            "closedWindows": [],
            "version": 2,
            "windows": [],
        ]
        let outcome = PersistenceService.decode(try JSONSerialization.data(withJSONObject: json))

        guard case .legacy = outcome else { return XCTFail("expected .legacy, got \(outcome)") }
        XCTAssertEqual(names(outcome), ["legacy"])
        // The migration wraps the leaf's content in a single-tab PaneTabs, preserving the id.
        XCTAssertEqual(outcome.state?.workspaces.first?.layout.allLeaves.first?.id, leafId)
        XCTAssertEqual(outcome.state?.workspaces.first?.layout.allLeaves.first?.content.tabs.count, 1)
    }

    func testLegacyRetryLooksBeyondTheOpenWorkspaceList() throws {
        // A pre-tabs file whose open list happens to be empty is still a legacy file,
        // and its closed workspaces are still recoverable. Gating the migration retry
        // on `workspaces` alone dropped every closed entry of such a file.
        let leafId = UUID()
        let legacyTree: SplitTree<PaneContent> = .leaf(
            id: leafId, content: .defaultTerminal(workingDirectory: "/tmp/old"))
        let treeJSON = try JSONSerialization.jsonObject(with: try encoder().encode(legacyTree))

        let json: [String: Any] = [
            "workspaces": [],
            "closedWorkspaces": [[
                "id": UUID().uuidString,
                "name": "old",
                "repoPath": "/tmp/old",
                "layout": treeJSON,
                "subItems": [],
                "lastOpened": "2026-01-01T00:00:00Z",
            ]],
            "closedWindows": [],
            "version": 2,
            "windows": [],
        ]
        let outcome = PersistenceService.decode(try JSONSerialization.data(withJSONObject: json))

        XCTAssertEqual(caseName(outcome), "legacy")
        XCTAssertEqual(outcome.state?.closedWorkspaces.map(\.name), ["old"])
    }

    func testEmptyWorkspacesArrayIsCleanNotALoss() throws {
        // The hosted-only case: every session lives in ccmuxd, so the local array is
        // legitimately empty. That must not look like a whole-array loss.
        let data = try encoder().encode(makeState(workspaceNames: []))
        let outcome = PersistenceService.decode(data)
        XCTAssertEqual(caseName(outcome), "clean")
        XCTAssertEqual(names(outcome), [])
    }
}

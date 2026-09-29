import AppKit
import XCTest
@testable import ccmux

/// Pins where a shared window opens: this Mac's own size, else the size the
/// daemon carries, else the standard one, fitted to the screen; and when a
/// size is worth telling the daemon about.
final class WindowSizingTests: XCTestCase {
    private func window(_ json: String) throws -> DaemonWindow {
        try JSONDecoder().decode(DaemonWindow.self, from: Data(json.utf8))
    }

    private func memory() -> SharedWindowSizeMemory {
        let suite = "WindowSizingTests-\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        addTeardownBlock { defaults.removePersistentDomain(forName: suite) }
        return SharedWindowSizeMemory(defaults: defaults)
    }

    func testMineBeatsSharedBeatsStandard() {
        let mine = NSSize(width: 1800, height: 1100)
        let shared = NSSize(width: 1300, height: 850)
        XCTAssertEqual(WindowSizing.pick(mine: mine, shared: shared), mine)
        XCTAssertEqual(WindowSizing.pick(mine: nil, shared: shared), shared)
        XCTAssertEqual(WindowSizing.pick(mine: nil, shared: nil), WindowSizing.standard)
    }

    func testFramesFitTheScreenAndCenter() {
        let visible = NSRect(x: 0, y: 25, width: 1440, height: 875)
        let min = NSSize(width: 600, height: 400)
        let big = WindowSizing.centeredFrame(NSSize(width: 2500, height: 1400), in: visible, minSize: min)
        XCTAssertEqual(big.size, visible.size, "a bigger screen's size shrinks to this one")
        let normal = WindowSizing.centeredFrame(NSSize(width: 1200, height: 800), in: visible, minSize: min)
        XCTAssertEqual(normal, NSRect(x: 120, y: 63, width: 1200, height: 800))
        let tiny = WindowSizing.centeredFrame(NSSize(width: 100, height: 100), in: visible, minSize: min)
        XCTAssertEqual(tiny.size, min)
    }

    func testDaemonSizeIsOptional() throws {
        let sized = try window(#"{"id":"a","name":"A","width":1500,"height":900}"#)
        XCTAssertEqual(sized.sharedSize, NSSize(width: 1500, height: 900))
        XCTAssertNil(try window(#"{"id":"a","name":"A"}"#).sharedSize, "a daemon older than sizes sends none")
        XCTAssertNil(try window(#"{"id":"a","name":"A","width":0,"height":900}"#).sharedSize)
    }

    func testMemoryKeepsOneSizePerWindow() {
        let mem = memory()
        XCTAssertNil(mem.size(for: "a"))
        mem.remember(NSSize(width: 1600, height: 1000), for: "a")
        mem.remember(NSSize(width: 900, height: 700), for: "b")
        mem.remember(NSSize(width: 1700, height: 1050), for: "a")
        XCTAssertEqual(mem.size(for: "a"), NSSize(width: 1700, height: 1050))
        XCTAssertEqual(mem.size(for: "b"), NSSize(width: 900, height: 700))
    }

    /// Opening is not resizing: it publishes nothing. A new size publishes
    /// once, an unchanged one never, full screen never.
    func testOnlyARealNewSizeIsPublished() throws {
        var published: [NSSize] = []
        let mem = memory()
        let sizer = SharedWindowSizer(memory: mem) { _, size in published.append(size) }
        let opened = NSSize(width: 1500, height: 900)
        sizer.opened("a", at: opened)
        sizer.settled("a", size: opened, fullScreen: false)
        XCTAssertEqual(published, [])
        XCTAssertEqual(mem.size(for: "a"), opened, "opening sets this Mac's size for it")

        let bigger = NSSize(width: 1800, height: 1000)
        sizer.settled("a", size: bigger, fullScreen: false)
        sizer.settled("a", size: bigger, fullScreen: false)
        sizer.settled("a", size: NSSize(width: 2560, height: 1440), fullScreen: true)
        XCTAssertEqual(published, [bigger])
        XCTAssertEqual(mem.size(for: "a"), bigger)
    }

    /// Opening picks this Mac's size over the daemon's.
    func testOpeningFramePrefersThisMac() throws {
        let mem = memory()
        let sizer = SharedWindowSizer(memory: mem) { _, _ in }
        let win = try window(#"{"id":"a","name":"A","width":1300,"height":850}"#)
        let visible = NSRect(x: 0, y: 0, width: 2560, height: 1400)
        let min = NSSize(width: 600, height: 400)
        XCTAssertEqual(sizer.openingFrame(for: win, in: visible, minSize: min).size, NSSize(width: 1300, height: 850))
        mem.remember(NSSize(width: 1900, height: 1200), for: "a")
        XCTAssertEqual(sizer.openingFrame(for: win, in: visible, minSize: min).size, NSSize(width: 1900, height: 1200))
    }

    /// A stamp wins while the daemon still has it; then the window's own name,
    /// then its sessions. An unloaded list trusts the stamp.
    func testSharedWindowIdResolution() throws {
        let shared = [
            try window(#"{"id":"w-dasha","name":"Dasha","workspaceIds":["ws-1"]}"#),
            try window(#"{"id":"w-hq","name":"HQ","workspaceIds":["ws-2"]}"#),
        ]
        let ws2: Set<UUID> = [RemoteWorkspaceBuilder.workspaceUUID("ws-2")]
        XCTAssertEqual(WindowSizing.sharedWindowId(stamped: "w-dasha", name: nil, members: ws2, shared: shared), "w-dasha")
        XCTAssertEqual(WindowSizing.sharedWindowId(stamped: "w-gone", name: "dasha", members: ws2, shared: shared), "w-dasha")
        XCTAssertEqual(WindowSizing.sharedWindowId(stamped: nil, name: nil, members: ws2, shared: shared), "w-hq")
        XCTAssertEqual(WindowSizing.sharedWindowId(stamped: nil, name: "Nope", members: ws2, shared: shared), "w-hq")
        XCTAssertNil(WindowSizing.sharedWindowId(stamped: nil, name: nil, members: [], shared: shared))
        XCTAssertEqual(WindowSizing.sharedWindowId(stamped: "w-gone", name: nil, members: [], shared: []), "w-gone")
    }

    /// An unnamed window answers to an automatic "Window N" that shifts as
    /// other windows close. Matching on it would hand one window's size to a
    /// shared window that happens to carry that name today. Runs the same
    /// helper `sharedWindowSizeSettled` calls, with the clash set up.
    func testAnUnnamedWindowNeverMatchesByTheAutomaticName() throws {
        let shared = [
            try window(#"{"id":"w-3","name":"Window 3","workspaceIds":["ws-9"]}"#),
            try window(#"{"id":"w-mine","name":"Mine","workspaceIds":["ws-1"]}"#),
        ]
        let wc = WindowContext(workspaceId: nil, workspaceManager: WorkspaceManager(autosaves: false))
        wc.autoName = "Window 3"
        XCTAssertNil(WindowSizing.sharedWindowId(for: wc, shared: shared),
                     "the automatic name must never match")
        wc.displayedWorkspaceId = RemoteWorkspaceBuilder.workspaceUUID("ws-1")
        XCTAssertEqual(WindowSizing.sharedWindowId(for: wc, shared: shared), "w-mine",
                       "the displayed session counts as a member")
        wc.windowName = "window 3"
        XCTAssertEqual(WindowSizing.sharedWindowId(for: wc, shared: shared), "w-3",
                       "a name of its own does match")
    }
}

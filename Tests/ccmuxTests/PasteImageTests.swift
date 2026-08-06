import XCTest
import AppKit
@testable import ccmux

/// Pins the pasteboard image extraction behind hosted-pane Cmd+V: text stays
/// text (nil → SwiftTerm's normal paste), images come out as bytes suitable
/// for the daemon paste upload.
final class PasteImageTests: XCTestCase {

    private func freshPasteboard() -> NSPasteboard {
        let pb = NSPasteboard(name: NSPasteboard.Name("ccmux-test-\(UUID().uuidString)"))
        pb.clearContents()
        return pb
    }

    /// A real 1x1 PNG (red pixel) so NSBitmapImageRep round-trips work.
    private func onePixelPNG() -> Data {
        let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: 1, pixelsHigh: 1,
                                   bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true,
                                   isPlanar: false, colorSpaceName: .deviceRGB,
                                   bytesPerRow: 4, bitsPerPixel: 32)!
        rep.setColor(.red, atX: 0, y: 0)
        return rep.representation(using: .png, properties: [:])!
    }

    func testTextOnlyPasteboardYieldsNil() {
        let pb = freshPasteboard()
        pb.setString("just text", forType: .string)
        XCTAssertNil(ClickThroughRemoteTerminalView.imageData(from: pb))
    }

    func testEmptyPasteboardYieldsNil() {
        XCTAssertNil(ClickThroughRemoteTerminalView.imageData(from: freshPasteboard()))
    }

    func testPNGDataPassesThrough() {
        let pb = freshPasteboard()
        let png = onePixelPNG()
        pb.setData(png, forType: .png)
        XCTAssertEqual(ClickThroughRemoteTerminalView.imageData(from: pb), png)
    }

    func testTIFFConvertsToPNG() {
        let pb = freshPasteboard()
        let tiff = NSBitmapImageRep(data: onePixelPNG())!.tiffRepresentation!
        pb.setData(tiff, forType: .tiff)
        let out = ClickThroughRemoteTerminalView.imageData(from: pb)
        XCTAssertNotNil(out)
        XCTAssertTrue(out!.starts(with: [0x89, 0x50, 0x4E, 0x47]), "TIFF must come out re-encoded as PNG")
    }

    func testTextBeatsRiddenAlongImageRendition() {
        // Spreadsheet cells / rich-text copies carry a picture rendition next
        // to the text — Cmd+V must stay a TEXT paste there.
        let pb = freshPasteboard()
        pb.setString("A1\tB1", forType: .string)
        pb.setData(NSBitmapImageRep(data: onePixelPNG())!.tiffRepresentation!, forType: .tiff)
        XCTAssertNil(ClickThroughRemoteTerminalView.imageData(from: pb))
    }

    func testImageFileURLBeatsItsFilenameString() throws {
        // Finder puts the filename on the string flavor next to the fileURL —
        // the file's bytes must still win.
        let png = onePixelPNG()
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("ccmux-paste-test-\(UUID().uuidString).png")
        try png.write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }

        let pb = freshPasteboard()
        XCTAssertTrue(pb.writeObjects([url as NSURL]))
        pb.setString(url.lastPathComponent, forType: .string)
        XCTAssertEqual(ClickThroughRemoteTerminalView.imageData(from: pb), png)
    }

    func testImageFileURLReadsOriginalBytes() throws {
        let png = onePixelPNG()
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("ccmux-paste-test-\(UUID().uuidString).png")
        try png.write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }

        let pb = freshPasteboard()
        XCTAssertTrue(pb.writeObjects([url as NSURL]))
        XCTAssertEqual(ClickThroughRemoteTerminalView.imageData(from: pb), png)
    }

    func testNonImageFileURLYieldsNil() throws {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("ccmux-paste-test-\(UUID().uuidString).txt")
        try Data("hello".utf8).write(to: url)
        defer { try? FileManager.default.removeItem(at: url) }

        let pb = freshPasteboard()
        XCTAssertTrue(pb.writeObjects([url as NSURL]))
        XCTAssertNil(ClickThroughRemoteTerminalView.imageData(from: pb))
    }
}

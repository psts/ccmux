import AppKit

// MARK: - CGS Private API Declarations

// These private APIs are used by Amethyst, yabai, Rectangle, and many other
// macOS window management tools. They've been stable since macOS 10.x.

private typealias CGSConnectionID = Int32
private typealias CGSSpaceID = size_t

@_silgen_name("CGSMainConnectionID")
private func CGSMainConnectionID() -> CGSConnectionID

@_silgen_name("CGSGetActiveSpace")
private func CGSGetActiveSpace(_ cid: CGSConnectionID) -> CGSSpaceID

@_silgen_name("CGSCopySpacesForWindows")
private func CGSCopySpacesForWindows(
    _ cid: CGSConnectionID,
    _ mask: UInt32,
    _ wids: CFArray
) -> Unmanaged<CFArray>?

@_silgen_name("CGSSpaceGetType")
private func CGSSpaceGetType(_ cid: CGSConnectionID, _ sid: CGSSpaceID) -> UInt32

@_silgen_name("CGSCopyManagedDisplaySpaces")
private func CGSCopyManagedDisplaySpaces(_ cid: CGSConnectionID) -> CFArray?

@_silgen_name("CGSAddWindowsToSpaces")
private func CGSAddWindowsToSpaces(
    _ cid: CGSConnectionID,
    _ wids: CFArray,
    _ sids: CFArray
)

@_silgen_name("CGSRemoveWindowsFromSpaces")
private func CGSRemoveWindowsFromSpaces(
    _ cid: CGSConnectionID,
    _ wids: CFArray,
    _ sids: CFArray
)

// Space type: user = 0
private let kCGSSpaceTypeUser: UInt32 = 0
// Mask: allSpaces = includesUser | includesOthers | includesCurrent
private let kCGSSpaceMaskAll: UInt32 = 0x07

// MARK: - Space Snapshot (for persistence)

/// Stores a window's Space identity as (displayUUID, ordinal) — these survive
/// across app launches, unlike raw CGSSpaceIDs which change every session.
struct SpaceSnapshot: Codable, Equatable {
    var displayUUID: String?
    var ordinal: Int
}

// MARK: - SpaceTracker

/// Manages macOS Spaces detection and window placement using CGS private APIs.
enum SpaceTracker {

    // MARK: - Query

    /// Returns the Space snapshot for a window (displayUUID + ordinal position).
    static func spaceSnapshot(for window: NSWindow) -> SpaceSnapshot? {
        let windowNumber = window.windowNumber
        guard windowNumber > 0 else { return nil }

        guard let spaceID = spaceIDForWindow(windowNumber) else { return nil }
        guard let info = spaceOrdinal(for: spaceID) else { return nil }

        return SpaceSnapshot(displayUUID: info.displayUUID, ordinal: info.ordinal)
    }

    /// Returns the current active Space ID.
    static func currentSpaceID() -> size_t {
        CGSGetActiveSpace(CGSMainConnectionID())
    }

    // MARK: - Restore

    /// Resolves a saved SpaceSnapshot back to a current CGSSpaceID.
    /// Falls back to matching by ordinal on any display if the saved display
    /// is not found. Clamps ordinal to the last available Space.
    static func resolveSpaceID(from snapshot: SpaceSnapshot) -> size_t? {
        let allSpaces = allUserSpacesOrdered()
        guard !allSpaces.isEmpty else { return nil }

        // Try exact display match first
        if let displayUUID = snapshot.displayUUID {
            let displaySpaces = allSpaces.filter { $0.displayUUID == displayUUID }
            if !displaySpaces.isEmpty {
                let clamped = min(snapshot.ordinal, displaySpaces.count - 1)
                return displaySpaces[clamped].spaceID
            }
        }

        // Fallback: match by ordinal on first display
        guard let firstDisplayUUID = allSpaces.first?.displayUUID else { return nil }
        let firstDisplaySpaces = allSpaces.filter { $0.displayUUID == firstDisplayUUID }
        guard !firstDisplaySpaces.isEmpty else { return nil }
        let clamped = min(snapshot.ordinal, firstDisplaySpaces.count - 1)
        return firstDisplaySpaces[clamped].spaceID
    }

    /// Pre-assigns a window to a target Space BEFORE it's ordered front.
    /// This avoids the macOS space-switch animation.
    /// The window must have a valid windowNumber (created with defer: false).
    static func assignWindowToSpace(_ window: NSWindow, spaceID: size_t) {
        let windowNumber = window.windowNumber
        guard windowNumber > 0 else { return }

        let cid = CGSMainConnectionID()
        let wids = [windowNumber as CFNumber] as CFArray
        CGSAddWindowsToSpaces(cid, wids, [spaceID as CFNumber] as CFArray)
    }

    /// Moves a window to a target Space quietly (add then remove, no animation).
    static func moveWindowToSpaceQuietly(_ window: NSWindow, targetSpaceID: size_t) {
        let windowNumber = window.windowNumber
        guard windowNumber > 0 else { return }

        let cid = CGSMainConnectionID()
        let wids = [windowNumber as CFNumber] as CFArray

        guard let currentSpaceID = spaceIDForWindow(windowNumber) else {
            // Window not on any space yet — just add
            CGSAddWindowsToSpaces(cid, wids, [targetSpaceID as CFNumber] as CFArray)
            return
        }
        guard currentSpaceID != targetSpaceID else { return }

        // Add to target FIRST (so window always has ≥1 space), then remove from source
        CGSAddWindowsToSpaces(cid, wids, [targetSpaceID as CFNumber] as CFArray)
        CGSRemoveWindowsFromSpaces(cid, wids, [currentSpaceID as CFNumber] as CFArray)
    }

    // MARK: - Internal helpers

    /// Returns the first user-type Space ID for a window.
    private static func spaceIDForWindow(_ windowNumber: Int) -> size_t? {
        let cid = CGSMainConnectionID()
        guard let unmanaged = CGSCopySpacesForWindows(
            cid,
            kCGSSpaceMaskAll,
            [windowNumber as CFNumber] as CFArray
        ) else { return nil }

        guard let spaceIDs = unmanaged.takeRetainedValue() as? [size_t] else {
            return nil
        }

        return spaceIDs.first { CGSSpaceGetType(cid, $0) == kCGSSpaceTypeUser }
    }

    /// Enumerates all user-type Spaces in Mission Control order per display.
    private static func allUserSpacesOrdered() -> [(displayUUID: String, spaceID: size_t, ordinal: Int)] {
        let cid = CGSMainConnectionID()
        guard let displaySpacesCF = CGSCopyManagedDisplaySpaces(cid) else { return [] }
        guard let displaySpaces = displaySpacesCF as? [[String: Any]] else { return [] }

        var result: [(displayUUID: String, spaceID: size_t, ordinal: Int)] = []

        for displayDict in displaySpaces {
            guard let displayUUID = displayDict["Display Identifier"] as? String else { continue }
            guard let spaces = displayDict["Spaces"] as? [[String: Any]] else { continue }

            var ordinal = 0
            for spaceDict in spaces {
                guard let spaceID64 = spaceDict["ManagedSpaceID"] as? Int64 else { continue }
                let spaceID = size_t(spaceID64)

                guard CGSSpaceGetType(cid, spaceID) == kCGSSpaceTypeUser else { continue }

                result.append((displayUUID: displayUUID, spaceID: spaceID, ordinal: ordinal))
                ordinal += 1
            }
        }

        return result
    }

    /// Converts a Space ID to its display UUID and 0-based ordinal position.
    private static func spaceOrdinal(for spaceID: size_t) -> (displayUUID: String, ordinal: Int)? {
        let allSpaces = allUserSpacesOrdered()
        guard let match = allSpaces.first(where: { $0.spaceID == spaceID }) else { return nil }
        return (displayUUID: match.displayUUID, ordinal: match.ordinal)
    }
}

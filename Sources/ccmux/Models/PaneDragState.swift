import SwiftUI

/// Tracks the state of a pane drag-and-drop operation.
/// Shared across the view hierarchy via @EnvironmentObject.
class PaneDragState: ObservableObject {
    @Published var draggedPaneId: UUID?
    @Published var currentLocation: CGPoint?
    @Published var hoveredPaneId: UUID?
    @Published var dropZone: DropZone?

    /// Cached pane frames in the "splitTree" coordinate space, updated via PreferenceKey.
    var paneFrames: [UUID: CGRect] = [:]

    var isDragging: Bool { draggedPaneId != nil }

    func beginDrag(paneId: UUID) {
        draggedPaneId = paneId
    }

    func updateLocation(_ location: CGPoint) {
        currentLocation = location
        // Hit test against cached pane frames
        for (id, frame) in paneFrames {
            if id != draggedPaneId && frame.contains(location) {
                hoveredPaneId = id
                dropZone = Self.dropZone(for: location, in: frame)
                return
            }
        }
        hoveredPaneId = nil
        dropZone = nil
    }

    func endDrag() {
        draggedPaneId = nil
        currentLocation = nil
        hoveredPaneId = nil
        dropZone = nil
    }

    /// Determine which zone a point falls in within a rect.
    /// Divides the rect into 4 triangular quadrants by comparing
    /// horizontal vs vertical distance from center.
    private static func dropZone(for point: CGPoint, in rect: CGRect) -> DropZone {
        let relX = (point.x - rect.minX) / rect.width   // 0..1
        let relY = (point.y - rect.minY) / rect.height   // 0..1
        let dx = abs(relX - 0.5)
        let dy = abs(relY - 0.5)
        if dx > dy {
            return relX < 0.5 ? .left : .right
        } else {
            return relY < 0.5 ? .top : .bottom
        }
    }
}

enum DropZone: Equatable {
    case left, right, top, bottom

    var splitDirection: SplitDirection {
        switch self {
        case .left, .right: return .horizontal
        case .top, .bottom: return .vertical
        }
    }

    var insertAsFirst: Bool {
        switch self {
        case .left, .top: return true
        case .right, .bottom: return false
        }
    }
}

/// PreferenceKey for collecting pane frames in the shared coordinate space.
struct PaneFramePreferenceKey: PreferenceKey {
    static var defaultValue: [UUID: CGRect] = [:]
    static func reduce(value: inout [UUID: CGRect], nextValue: () -> [UUID: CGRect]) {
        value.merge(nextValue()) { _, new in new }
    }
}

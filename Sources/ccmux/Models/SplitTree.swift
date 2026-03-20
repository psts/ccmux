import Foundation

enum SplitDirection: String, Codable {
    case horizontal // children are left/right
    case vertical   // children are top/bottom
}

/// A recursive binary split tree. Each node is either a split (with two children and a ratio)
/// or a leaf (containing a value of type V).
indirect enum SplitTree<V: Identifiable & Codable>: Identifiable, Codable where V.ID == UUID {
    case leaf(id: UUID, content: V)
    case split(id: UUID, direction: SplitDirection, ratio: CGFloat, first: SplitTree<V>, second: SplitTree<V>)

    var id: UUID {
        switch self {
        case .leaf(let id, _): return id
        case .split(let id, _, _, _, _): return id
        }
    }

    // MARK: - Operations (pure functions returning new trees)

    /// Replace a leaf with a split containing the original leaf and a new leaf.
    func splitLeaf(targetId: UUID, direction: SplitDirection, newContent: V) -> SplitTree {
        switch self {
        case .leaf(let id, let content):
            if id == targetId {
                return .split(
                    id: UUID(),
                    direction: direction,
                    ratio: 0.5,
                    first: .leaf(id: id, content: content),
                    second: .leaf(id: UUID(), content: newContent)
                )
            }
            return self

        case .split(let id, let dir, let ratio, let first, let second):
            return .split(
                id: id,
                direction: dir,
                ratio: ratio,
                first: first.splitLeaf(targetId: targetId, direction: direction, newContent: newContent),
                second: second.splitLeaf(targetId: targetId, direction: direction, newContent: newContent)
            )
        }
    }

    /// Remove a leaf, promoting its sibling to replace the parent split.
    /// Returns nil if the entire tree would be removed.
    func closeLeaf(targetId: UUID) -> SplitTree? {
        switch self {
        case .leaf(let id, _):
            return id == targetId ? nil : self

        case .split(let id, let dir, let ratio, let first, let second):
            // Check if either direct child is the target leaf
            if case .leaf(let fid, _) = first, fid == targetId {
                return second
            }
            if case .leaf(let sid, _) = second, sid == targetId {
                return first
            }

            // Recurse into children
            let newFirst = first.closeLeaf(targetId: targetId)
            let newSecond = second.closeLeaf(targetId: targetId)

            // If a child was removed, return the surviving sibling
            if newFirst == nil { return newSecond }
            if newSecond == nil { return newFirst }

            return .split(id: id, direction: dir, ratio: ratio, first: newFirst!, second: newSecond!)
        }
    }

    /// Update the ratio of a specific split node.
    func updateRatio(splitId: UUID, newRatio: CGFloat) -> SplitTree {
        switch self {
        case .leaf:
            return self

        case .split(let id, let dir, let ratio, let first, let second):
            if id == splitId {
                return .split(id: id, direction: dir, ratio: newRatio, first: first, second: second)
            }
            return .split(
                id: id,
                direction: dir,
                ratio: ratio,
                first: first.updateRatio(splitId: splitId, newRatio: newRatio),
                second: second.updateRatio(splitId: splitId, newRatio: newRatio)
            )
        }
    }

    /// Replace the content of a specific leaf.
    func replaceContent(leafId: UUID, newContent: V) -> SplitTree {
        switch self {
        case .leaf(let id, _):
            if id == leafId {
                return .leaf(id: id, content: newContent)
            }
            return self

        case .split(let id, let dir, let ratio, let first, let second):
            return .split(
                id: id,
                direction: dir,
                ratio: ratio,
                first: first.replaceContent(leafId: leafId, newContent: newContent),
                second: second.replaceContent(leafId: leafId, newContent: newContent)
            )
        }
    }

    /// Return all leaf values as a flat list.
    var allLeaves: [(id: UUID, content: V)] {
        switch self {
        case .leaf(let id, let content):
            return [(id: id, content: content)]
        case .split(_, _, _, let first, let second):
            return first.allLeaves + second.allLeaves
        }
    }

    /// Count of all leaf nodes.
    var leafCount: Int {
        switch self {
        case .leaf: return 1
        case .split(_, _, _, let first, let second):
            return first.leafCount + second.leafCount
        }
    }

    /// Extract a leaf by ID, returning the leaf and the remaining tree.
    /// Returns nil if the leaf doesn't exist.
    func extractLeaf(id: UUID) -> (leaf: SplitTree, remainder: SplitTree?)? {
        switch self {
        case .leaf(let leafId, _):
            if leafId == id {
                return (leaf: self, remainder: nil)
            }
            return nil

        case .split(let splitId, let dir, let ratio, let first, let second):
            // Direct child is the target
            if case .leaf(let fid, _) = first, fid == id {
                return (leaf: first, remainder: second)
            }
            if case .leaf(let sid, _) = second, sid == id {
                return (leaf: second, remainder: first)
            }

            // Recurse
            if let result = first.extractLeaf(id: id) {
                let newFirst = result.remainder
                if let newFirst {
                    return (leaf: result.leaf, remainder: .split(id: splitId, direction: dir, ratio: ratio, first: newFirst, second: second))
                } else {
                    return (leaf: result.leaf, remainder: second)
                }
            }
            if let result = second.extractLeaf(id: id) {
                let newSecond = result.remainder
                if let newSecond {
                    return (leaf: result.leaf, remainder: .split(id: splitId, direction: dir, ratio: ratio, first: first, second: newSecond))
                } else {
                    return (leaf: result.leaf, remainder: first)
                }
            }
            return nil
        }
    }

    /// Insert an existing leaf next to a target leaf.
    private func splitLeafWithExisting(targetId: UUID, direction: SplitDirection, existingLeaf: SplitTree, insertAsFirst: Bool) -> SplitTree {
        switch self {
        case .leaf(let id, let content):
            if id == targetId {
                return .split(
                    id: UUID(),
                    direction: direction,
                    ratio: 0.5,
                    first: insertAsFirst ? existingLeaf : .leaf(id: id, content: content),
                    second: insertAsFirst ? .leaf(id: id, content: content) : existingLeaf
                )
            }
            return self

        case .split(let id, let dir, let ratio, let first, let second):
            return .split(
                id: id,
                direction: dir,
                ratio: ratio,
                first: first.splitLeafWithExisting(targetId: targetId, direction: direction, existingLeaf: existingLeaf, insertAsFirst: insertAsFirst),
                second: second.splitLeafWithExisting(targetId: targetId, direction: direction, existingLeaf: existingLeaf, insertAsFirst: insertAsFirst)
            )
        }
    }

    /// Atomically move a leaf from one position to another.
    /// Returns nil if sourceId == targetId, either ID not found, or only one leaf.
    func moveLeaf(sourceId: UUID, targetId: UUID, direction: SplitDirection, insertAsFirst: Bool) -> SplitTree? {
        guard sourceId != targetId else { return nil }
        guard leafCount > 1 else { return nil }

        // Extract the source leaf
        guard let extraction = extractLeaf(id: sourceId),
              let remainder = extraction.remainder else { return nil }

        // Insert it next to the target
        return remainder.splitLeafWithExisting(
            targetId: targetId,
            direction: direction,
            existingLeaf: extraction.leaf,
            insertAsFirst: insertAsFirst
        )
    }

    /// Find a leaf by ID.
    func findLeaf(id: UUID) -> V? {
        switch self {
        case .leaf(let leafId, let content):
            return leafId == id ? content : nil
        case .split(_, _, _, let first, let second):
            return first.findLeaf(id: id) ?? second.findLeaf(id: id)
        }
    }

    // MARK: - Codable

    private enum CodingKeys: String, CodingKey {
        case type, id, content, direction, ratio, first, second
    }

    private enum NodeType: String, Codable {
        case leaf, split
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        let type = try container.decode(NodeType.self, forKey: .type)

        switch type {
        case .leaf:
            let id = try container.decode(UUID.self, forKey: .id)
            let content = try container.decode(V.self, forKey: .content)
            self = .leaf(id: id, content: content)

        case .split:
            let id = try container.decode(UUID.self, forKey: .id)
            let direction = try container.decode(SplitDirection.self, forKey: .direction)
            let ratio = try container.decode(CGFloat.self, forKey: .ratio)
            let first = try container.decode(SplitTree<V>.self, forKey: .first)
            let second = try container.decode(SplitTree<V>.self, forKey: .second)
            self = .split(id: id, direction: direction, ratio: ratio, first: first, second: second)
        }
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)

        switch self {
        case .leaf(let id, let content):
            try container.encode(NodeType.leaf, forKey: .type)
            try container.encode(id, forKey: .id)
            try container.encode(content, forKey: .content)

        case .split(let id, let direction, let ratio, let first, let second):
            try container.encode(NodeType.split, forKey: .type)
            try container.encode(id, forKey: .id)
            try container.encode(direction, forKey: .direction)
            try container.encode(ratio, forKey: .ratio)
            try container.encode(first, forKey: .first)
            try container.encode(second, forKey: .second)
        }
    }
}

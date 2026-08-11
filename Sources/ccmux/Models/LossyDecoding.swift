import Foundation

/// Element wrapper that turns an un-decodable element into `nil` instead of throwing,
/// keeping the failure so it can be logged.
///
/// `init(from:)` never throws, which is the whole point: Foundation's unkeyed
/// container does not advance `currentIndex` when an element decode throws, so a
/// naive "catch and continue" loop spins forever. Decoding `[LossyElement<T>]`
/// keeps the container's own iteration intact.
private struct LossyElement<T: Decodable>: Decodable {
    let value: T?
    let failure: Error?

    init(from decoder: Decoder) throws {
        do {
            value = try T(from: decoder)
            failure = nil
        } catch {
            value = nil
            // The DecodingError names the coding path and the reason ("layout.type"
            // vs "expected String"). A bare count leaves a vanished workspace
            // undiagnosable without the user shipping their state.json.
            failure = error
        }
    }
}

/// Tally of entries a lossy decode had to drop, carried through `decoder.userInfo`
/// so the model layer can report losses without gaining a field that would then be
/// re-encoded on the next save.
final class DecodeDropLog {
    static let key = CodingUserInfoKey(rawValue: "ccmux.decodeDropLog")!

    private(set) var fields: [(field: String, kept: Int, dropped: Int)] = []

    func record(field: String, kept: Int, dropped: Int) {
        guard dropped > 0 else { return }
        fields.append((field, kept, dropped))
    }

    var droppedCount: Int { fields.reduce(0) { $0 + $1.dropped } }
    var isEmpty: Bool { fields.isEmpty }

    /// Human-readable summary for the log line, e.g. `workspaces: 2 of 5 dropped`.
    var summary: String {
        fields.map { "\($0.field): \($0.dropped) of \($0.kept + $0.dropped) dropped" }
            .joined(separator: ", ")
    }

    /// True when some field lost every one of a non-empty set of entries. That is the
    /// signature of a wholesale schema mismatch (an older `layout` shape), not of
    /// isolated corruption — the caller retries via the legacy migration path rather
    /// than accepting a silent wipe.
    ///
    /// Any field counts, not just `workspaces`: `closedWorkspaces` holds the same
    /// layout shape, so a pre-tabs file whose open list happens to be empty is still
    /// a legacy file and its closed list is still recoverable.
    var lostAnEntireField: Bool {
        fields.contains { $0.kept == 0 }
    }
}

extension KeyedDecodingContainer {
    /// Array decode that drops un-decodable *elements* instead of failing the whole
    /// container. Drops are tallied into the `DecodeDropLog` found in
    /// `decoder.userInfo`, when one was supplied, and each one is logged with its
    /// index and underlying error.
    ///
    /// `required` controls the *key*, which is a different question from the
    /// elements: a missing `workspaces` key is not "you have no workspaces", it is a
    /// file we failed to understand, and treating it as an empty array would let the
    /// launch autosave overwrite the original with nothing.
    func decodeLossyArray<T: Decodable>(
        _: T.Type, forKey key: Key, into log: DecodeDropLog?, required: Bool = false
    ) throws -> [T] {
        let raw: [LossyElement<T>]
        if required {
            raw = try decode([LossyElement<T>].self, forKey: key)
        } else {
            guard let present = try decodeIfPresent([LossyElement<T>].self, forKey: key) else { return [] }
            raw = present
        }
        for (index, element) in raw.enumerated() where element.value == nil {
            NSLog("[ccmux] dropped %@[%d]: %@", key.stringValue, index,
                  String(describing: element.failure))
        }
        let values = raw.compactMap(\.value)
        log?.record(field: key.stringValue, kept: values.count, dropped: raw.count - values.count)
        return values
    }
}

extension Decoder {
    /// The drop log for this decode, when the caller supplied one.
    var dropLog: DecodeDropLog? { userInfo[DecodeDropLog.key] as? DecodeDropLog }
}

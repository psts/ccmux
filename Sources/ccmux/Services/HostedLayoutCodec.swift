import Foundation

/// Serializes a hosted workspace's `SplitTree` to/from the opaque JSON blob the
/// daemon versions and stores (`PUT /v1/workspaces/{id}/layout`, restored via the
/// attach hello / REST `layoutJson`). The daemon treats the blob as opaque bytes;
/// only lenses interpret it, so this is purely lens-side.
///
/// Encoding uses `.sortedKeys` so an unchanged tree always encodes to the same
/// bytes — that byte-stable output is what lets `RemoteSessionService` detect a
/// *real* layout change (vs. a re-render of the same arrangement) and avoid
/// pointless PUTs / version churn.
enum HostedLayoutCodec {
    static func encode(_ tree: SplitTree<PaneTabs>) -> String {
        let enc = JSONEncoder()
        enc.outputFormatting = [.sortedKeys]
        guard let data = try? enc.encode(tree), let s = String(data: data, encoding: .utf8) else { return "" }
        return s
    }

    static func decode(_ blob: String) -> SplitTree<PaneTabs>? {
        guard !blob.isEmpty, let data = blob.data(using: .utf8) else { return nil }
        return try? JSONDecoder().decode(SplitTree<PaneTabs>.self, from: data)
    }

    /// The daemon pane ids referenced by a tree's hosted terminals — used to check a
    /// restored blob against the workspace's live pane set before trusting it.
    static func hostedPaneIds(_ tree: SplitTree<PaneTabs>) -> Set<String> {
        var ids = Set<String>()
        for (_, tabs) in tree.allLeaves {
            for content in tabs.tabs {
                if case .terminal(let config) = content, let paneId = config.host.hostedPaneId {
                    ids.insert(paneId)
                }
            }
        }
        return ids
    }
}

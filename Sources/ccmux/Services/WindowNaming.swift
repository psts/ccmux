import Foundation

/// How a Mac window gets the name it answers to.
///
/// The name is the window's identity on the daemon: membership, open flags,
/// sizes and the peers bus group all match on it. So it is fixed once, when
/// the window is made, and changes only by an explicit rename. It used to be
/// derived from the window's place in the list ("Window \(index + 1)"), and
/// closing an earlier window renumbered the later ones: a window the daemon
/// knew as "Window 4" became "Window 3" here and shed every repo it held.
///
/// Every comparison is `WindowManager.sameWindowName` (case-insensitive), the
/// same rule the daemon's unique index and the ownership match use.
enum WindowNaming {
    /// The lowest "Window N" that no name in `taken` already is.
    static func freeName(taken: [String]) -> String {
        var n = 1
        while isTaken("Window \(n)", by: taken) { n += 1 }
        return "Window \(n)"
    }

    static func isTaken(_ name: String, by names: [String]) -> Bool {
        names.contains { WindowManager.sameWindowName($0, name) }
    }

    /// The names saved windows reopen with, index for index. A saved name is
    /// kept. A window saved without one (state from a build that named
    /// windows by position) takes the name it answered to then, "Window N"
    /// from its place in the SAVED list: that is the name its repos are
    /// grouped under on the daemon. Counted over the saved list, not the
    /// windows that reopen, because launch skips some and counting only the
    /// survivors renumbers the rest. One already used by a saved name takes
    /// a free number instead, so no two windows answer to one name. Free
    /// numbers are handed out only after every positional name is reserved:
    /// picking one early could take the name a LATER window answered to, and
    /// that window's repos would land in the wrong one.
    static func launchNames(saved: [String?]) -> [String] {
        let custom = saved.compactMap { $0 }
        var used = custom
        var names: [String?] = saved
        for (index, name) in saved.enumerated() where name == nil {
            let positional = "Window \(index + 1)"
            guard !isTaken(positional, by: custom) else { continue }
            names[index] = positional
            used.append(positional)
        }
        return names.map { name in
            if let name { return name }
            let pick = freeName(taken: used)
            used.append(pick)
            return pick
        }
    }

    /// Why a window cannot take `name`, or nil when it can. `others` are the
    /// names of the OTHER open windows: two answering to one name means only
    /// the first one gets the repos.
    static func renameRefusal(_ name: String, others: [String]) -> String? {
        let trimmed = name.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.isEmpty { return "A window needs a name." }
        if isTaken(trimmed, by: others) { return "Another open window is already called “\(trimmed)”." }
        return nil
    }

    /// The name a closed-window record reopens with, or nil when the caller
    /// must pick a free one: the record has none (saved before names were
    /// fixed), or an open window has taken it since.
    static func restoreName(saved: String?, open: [String]) -> String? {
        guard let saved, !isTaken(saved, by: open) else { return nil }
        return saved
    }
}

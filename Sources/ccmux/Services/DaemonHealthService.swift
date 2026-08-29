import Foundation
import Combine

/// Polls the daemon's own child-process census.
///
/// A child ccmuxd started and never reaped is invisible everywhere but `ps`.
/// That is how the daemon once ran for 20 hours holding 12 defunct tmux
/// clients with nothing anywhere saying so. This is the Mac lens's half of
/// that warning; the web lens shows the identical line, from the same field,
/// under the identical rule (daemon/web/app.js, fetchDaemonHealth).
class DaemonHealthService: ObservableObject {
    static let shared = DaemonHealthService()

    /// Children the daemon started and never collected. Zero when healthy —
    /// and also zero when the daemon could not inspect itself, because
    /// `known: false` means "no answer", which must never be shown as a
    /// problem the user can act on.
    @Published private(set) var defunctChildren: Int = 0

    /// The show/hide rule. It lives here so both lenses apply the same one
    /// rather than two approximations of it.
    var shouldWarn: Bool { defunctChildren > 0 }

    /// Word-for-word the web lens's string.
    var warningText: String {
        let n = defunctChildren
        return "ccmuxd is holding \(n) defunct child process\(n == 1 ? "" : "es"). "
            + "Restarting the daemon clears them."
    }

    private var timer: Timer?

    private struct Health: Decodable {
        struct Children: Decodable {
            let live: Int
            let defunct: Int
            let known: Bool
        }
        /// Optional: an older daemon does not send it, and that must read as
        /// "nothing to report", not as a decode failure that hides the rest.
        let children: Children?
    }

    private init() {}

    /// Begins polling. Idempotent, so a second call at launch is harmless.
    @MainActor
    func start(interval: TimeInterval = 30) {
        guard timer == nil else { return }
        Task { await self.refresh() }
        timer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { _ in
            Task { @MainActor in await DaemonHealthService.shared.refresh() }
        }
    }

    @MainActor
    func refresh() async {
        guard let url = URL(string: "\(DaemonConfig.localURL)/v1/health"),
              let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let health = try? JSONDecoder().decode(Health.self, from: data) else {
            return // an unreachable daemon is surfaced elsewhere; invent no number
        }
        guard let children = health.children, children.known else {
            defunctChildren = 0
            return
        }
        if defunctChildren != children.defunct { defunctChildren = children.defunct }
    }
}

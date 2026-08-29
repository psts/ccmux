import Foundation
import Combine

/// Polls the daemon's own child-process census.
///
/// A child ccmuxd started and never reaped is invisible everywhere but `ps`.
/// This is the Mac lens's half of that warning; the web lens shows the
/// identical line, from the same field, under the identical rule
/// (daemon/web/app.js, fetchDaemonHealth). The incident that prompted it is
/// recorded once, in the daemon's internal/childproc package doc.
final class DaemonHealthService: ObservableObject {
    static let shared = DaemonHealthService()

    /// Children the daemon started and never collected. Zero when healthy, and
    /// zero on every no-answer path too: `known: false`, an unreachable
    /// daemon, a non-200, or a body this app cannot decode. None of those is a
    /// problem the user can act on, and keeping the last reading through them
    /// is worse than useless — the message says "restart the daemon", so a
    /// stale count goes on blaming the user for zombies they just cleared.
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
            /// Optional although the daemon always sends it: nothing here
            /// reads it, and as a required key it would fail the whole decode
            /// and hide `defunct` — the one field this service exists for.
            let live: Int?
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
              (resp as? HTTPURLResponse)?.statusCode == 200 else {
            defunctChildren = 0 // no answer is not "still N zombies"
            return
        }
        do {
            let health = try JSONDecoder().decode(Health.self, from: data)
            guard let children = health.children, children.known else {
                defunctChildren = 0
                return
            }
            if defunctChildren != children.defunct { defunctChildren = children.defunct }
        } catch {
            // Split out from the transport failures on purpose. If the field
            // names ever move, this warning goes dark permanently, and this
            // line is the only thing that would ever say so.
            NSLog("[ccmux health] /v1/health decode failed: \(error)")
            defunctChildren = 0
        }
    }
}

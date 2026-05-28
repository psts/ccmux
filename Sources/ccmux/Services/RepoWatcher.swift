import Foundation
import CoreServices

/// Recursive directory watcher for a single git repository.
///
/// One `FSEventStream` per workspace, rooted at the repo path. Catches both
/// working-tree edits and changes inside `.git/` (commits, branch switches,
/// fetches) — i.e. everything that can shift the answer of `git status`.
///
/// The stream's own `latency` parameter coalesces bursts in the kernel, so no
/// userland debounce is needed: FSEvents already collapses 1000 `touch`es into
/// one callback.
final class RepoWatcher {
    private let resolvedPath: String
    private let onChange: () -> Void
    private let queue: DispatchQueue
    private var stream: FSEventStreamRef?

    init(repoPath: String, onChange: @escaping () -> Void) {
        // FSEvents reports paths under the resolved (real) path. Symlinked repo
        // roots otherwise produce events that don't match the registered path
        // — harmless for our "ping" use case, but tightening up here means we
        // could filter paths later without a footgun.
        var buf = [CChar](repeating: 0, count: Int(PATH_MAX))
        if let resolved = realpath(repoPath, &buf) {
            self.resolvedPath = String(cString: resolved)
        } else {
            self.resolvedPath = repoPath
        }
        self.onChange = onChange
        self.queue = DispatchQueue(label: "ccmux.repowatcher", qos: .utility)
    }

    deinit {
        stop()
    }

    func start() {
        guard stream == nil else { return }

        var context = FSEventStreamContext(
            version: 0,
            info: Unmanaged.passUnretained(self).toOpaque(),
            retain: nil,
            release: nil,
            copyDescription: nil
        )

        let callback: FSEventStreamCallback = { _, info, _, _, _, _ in
            guard let info else { return }
            let watcher = Unmanaged<RepoWatcher>.fromOpaque(info).takeUnretainedValue()
            watcher.onChange()
        }

        // NoDefer: deliver the first event immediately instead of waiting for
        // the latency window — feels more responsive for a sidebar.
        // WatchRoot: notify if the watched root itself is renamed/deleted.
        // (Omit FileEvents — we only need a "something changed" ping.)
        let flags = UInt32(kFSEventStreamCreateFlagNoDefer | kFSEventStreamCreateFlagWatchRoot)

        guard let stream = FSEventStreamCreate(
            kCFAllocatorDefault,
            callback,
            &context,
            [resolvedPath] as CFArray,
            FSEventStreamEventId(kFSEventStreamEventIdSinceNow),
            0.3,
            flags
        ) else { return }

        FSEventStreamSetDispatchQueue(stream, queue)
        FSEventStreamStart(stream)
        self.stream = stream
    }

    func stop() {
        guard let stream else { return }
        FSEventStreamStop(stream)
        FSEventStreamInvalidate(stream)
        FSEventStreamRelease(stream)
        self.stream = nil
    }
}

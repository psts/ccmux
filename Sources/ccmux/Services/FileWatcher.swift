import Foundation

/// Watches a single file path for on-disk changes via a kqueue VNODE source.
///
/// Handles atomic-rename saves (editors that write to a temp file and rename over
/// the target) by re-arming against the path when the watched fd is unlinked.
/// Coalesces bursts of events through a debounce window before notifying.
final class FileWatcher {
    private let path: String
    private let onChange: () -> Void
    private let queue: DispatchQueue
    private let debounceMs: Int

    private var fileSource: DispatchSourceFileSystemObject?
    private var debounceItem: DispatchWorkItem?
    private var stopped: Bool = false

    init(path: String, debounceMs: Int = 150, onChange: @escaping () -> Void) {
        self.path = path
        self.onChange = onChange
        self.debounceMs = debounceMs
        self.queue = DispatchQueue(label: "ccmux.filewatcher", qos: .utility)
    }

    func start() {
        queue.async { [weak self] in self?.arm() }
    }

    func stop() {
        queue.async { [weak self] in
            guard let self else { return }
            self.stopped = true
            self.debounceItem?.cancel()
            self.debounceItem = nil
            self.fileSource?.cancel()
            self.fileSource = nil
        }
    }

    deinit {
        debounceItem?.cancel()
        fileSource?.cancel()
    }

    private func arm() {
        guard !stopped else { return }
        fileSource?.cancel()
        fileSource = nil

        let fd = open(path, O_EVTONLY)
        guard fd >= 0 else { return }

        let source = DispatchSource.makeFileSystemObjectSource(
            fileDescriptor: fd,
            eventMask: [.write, .extend, .rename, .delete, .attrib, .link, .revoke],
            queue: queue
        )
        source.setEventHandler { [weak self, weak source] in
            guard let self, let source else { return }
            let flags = source.data
            // Atomic-rename / unlink: the watched inode is now orphaned. Re-arm
            // against the path after a tiny delay so the new file is in place.
            let revoked = flags.contains(.delete) || flags.contains(.rename) || flags.contains(.revoke)
            if revoked {
                self.queue.asyncAfter(deadline: .now() + .milliseconds(80)) { [weak self] in
                    self?.arm()
                    self?.scheduleNotify()
                }
                return
            }
            self.scheduleNotify()
        }
        source.setCancelHandler {
            close(fd)
        }
        fileSource = source
        source.resume()
    }

    private func scheduleNotify() {
        debounceItem?.cancel()
        let item = DispatchWorkItem { [weak self] in self?.onChange() }
        debounceItem = item
        queue.asyncAfter(deadline: .now() + .milliseconds(debounceMs), execute: item)
    }
}

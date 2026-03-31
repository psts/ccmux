import Foundation

class Debouncer {
    private let delay: TimeInterval
    private var workItem: DispatchWorkItem?
    private let queue: DispatchQueue
    private let lock = NSLock()

    init(delay: TimeInterval, queue: DispatchQueue = .main) {
        self.delay = delay
        self.queue = queue
    }

    func call(action: @escaping () -> Void) {
        lock.lock()
        workItem?.cancel()
        let item = DispatchWorkItem(block: action)
        workItem = item
        lock.unlock()
        queue.asyncAfter(deadline: .now() + delay, execute: item)
    }

    func cancel() {
        lock.lock()
        workItem?.cancel()
        workItem = nil
        lock.unlock()
    }
}

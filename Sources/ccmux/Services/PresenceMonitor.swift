import AppKit

/// Tracks whether the user is demonstrably AT this Mac: screen unlocked, no
/// screensaver, displays awake. Drives the hosted focus frames that suppress
/// phone pushes — "present" means macOS notifications are seeable here, so the
/// phone should stay quiet. App frontmost-ness is deliberately NOT a factor:
/// ccmux in the background still notifies locally.
///
/// The three away-signals are tracked independently and recombined, because
/// macOS delivers them in surprising orders (a screensaver stop while the lock
/// prompt is still up must not count as "back").
final class PresenceMonitor {
    private(set) var isPresent = true
    /// Fired on the main thread when presence flips.
    var onChange: ((Bool) -> Void)?

    private var locked = false
    private var saverRunning = false
    private var displaysAsleep = false

    init() {
        let dist = DistributedNotificationCenter.default()
        dist.addObserver(self, selector: #selector(screenLocked), name: .init("com.apple.screenIsLocked"), object: nil)
        dist.addObserver(self, selector: #selector(screenUnlocked), name: .init("com.apple.screenIsUnlocked"), object: nil)
        dist.addObserver(self, selector: #selector(saverStarted), name: .init("com.apple.screensaver.didstart"), object: nil)
        dist.addObserver(self, selector: #selector(saverStopped), name: .init("com.apple.screensaver.didstop"), object: nil)
        let ws = NSWorkspace.shared.notificationCenter
        ws.addObserver(self, selector: #selector(screensSlept), name: NSWorkspace.screensDidSleepNotification, object: nil)
        ws.addObserver(self, selector: #selector(screensWoke), name: NSWorkspace.screensDidWakeNotification, object: nil)
    }

    @objc private func screenLocked() { locked = true; recompute() }
    @objc private func screenUnlocked() { locked = false; recompute() }
    @objc private func saverStarted() { saverRunning = true; recompute() }
    @objc private func saverStopped() { saverRunning = false; recompute() }
    @objc private func screensSlept() { displaysAsleep = true; recompute() }
    @objc private func screensWoke() { displaysAsleep = false; recompute() }

    private func recompute() {
        let present = !locked && !saverRunning && !displaysAsleep
        guard present != isPresent else { return }
        isPresent = present
        DispatchQueue.main.async { [weak self] in self?.onChange?(present) }
    }
}

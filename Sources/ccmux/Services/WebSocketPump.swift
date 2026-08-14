import Foundation

/// One auto-reconnecting WebSocket, plus the liveness check `URLSessionWebSocketTask`
/// does not give you.
///
/// The lens sockets (`/v1/events`, `/v1/attach`) both used to reconnect ONLY when
/// `receive` returned an error. That covers a socket the peer closes. It does not
/// cover a socket that simply stops existing — a Mac that slept, a re-keyed
/// tailnet path, a DERP relay swap. There the connection is half-open: no FIN, no
/// RST, so `receive` never calls back at all and the reconnect never runs. The
/// symptom was hosted attention flashes dying after a sleep and never returning
/// until the app was relaunched.
///
/// The fix is to make silence measurable. The daemon now pings (see the Go side's
/// keepalive.go), and this pump pings back on its own schedule: a pong is the only
/// positive proof the path is still there. Waiting for DATA would be wrong, since
/// a quiet firehose can legitimately send nothing for hours.
///
/// Both clients share this because they had the same three defects — a state race
/// across two queues, a reconnect that could open a second socket, and a replaced
/// task that was never cancelled. Their differences (bidirectional vs receive-only)
/// live in the clients; the connection lifecycle lives here, once.
final class WebSocketPump {
    /// Called on the pump's private queue for every text frame.
    var onText: ((String) -> Void)?
    /// Called on the main thread whenever the connection state changes.
    var onState: ((DaemonConnectionState) -> Void)?

    private let session: URLSession
    private let makeURL: () -> URL?
    private let label: String

    /// Everything below is confined to this queue. It previously lived across two
    /// (URLSession's delegate queue delivered receive callbacks while `connect` and
    /// `disconnect` ran on main), which was an unguarded data race on every field.
    private let queue: DispatchQueue

    private var task: URLSessionWebSocketTask?
    private var closed = true
    private var attempts = 0
    /// Bumped on every open. Callbacks carry the generation they belong to, so a
    /// delayed reconnect that fires after a `disconnect()`/`connect()` cycle cannot
    /// open a second socket behind the first — the bug that made a wake handler
    /// leave two live connections and one orphaned presence entry.
    private var generation = 0
    private var timer: DispatchSourceTimer?
    /// When the outstanding ping was sent, or nil when none is in flight.
    private var pingSentAt: Date?
    /// When the last ping was sent, answered or not. Separate from `pingSentAt`
    /// so the interval survives a pong arriving between ticks.
    private var lastPingAt = Date()
    private var state: DaemonConnectionState = .closed

    /// How often to ping, and how long a pong may take before we call it dead. The
    /// daemon pings every 30s and reaps at 90s; staying inside that keeps a genuine
    /// outage visible within about a minute.
    private let pingEvery: TimeInterval
    private let pongWithin: TimeInterval

    init(
        label: String,
        pingEvery: TimeInterval = 25,
        pongWithin: TimeInterval = 20,
        makeURL: @escaping () -> URL?
    ) {
        self.label = label
        self.pingEvery = pingEvery
        self.pongWithin = pongWithin
        self.makeURL = makeURL
        self.session = URLSession(configuration: .default)
        self.queue = DispatchQueue(label: "ccmux.wspump.\(label)")
    }

    /// A resumed DispatchSourceTimer is retained by GCD, so a pump released
    /// without disconnect() would keep its timer (and its socket) alive forever.
    /// Every current owner does call disconnect(); this makes that a courtesy
    /// rather than a requirement.
    deinit {
        timer?.cancel()
        task?.cancel(with: .goingAway, reason: nil)
    }

    // MARK: - Lifecycle

    func connect() {
        queue.async { [self] in
            closed = false
            // Announce the attempt before making it. The pump starts at .closed,
            // so without this a connect() that fails immediately (a URL that
            // cannot be built) would transition .closed -> .closed, which `set`
            // filters out as a no-op — leaving the owner's UI on whatever it
            // assumed and no callback ever explaining why.
            set(.connecting)
            open()
        }
    }

    func disconnect() {
        queue.async { [self] in
            closed = true
            generation &+= 1 // orphan every in-flight callback and delayed reconnect
            stopTimer()
            cancelTask()
            set(.closed)
        }
    }

    /// Tear the socket down and immediately dial again, whatever the pump currently
    /// believes about its health. This is what a wake handler wants: after a sleep
    /// the old connection is usually a corpse that will never report itself.
    func forceReconnect() {
        queue.async { [self] in
            guard !closed else { return }
            attempts = 0 // a deliberate bounce should retry immediately, not back off
            open()
        }
    }

    /// Send a frame, treating a send failure as evidence the path is dead.
    ///
    /// Both halves matter. Keystrokes travel this way, and a swallowed error left
    /// a terminal looking live while it ate input until the next ping noticed, up
    /// to a ping interval later. Focus and presence travel this way too, and a
    /// dropped `present: false` at screen-lock leaves the daemon believing this
    /// screen is still occupied — which suppresses that person's phone pushes
    /// across the whole federation until the socket is rebuilt.
    func send(_ text: String) {
        queue.async { [self] in
            guard let task else {
                NSLog("[ccmux ws] %@: dropped a frame, no socket yet (reconnecting)", label)
                return
            }
            let gen = generation
            task.send(.string(text)) { [weak self] error in
                guard let self, let error else { return }
                self.queue.async {
                    guard !self.closed, gen == self.generation else { return }
                    NSLog("[ccmux ws] %@: send failed (%@), reconnecting", self.label, "\(error)")
                    self.scheduleReconnect()
                }
            }
        }
    }

    // MARK: - Socket

    private func open() {
        guard !closed else { return }
        guard let url = makeURL() else {
            // A nil URL is a misconfigured origin, not a transient fault, so there
            // is nothing to retry. Say so and land on .closed: returning quietly
            // left the pump dead forever while the UI, which starts at .connecting,
            // showed a spinner that could never resolve and logged nothing.
            NSLog("[ccmux ws] %@: cannot build a URL (daemon origin misconfigured); not connecting", label)
            set(.closed)
            return
        }
        generation &+= 1
        let gen = generation

        cancelTask() // never leave the previous socket open: the daemon would hold
                     // its presence entry, which keeps suppressing phone pushes
        set(attempts == 0 ? .connecting : .reconnecting)

        let task = session.webSocketTask(with: url)
        self.task = task
        task.resume()
        startTimer()
        receive(on: task, generation: gen, firstFrame: true)
    }

    private func receive(on task: URLSessionWebSocketTask, generation gen: Int, firstFrame: Bool) {
        task.receive { [weak self] result in
            guard let self else { return }
            self.queue.async {
                guard !self.closed, gen == self.generation else { return }
                switch result {
                case .success(let message):
                    if firstFrame {
                        self.attempts = 0
                        self.set(.connected)
                    }
                    self.deliver(message)
                    self.receive(on: task, generation: gen, firstFrame: false)
                case .failure(let error):
                    // Logged at the first attempt only (scheduleReconnect has not
                    // incremented yet, so attempts is still 0 for a fresh break).
                    // A flaky link must not fill the log, but a workspace that no
                    // longer exists, a bad cert, or a refused upgrade would
                    // otherwise reconnect forever in complete silence.
                    if self.attempts == 0 {
                        NSLog("[ccmux ws] %@: receive failed (%@), reconnecting", self.label, "\(error)")
                    }
                    self.scheduleReconnect()
                }
            }
        }
    }

    private func deliver(_ message: URLSessionWebSocketTask.Message) {
        switch message {
        case .string(let s): onText?(s)
        case .data(let d): if let s = String(data: d, encoding: .utf8) { onText?(s) }
        @unknown default: break
        }
    }

    private func cancelTask() {
        task?.cancel(with: .goingAway, reason: nil)
        task = nil
    }

    private func scheduleReconnect() {
        guard !closed else { return }
        stopTimer()
        cancelTask()
        set(.reconnecting)
        attempts += 1
        // Bumping the generation here, not only in open(), is what makes this
        // idempotent for ONE failure. Cancelling the task above makes the pending
        // receive fail, which calls straight back in here; without the bump that
        // second call counted as another attempt and the ladder skipped a rung
        // (1s, 4s, 5s instead of 0.5s, 1s, 2s).
        generation &+= 1
        let gen = generation
        let delay = Self.backoff(attempt: attempts)
        queue.asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, !self.closed, gen == self.generation else { return }
            self.open()
        }
    }

    /// Reconnect delay for the nth consecutive failure: 0.5s, 1s, 2s, 4s, capped
    /// at 5s. Capped because a lens that reconnects a few seconds late is fine,
    /// while one that has backed off to minutes looks broken to the user.
    static func backoff(attempt: Int) -> TimeInterval {
        guard attempt > 0 else { return 0 }
        return min(0.5 * pow(2.0, Double(attempt - 1)), 5.0)
    }

    // MARK: - Liveness

    /// One timer drives both halves: send a ping when one is due, and give up when
    /// an outstanding ping has gone unanswered for too long.
    private func startTimer() {
        stopTimer()
        lastPingAt = Date()
        let t = DispatchSource.makeTimerSource(queue: queue)
        // A DispatchSourceTimer, not a Timer: this queue has no run loop, so a
        // scheduled Timer created here would simply never fire.
        //
        // The tick is deliberately finer than the ping interval so an unanswered
        // ping's deadline gets checked BETWEEN pings. checkLiveness keeps its own
        // clock, so a finer tick does not mean more pings.
        t.schedule(deadline: .now() + pingEvery, repeating: pingEvery / 2)
        t.setEventHandler { [weak self] in self?.checkLiveness() }
        t.resume()
        timer = t
    }

    private func stopTimer() {
        timer?.cancel()
        timer = nil
        pingSentAt = nil
    }

    private func checkLiveness() {
        guard !closed, let task else { return }
        let now = Date()
        if let sent = pingSentAt {
            if now.timeIntervalSince(sent) > pongWithin {
                NSLog("[ccmux ws] %@: no pong in %.0fs, reconnecting", label, pongWithin)
                scheduleReconnect()
            }
            return // one ping in flight at a time
        }
        // The timer ticks faster than the ping interval (see startTimer), so the
        // interval is enforced here rather than by the tick rate.
        guard now.timeIntervalSince(lastPingAt) >= pingEvery else { return }
        lastPingAt = now
        pingSentAt = now
        let gen = generation
        task.sendPing { [weak self] error in
            guard let self else { return }
            self.queue.async {
                guard !self.closed, gen == self.generation else { return }
                if let error {
                    // A failed ping is a dead path, reported without waiting out
                    // the pong deadline.
                    NSLog("[ccmux ws] %@: ping failed (%@), reconnecting", self.label, "\(error)")
                    self.scheduleReconnect()
                } else {
                    self.pingSentAt = nil
                }
            }
        }
    }

    private func set(_ next: DaemonConnectionState) {
        guard next != state else { return }
        state = next
        DispatchQueue.main.async { [weak self] in self?.onState?(next) }
    }
}

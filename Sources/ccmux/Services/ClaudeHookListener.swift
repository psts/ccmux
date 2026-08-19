import Foundation
import AppKit

/// Receives Claude Code hook events forwarded by `hooks/ccmux-notify.sh` over a
/// Unix domain socket and turns them into per-workspace attention signals
/// (sidebar flash + macOS notification).
///
/// Independent of claude-deck: ccmux owns its own socket and its own hook entry in
/// `~/.claude/settings.json`. The hook sends one compact JSON message per event
/// (`{type, cwd, notification_type}`) then closes, mirroring claude-deck's pattern.
final class ClaudeHookListener {
    static let socketPath = "/tmp/ccmux-hooks.sock"

    /// What an incoming event should do to a workspace's attention state.
    enum EventOutcome: Equatable {
        case set(AttentionState)
        case clear
        case ignore
    }

    private weak var workspaceManager: WorkspaceManager?
    private weak var windowManager: WindowManager?
    private let notifier: AttentionNotifier

    private var listenFD: Int32 = -1
    private var acceptSource: DispatchSourceRead?
    private let queue = DispatchQueue(label: "com.ccmux.hooklistener")

    init(workspaceManager: WorkspaceManager, windowManager: WindowManager, notifier: AttentionNotifier) {
        self.workspaceManager = workspaceManager
        self.windowManager = windowManager
        self.notifier = notifier
    }

    // MARK: - Lifecycle

    func start() {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { NSLog("[ccmux hooks] socket() failed: \(errno)"); return }

        // Non-blocking so the accept loop can drain all pending connections then stop.
        _ = fcntl(fd, F_SETFL, fcntl(fd, F_GETFL, 0) | O_NONBLOCK)

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
        unlink(Self.socketPath)
        // Capacity captured before the exclusive `&addr.sun_path` borrow (computing it
        // inside the closure would be an overlapping access).
        let pathCapacity = MemoryLayout.size(ofValue: addr.sun_path)
        Self.socketPath.withCString { cstr in
            withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
                ptr.withMemoryRebound(to: CChar.self, capacity: pathCapacity) { dst in
                    _ = strncpy(dst, cstr, pathCapacity - 1)
                }
            }
        }

        let bindOK = withUnsafePointer(to: &addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                bind(fd, sa, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bindOK == 0 else { NSLog("[ccmux hooks] bind() failed: \(errno)"); close(fd); return }

        // World-writable so the hook (running as any shell) can connect.
        chmod(Self.socketPath, 0o777)

        guard listen(fd, 16) == 0 else { NSLog("[ccmux hooks] listen() failed: \(errno)"); close(fd); return }

        listenFD = fd
        let src = DispatchSource.makeReadSource(fileDescriptor: fd, queue: queue)
        src.setEventHandler { [weak self] in self?.drainAccepts() }
        src.resume()
        acceptSource = src
        NSLog("[ccmux hooks] listening on \(Self.socketPath)")
    }

    func stop() {
        acceptSource?.cancel()
        acceptSource = nil
        if listenFD >= 0 { close(listenFD); listenFD = -1 }
        unlink(Self.socketPath)
    }

    // MARK: - Accept / read

    private func drainAccepts() {
        while true {
            let clientFD = accept(listenFD, nil, nil)
            if clientFD < 0 { break }  // EAGAIN/EWOULDBLOCK — nothing more pending
            // Read each client off the listener queue so a slow `nc` can't stall accepts.
            DispatchQueue.global(qos: .utility).async { [weak self] in
                self?.readClient(clientFD)
            }
        }
    }

    private func readClient(_ clientFD: Int32) {
        defer { close(clientFD) }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let n = read(clientFD, &buffer, buffer.count)
            if n > 0 { data.append(buffer, count: n) } else { break }
        }
        guard !data.isEmpty else { return }
        DispatchQueue.main.async { [weak self] in self?.handle(data) }
    }

    // MARK: - Event handling (main thread)

    private func handle(_ data: Data) {
        guard let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let type = obj["type"] as? String
        else { return }

        // Trace context shared by every branch below. `trace_id` comes from
        // ccmux-notify.sh, so a local decision lines up with the hook that caused
        // it in the shared log.
        var ctx: [String: String] = ["event": type]
        ctx["trace_id"] = obj["trace_id"] as? String
        ctx["cwd"] = obj["cwd"] as? String
        ctx["session_id"] = obj["session_id"] as? String

        guard let cwd = obj["cwd"] as? String, !cwd.isEmpty,
              let wm = workspaceManager,
              let workspace = wm.workspace(forCwd: cwd),
              let monitor = wm.attentionMonitors[workspace.id]
        else {
            HookTrace.write(decision: "unresolved", fields: ctx.merging(
                ["detail": "no local workspace matches this cwd"]) { _, new in new })
            return
        }
        ctx["workspace_id"] = workspace.id.uuidString

        switch Self.outcome(forEvent: type, notificationType: obj["notification_type"] as? String) {
        case .clear:
            monitor.clear()
            HookTrace.write(decision: "cleared", fields: ctx)
        case .ignore:
            HookTrace.write(decision: "ignored", fields: ctx.merging(
                ["detail": obj["notification_type"] as? String ?? "no attention meaning"]) { _, new in new })
        case .set(let newState):
            ctx["attention"] = String(describing: newState)
            // Suppress when you're already watching this workspace — nothing to flag.
            if windowManager?.isWatching(workspace.id) ?? false {
                monitor.clear()
                HookTrace.write(decision: "suppressed", fields: ctx.merging(
                    ["detail": "key window, on your current Space"]) { _, new in new })
                return
            }
            monitor.set(newState)

            // The sidebar flashes for every state; only some raise an alert. The
            // rule lives on AttentionNotifier so this path and the hosted firehose
            // path cannot drift apart — they already did once.
            guard AttentionNotifier.alerts(newState) else {
                HookTrace.write(decision: "flashed", fields: ctx.merging(
                    ["detail": "done flashes the lens; the alert waits for idle_prompt"]) { _, new in new })
                return
            }
            notifier.post(for: workspace, state: newState)
            HookTrace.write(decision: "posted", fields: ctx)
        }
    }

    // MARK: - Pure event mapping (tested)

    /// Map a hook event (+ optional notification subtype) to a state outcome.
    static func outcome(forEvent type: String, notificationType: String?) -> EventOutcome {
        switch type {
        case "notification":
            switch notificationType {
            case "idle_prompt", "permission_prompt", "elicitation_dialog":
                return .set(.needsInput)
            default:
                return .ignore  // auth_success and other non-blocking notifications
            }
        case "permission_request", "ask_user_question":
            return .set(.needsInput)
        case "stop":
            return .set(.done)
        case "user_prompt_submit", "session_end":
            return .clear
        default:
            return .ignore
        }
    }
}

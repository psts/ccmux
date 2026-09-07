import Foundation

/// One agent pane's conversation as the chat view shows it, fed by the
/// daemon's /v1/panes/{id}/agent/ws (see daemon/internal/api/agentchat.go).
/// Same rules as the web lens's agentchat.js: hello resets everything, a
/// turn or part upserts by id, a delta appends to one part's field, idle
/// clears busy, and a prompt while asleep wakes the agent with the text.
@MainActor
final class AgentChatState: ObservableObject {
    @Published private(set) var turns: [AgentTurn] = []
    @Published private(set) var permissions: [AgentPermission] = []
    @Published private(set) var state = "connecting"
    @Published private(set) var title = ""
    @Published private(set) var error = ""
    @Published private(set) var busy = false
    @Published private(set) var connection: DaemonConnectionState = .closed

    let agent: String
    private var pump: WebSocketPump?

    init(agent: String) {
        self.agent = agent
    }

    /// Dials the chat socket; the pump reconnects on its own.
    func start(paneId: String, wsOrigin: String) {
        guard pump == nil else { return }
        let url = URL(string: "\(wsOrigin)/v1/panes/\(paneId)/agent/ws")
        let p = WebSocketPump(label: "agent-chat-\(paneId)") { url }
        p.onText = { [weak self] text in
            guard let data = text.data(using: .utf8),
                  let frame = try? JSONDecoder().decode(AgentChatFrame.self, from: data) else { return }
            DispatchQueue.main.async { self?.apply(frame) }
        }
        p.onState = { [weak self] s in
            DispatchQueue.main.async {
                self?.connection = s
                if s != .connected { self?.state = "reconnecting" }
            }
        }
        pump = p
        p.connect()
    }

    func stop() {
        pump?.disconnect()
        pump = nil
    }

    func send(prompt text: String) {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        busy = true
        send(AgentChatFrame(t: "prompt", text: trimmed))
    }

    func abort() { send(AgentChatFrame(t: "abort")) }

    func reply(_ permission: AgentPermission, _ reply: String) {
        send(AgentChatFrame(t: "permission", id: permission.id, reply: reply))
    }

    private func send(_ frame: AgentChatFrame) {
        guard let data = try? JSONEncoder().encode(frame), let text = String(data: data, encoding: .utf8) else { return }
        pump?.send(text)
    }

    // MARK: - Frames

    func apply(_ f: AgentChatFrame) {
        switch f.t {
        case "hello": hello(f)
        case "state": state = f.state ?? state
        case "turn": if let t = f.turn { upsert(t) }
        case "part": if let mid = f.messageId, let p = f.part { upsert(part: p, in: mid) }
        case "delta": delta(f)
        case "idle": busy = false
        case "permission": if let p = f.permission, !permissions.contains(where: { $0.id == p.id }) { permissions.append(p) }
        case "permission-replied": permissions.removeAll { $0.id == f.id }
        case "error": error = f.error ?? ""
        default: break
        }
    }

    private func hello(_ f: AgentChatFrame) {
        turns = []
        permissions = f.permissions ?? []
        state = f.state ?? "asleep"
        title = f.title ?? ""
        error = f.error ?? ""
        busy = false
        for t in f.turns ?? [] { upsert(t) }
    }

    private func upsert(_ t: AgentTurn) {
        if let i = turns.firstIndex(where: { $0.id == t.id }) {
            var cur = turns[i]
            cur.role = t.role
            cur.time = t.time
            cur.error = t.error
            turns[i] = cur
            for p in t.parts { upsert(part: p, in: t.id) }
        } else {
            turns.append(t)
        }
        if t.role == "assistant", t.error == nil { busy = true }
    }

    private func upsert(part: AgentTurnPart, in messageId: String) {
        if turns.firstIndex(where: { $0.id == messageId }) == nil {
            turns.append(AgentTurn(id: messageId, role: "assistant"))
        }
        let i = turns.firstIndex { $0.id == messageId }!
        if let j = turns[i].parts.firstIndex(where: { $0.id == part.id }) {
            turns[i].parts[j] = part
        } else {
            turns[i].parts.append(part)
        }
    }

    private func delta(_ f: AgentChatFrame) {
        guard let mid = f.messageId, let pid = f.partId, let d = f.delta else { return }
        guard let i = turns.firstIndex(where: { $0.id == mid }),
              let j = turns[i].parts.firstIndex(where: { $0.id == pid }) else {
            let type = f.field == "text" ? "text" : "reasoning"
            upsert(part: AgentTurnPart(id: pid, type: type, text: d), in: mid)
            return
        }
        switch f.field {
        case "text": turns[i].parts[j].text = (turns[i].parts[j].text ?? "") + d
        case "output": turns[i].parts[j].output = (turns[i].parts[j].output ?? "") + d
        default: break
        }
    }
}

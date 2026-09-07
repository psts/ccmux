import SwiftUI

/// An agent pane: its conversation as a chat, with the raw terminal one
/// click away. Same two modes as the web lens's agentchat.js, per pane.
struct AgentPaneView: View {
    let tabId: UUID
    let paneId: String
    let workingDirectory: String
    let agent: String
    let wsOrigin: String
    @State private var showTerminal = false

    var body: some View {
        if showTerminal {
            VStack(spacing: 0) {
                HStack(spacing: 8) {
                    Text("⚙ \(agent) · terminal").font(.system(size: 11)).foregroundColor(.secondary)
                    Spacer()
                    Button("Chat") { showTerminal = false }.font(.system(size: 11))
                }
                .padding(.horizontal, 10).padding(.vertical, 5)
                Divider()
                HostedTerminalPaneView(tabId: tabId, paneId: paneId, workingDirectory: workingDirectory)
            }
        } else {
            AgentChatPaneView(paneId: paneId, agent: agent, wsOrigin: wsOrigin) { showTerminal = true }
        }
    }
}

struct AgentChatPaneView: View {
    let paneId: String
    let agent: String
    let wsOrigin: String
    let onTerminal: () -> Void
    @StateObject private var chat: AgentChatState
    @State private var draft = ""

    init(paneId: String, agent: String, wsOrigin: String, onTerminal: @escaping () -> Void) {
        self.paneId = paneId
        self.agent = agent
        self.wsOrigin = wsOrigin
        self.onTerminal = onTerminal
        _chat = StateObject(wrappedValue: AgentChatState(agent: agent))
    }

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            if !chat.error.isEmpty {
                Text(chat.error).font(.system(size: 11)).foregroundColor(.red)
                    .padding(.horizontal, 10).padding(.vertical, 4)
                    .frame(maxWidth: .infinity, alignment: .leading)
                Divider()
            }
            transcript
            if !chat.permissions.isEmpty { permissionCards }
            ForEach(chat.questions) { q in
                AgentQuestionCard(question: q, agent: agent,
                                  onAnswer: { chat.answer(q, $0) }, onReject: { chat.reject(q) })
                    .padding(.horizontal, 12).padding(.bottom, 8)
            }
            Divider()
            composer
        }
        .onAppear { chat.start(paneId: paneId, wsOrigin: wsOrigin) }
        .onDisappear { chat.stop() }
    }

    private var header: some View {
        HStack(spacing: 10) {
            Text("⚙ \(agent)").font(.system(size: 12, weight: .semibold))
            Text(chat.state).font(.system(size: 11)).foregroundColor(stateColor)
            Text(chat.title).font(.system(size: 11)).foregroundColor(.secondary).lineLimit(1)
            Spacer()
            if chat.busy && chat.state == "running" {
                Button("Stop") { chat.abort() }.font(.system(size: 11))
            }
            Button("Terminal", action: onTerminal).font(.system(size: 11))
        }
        .padding(.horizontal, 10).padding(.vertical, 5)
    }

    private var stateColor: Color {
        switch chat.state {
        case "running": return .green
        case "starting", "connecting", "reconnecting": return .orange
        default: return .secondary
        }
    }

    private var transcript: some View {
        ScrollViewReader { proxy in
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    ForEach(chat.turns) { turn in
                        AgentTurnRow(turn: turn, agent: agent).id(turn.id)
                    }
                    if chat.busy {
                        Text("…").foregroundColor(.secondary).id("busy")
                    }
                }
                .padding(12)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .onChange(of: chat.turns.count) {
                if let last = chat.turns.last?.id { proxy.scrollTo(last, anchor: .bottom) }
            }
        }
    }

    private var permissionCards: some View {
        VStack(spacing: 8) {
            ForEach(chat.permissions) { p in
                VStack(alignment: .leading, spacing: 6) {
                    Text("\(agent) asks to run \(p.summary)").font(.system(size: 11))
                    HStack(spacing: 6) {
                        Button("Allow once") { chat.reply(p, "once") }
                        Button("Allow always") { chat.reply(p, "always") }
                        Button("Reject") { chat.reply(p, "reject") }
                    }
                    .font(.system(size: 11))
                }
                .padding(8)
                .frame(maxWidth: .infinity, alignment: .leading)
                .overlay(RoundedRectangle(cornerRadius: 6).stroke(Color.orange, lineWidth: 1))
            }
        }
        .padding(.horizontal, 12).padding(.bottom, 8)
    }

    private var composer: some View {
        HStack(alignment: .bottom, spacing: 8) {
            TextField(placeholder, text: $draft, axis: .vertical)
                .lineLimit(1...6)
                .textFieldStyle(.roundedBorder)
                .onSubmit(submit)
            Button("Send", action: submit).disabled(draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
        }
        .padding(10)
    }

    private var placeholder: String {
        chat.state == "asleep" ? "Message \(agent)… Enter starts it" : "Message \(agent)… Enter sends, Option-Enter for a new line"
    }

    private func submit() {
        chat.send(prompt: draft)
        draft = ""
    }
}

/// One turn: who, then its parts. Reasoning and tool calls fold away.
struct AgentTurnRow: View {
    let turn: AgentTurn
    let agent: String

    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(turn.role == "user" ? "you" : agent)
                .font(.system(size: 10))
                .foregroundColor(turn.role == "user" ? .green : .secondary)
            ForEach(turn.parts) { part in
                AgentPartView(part: part)
            }
            if let err = turn.error {
                Text(err).font(.system(size: 11)).foregroundColor(.red)
            }
        }
        .padding(.horizontal, 10).padding(.vertical, 6)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(turn.role == "user" ? Color.white.opacity(0.05) : Color.clear)
        .cornerRadius(6)
    }
}

struct AgentPartView: View {
    let part: AgentTurnPart

    var body: some View {
        switch part.type {
        case "reasoning" where (part.text ?? "").isEmpty:
            // The provider returned only a signature: nothing to show yet.
            EmptyView()
        case "reasoning":
            DisclosureGroup("Thought") {
                Text(part.text ?? "").font(.system(size: 11)).foregroundColor(.secondary).textSelection(.enabled)
            }
            .font(.system(size: 11)).foregroundColor(.secondary)
        case "tool":
            DisclosureGroup("\(mark) \(part.tool ?? "tool")\(part.title.map { " · \($0)" } ?? "")") {
                VStack(alignment: .leading, spacing: 4) {
                    if !part.inputText.isEmpty {
                        Text(part.inputText).font(.system(size: 10, design: .monospaced)).textSelection(.enabled)
                    }
                    if let out = part.error ?? part.output, !out.isEmpty {
                        Text(out).font(.system(size: 10, design: .monospaced))
                            .foregroundColor(part.error == nil ? .primary : .red)
                            .textSelection(.enabled)
                    }
                }
                .padding(.leading, 8)
            }
            .font(.system(size: 11)).foregroundColor(toolColor)
        default:
            Text(part.text ?? "").font(.system(size: 12)).textSelection(.enabled)
        }
    }

    private var mark: String {
        switch part.status {
        case "running": return "◐"
        case "completed": return "●"
        case "error": return "✖"
        default: return "○"
        }
    }

    private var toolColor: Color {
        switch part.status {
        case "error": return .red
        case "running": return .orange
        default: return .secondary
        }
    }
}

/// One question request: each question's options stacked, label and
/// description, full width. A single-choice question answers on the tap
/// itself; several choices or a free answer keep an Answer button.
struct AgentQuestionCard: View {
    let question: AgentQuestion
    let agent: String
    let onAnswer: ([[String]]) -> Void
    let onReject: () -> Void
    @State private var chosen: [Set<String>] = []
    @State private var custom: [String] = []

    private var direct: Bool {
        question.questions.count == 1 && !(question.questions[0].multiple ?? false) && !(question.questions[0].custom ?? false)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(question.questions.enumerated()), id: \.offset) { i, q in
                VStack(alignment: .leading, spacing: 6) {
                    Text((q.header.map { "\($0): " } ?? "") + q.question).font(.system(size: 12, weight: .semibold))
                    ForEach(q.options) { o in
                        optionRow(i, o, multiple: q.multiple ?? false)
                    }
                    if q.custom ?? false {
                        TextField("Or type your own answer", text: binding(i)).font(.system(size: 12))
                    }
                }
            }
            HStack(spacing: 6) {
                if !direct { Button("Answer") { onAnswer(answers()) } }
                Button("Reject", action: onReject)
            }
            .font(.system(size: 11))
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .overlay(RoundedRectangle(cornerRadius: 6).stroke(Color.orange, lineWidth: 1))
        .onAppear {
            chosen = Array(repeating: [], count: question.questions.count)
            custom = Array(repeating: "", count: question.questions.count)
        }
    }

    private func optionRow(_ i: Int, _ o: AgentQuestionOption, multiple: Bool) -> some View {
        let picked = chosen.indices.contains(i) && chosen[i].contains(o.label)
        return Button {
            if direct { onAnswer([[o.label]]) } else { toggle(i, o.label, multiple: multiple) }
        } label: {
            VStack(alignment: .leading, spacing: 2) {
                Text(o.label).font(.system(size: 12, weight: .semibold))
                if let d = o.description, !d.isEmpty {
                    Text(d).font(.system(size: 11)).foregroundColor(.secondary)
                }
            }
            .padding(8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .overlay(RoundedRectangle(cornerRadius: 6).stroke(picked ? Color.green : Color.secondary.opacity(0.4), lineWidth: 1))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private func toggle(_ i: Int, _ label: String, multiple: Bool) {
        guard chosen.indices.contains(i) else { return }
        if chosen[i].contains(label) {
            chosen[i].remove(label)
        } else {
            if !multiple { chosen[i].removeAll() }
            chosen[i].insert(label)
        }
    }

    private func binding(_ i: Int) -> Binding<String> {
        Binding(get: { custom.indices.contains(i) ? custom[i] : "" },
                set: { if custom.indices.contains(i) { custom[i] = $0 } })
    }

    private func answers() -> [[String]] {
        question.questions.indices.map { i in
            var a = Array(chosen.indices.contains(i) ? chosen[i] : [])
            let free = custom.indices.contains(i) ? custom[i].trimmingCharacters(in: .whitespaces) : ""
            if !free.isEmpty { a.append(free) }
            return a
        }
    }
}

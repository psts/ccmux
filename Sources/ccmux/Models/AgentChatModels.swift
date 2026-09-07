import Foundation

/// The agent chat wire, mirroring the daemon's chatFrame
/// (daemon/internal/api/agentchat.go) and the web lens (agentchat.js). One
/// envelope both ways; every field optional so a frame this build does not
/// know still decodes and is ignored by kind.
struct AgentChatFrame: Codable {
    var t: String
    var agent: String?
    var state: String?
    var session: String?
    var title: String?
    var turns: [AgentTurn]?
    var permissions: [AgentPermission]?
    var turn: AgentTurn?
    var part: AgentTurnPart?
    var messageId: String?
    var partId: String?
    var field: String?
    var delta: String?
    var permission: AgentPermission?
    var id: String?
    var reply: String?
    var questions: [AgentQuestion]?
    var question: AgentQuestion?
    var answers: [[String]]?
    var text: String?
    var resume: Bool?
    var error: String?

    init(t: String, text: String? = nil, id: String? = nil, reply: String? = nil, answers: [[String]]? = nil, resume: Bool? = nil) {
        self.t = t
        self.text = text
        self.id = id
        self.reply = reply
        self.answers = answers
        self.resume = resume
    }
}

/// The question tool asking the human to choose; the reply is the chosen
/// labels per question, in order.
struct AgentQuestion: Codable, Identifiable {
    var id: String
    var sessionID: String?
    var questions: [AgentQuestionInfo]
}

struct AgentQuestionInfo: Codable {
    var question: String
    var header: String?
    var options: [AgentQuestionOption]
    var multiple: Bool?
    var custom: Bool?
}

struct AgentQuestionOption: Codable, Identifiable {
    var label: String
    var description: String?
    var id: String { label }
}

/// One message of the conversation, in the daemon's normalized shape.
struct AgentTurn: Codable, Identifiable {
    var id: String
    var role: String
    var time: Int64
    var parts: [AgentTurnPart]
    var error: String?
    /// Set on a role "session" marker: the divider between two conversations.
    var title: String?

    init(id: String, role: String, time: Int64 = 0, parts: [AgentTurnPart] = [], error: String? = nil) {
        self.id = id
        self.role = role
        self.time = time
        self.parts = parts
        self.error = error
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        role = try c.decodeIfPresent(String.self, forKey: .role) ?? "assistant"
        time = try c.decodeIfPresent(Int64.self, forKey: .time) ?? 0
        parts = try c.decodeIfPresent([AgentTurnPart].self, forKey: .parts) ?? []
        error = try c.decodeIfPresent(String.self, forKey: .error)
        title = try c.decodeIfPresent(String.self, forKey: .title)
    }
}

/// One piece of a turn: text, reasoning, or a tool call with its state.
struct AgentTurnPart: Codable, Identifiable {
    var id: String
    var type: String
    var text: String?
    var tool: String?
    var status: String?
    var title: String?
    var input: AnyJSON?
    var output: String?
    var error: String?

    var inputText: String {
        guard let input else { return "" }
        return input.pretty
    }
}

/// Opencode asking before a tool runs; replies are once, always or reject.
struct AgentPermission: Codable, Identifiable {
    var id: String
    var sessionID: String?
    var permission: String
    var patterns: [String]?

    var summary: String {
        let pats = (patterns ?? []).joined(separator: ", ")
        return "\(permission): \(pats.isEmpty ? "(no pattern)" : pats)"
    }
}

/// A JSON value kept as-is (a tool's input is whatever the tool takes),
/// rendered pretty for the transcript.
enum AnyJSON: Codable {
    case string(String), number(Double), bool(Bool), null
    case array([AnyJSON]), object([String: AnyJSON])

    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() { self = .null; return }
        if let b = try? c.decode(Bool.self) { self = .bool(b); return }
        if let n = try? c.decode(Double.self) { self = .number(n); return }
        if let s = try? c.decode(String.self) { self = .string(s); return }
        if let a = try? c.decode([AnyJSON].self) { self = .array(a); return }
        self = .object(try c.decode([String: AnyJSON].self))
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch self {
        case .string(let s): try c.encode(s)
        case .number(let n): try c.encode(n)
        case .bool(let b): try c.encode(b)
        case .null: try c.encodeNil()
        case .array(let a): try c.encode(a)
        case .object(let o): try c.encode(o)
        }
    }

    var pretty: String {
        switch self {
        case .string(let s): return s
        case .number(let n): return n == n.rounded() ? String(Int(n)) : String(n)
        case .bool(let b): return String(b)
        case .null: return "null"
        case .array(let a): return "[" + a.map(\.pretty).joined(separator: ", ") + "]"
        case .object(let o):
            return o.keys.sorted().map { "\($0): \(o[$0]!.pretty)" }.joined(separator: "\n")
        }
    }
}

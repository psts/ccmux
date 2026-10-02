import Foundation

struct PeerMessage: Codable, Identifiable {
    let id: Int
    let from_id: String
    let to_id: String
    let from_name: String
    let to_name: String
    let text: String
    let sent_at: String
}

struct PeerInfo: Codable, Identifiable {
    let id: String
    let name: String
    let project: String
    let pid: Int
    let cwd: String
    let git_root: String
    let summary: String
    let last_seen: String
}

struct PeerWSMessage: Codable {
    let type: String
    /// The bus's number for this message, the same number history returns as
    /// `id`; what lets a read-back merge with live rows instead of replacing
    /// them. Optional only so a frame without one still decodes.
    let seq: Int?
    let from_id: String
    let from_name: String
    let from_summary: String?
    let from_cwd: String?
    let to_id: String
    let to_name: String
    let text: String
    let sent_at: String
}

extension PeerMessage {
    /// How many messages a history read asks for, in both lenses (peers.js
    /// HISTORY_LIMIT): a read that comes back this full may not reach back to
    /// what is on screen.
    static let historyLimit = 200

    /// History read back after a hello, merged into what is already on screen.
    /// Rows are keyed by the bus's message number, so a message that arrived
    /// live while the read was in flight is neither lost nor shown twice.
    /// A row on screen older than the history stays, unless the read came
    /// back full: then more may have been sent than it covers, and keeping
    /// those older rows would draw an unmarked hole between them and the
    /// history, so the list becomes what a fresh open shows. Rows with no
    /// number (id <= 0, a frame that carried none) keep their order at the
    /// end. peers.js mergeRows is the same rule.
    static func merged(history: [PeerMessage], onScreen: [PeerMessage], limit: Int = PeerMessage.historyLimit) -> [PeerMessage] {
        let floor = history.count >= limit ? (history.map(\.id).min() ?? 0) : 0
        var byID: [Int: PeerMessage] = [:]
        for m in onScreen where m.id > 0 && m.id >= floor { byID[m.id] = m }
        for m in history { byID[m.id] = m }
        return byID.values.sorted { $0.id < $1.id } + onScreen.filter { $0.id <= 0 }
    }

    /// The row a live frame adds, or nil when that message is already on
    /// screen (read back before its live copy landed); a duplicate id would
    /// garble the overlay's list, which is keyed on it. The row takes the
    /// bus's message number as its id, the number history uses, so a later
    /// read-back merges with it. A frame with no number gets the next local
    /// (negative) id.
    static func liveRow(_ m: PeerWSMessage, onScreen: [PeerMessage], nextLocalId: inout Int) -> PeerMessage? {
        let id: Int
        if let seq = m.seq {
            guard !onScreen.contains(where: { $0.id == seq }) else { return nil }
            id = seq
        } else {
            id = nextLocalId
            nextLocalId -= 1
        }
        return PeerMessage(id: id, from_id: m.from_id, to_id: m.to_id, from_name: m.from_name,
                           to_name: m.to_name, text: m.text, sent_at: m.sent_at)
    }
}

extension PeerWSMessage {
    /// The message one frame of the bus's listen stream carries, or nil when
    /// it carries none. The stream opens with a {"type":"hello"}, which has
    /// nothing to draw (the web lens skips every non-"message" frame the same
    /// way). A "message" frame that will not decode is a contract mismatch
    /// between app and daemon, so it is logged rather than dropped unseen.
    static func fromListenFrame(_ text: String) -> PeerWSMessage? {
        struct Kind: Decodable { let type: String }
        let data = Data(text.utf8)
        guard let kind = try? JSONDecoder().decode(Kind.self, from: data), kind.type == "message" else {
            return nil
        }
        do {
            return try JSONDecoder().decode(PeerWSMessage.self, from: data)
        } catch {
            NSLog("[ccmux peers] dropped an undecodable message frame: %@ (%d bytes)", "\(error)", data.count)
            return nil
        }
    }
}

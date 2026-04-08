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
    let from_id: String
    let from_name: String
    let from_summary: String?
    let from_cwd: String?
    let to_id: String
    let to_name: String
    let text: String
    let sent_at: String
}

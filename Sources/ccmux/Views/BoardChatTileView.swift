import SwiftUI

/// A passive agent tile, drawn as its chat: the last turns (a tap there
/// opens the tile, as on any tile), then the cards it waits on, the same
/// cards the chat shows. Those answer in place over REST: an answer names
/// its request, so it cannot land on a card that changed since the read.
/// The web lens's paintChat draws the same tile.
struct BoardChatTileView: View {
    let picture: BoardChatPicture
    let onOpen: () -> Void
    let onAnswer: (AgentChatFrame) async -> String?
    @State private var sending = false
    @State private var failure = ""

    /// One scroll, opened at the bottom, so the cards (last) are what shows.
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 12) {
                transcript
                    .contentShape(Rectangle())
                    .onTapGesture(perform: onOpen)
                cards
            }
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .defaultScrollAnchor(.bottom)
        // A new read is a new set of cards, live again and with no error
        // from the last: the answered card is gone from it, or still there
        // if the agent never took it. The web lens repaints the same way.
        .onChange(of: picture) {
            sending = false
            failure = ""
        }
    }

    @ViewBuilder
    private var transcript: some View {
        if picture.turns.isEmpty {
            Text("(no reply yet)").font(.system(size: 11)).foregroundColor(.secondary)
        }
        ForEach(picture.turns) { turn in
            AgentTurnRow(turn: turn, agent: picture.agent)
        }
    }

    /// The cards answer in place, so they never let a tap through to open
    /// the tile.
    private var cards: some View {
        VStack(spacing: 8) {
            ForEach(picture.permissions) { p in
                AgentPermissionCard(permission: p, agent: picture.agent) {
                    send(AgentChatFrame(t: "permission", id: p.id, reply: $0))
                }
            }
            ForEach(picture.questions) { q in
                AgentQuestionCard(question: q, agent: picture.agent,
                                  onAnswer: { send(AgentChatFrame(t: "question", id: q.id, answers: $0)) },
                                  onReject: { send(AgentChatFrame(t: "question-reject", id: q.id)) })
            }
            if !failure.isEmpty {
                Text("Not sent: \(failure)").font(.system(size: 11)).foregroundColor(.red)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .disabled(sending)
    }

    /// A sent answer keeps the cards off until the next read redraws them
    /// (onChange above), so a second tap cannot post the same answered id;
    /// a failure turns them back on to retry. Same as the web lens's answerCard.
    private func send(_ frame: AgentChatFrame) {
        sending = true
        failure = ""
        Task {
            guard let why = await onAnswer(frame) else { return }
            sending = false
            failure = why
        }
    }
}

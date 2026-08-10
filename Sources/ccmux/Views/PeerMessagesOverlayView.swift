import SwiftUI

struct PeerMessagesOverlayView: View {
    @ObservedObject var state: PeerMessagesState
    let onClose: () -> Void

    var body: some View {
        VStack(spacing: 0) {
            // Header
            HStack {
                Image(systemName: "antenna.radiowaves.left.and.right")
                    .font(.system(size: 12))
                    .foregroundColor(.accentColor)
                Text("Peer Messages")
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundColor(.white.opacity(0.9))
                Spacer()
                if state.isConnected {
                    Circle()
                        .fill(Color.green)
                        .frame(width: 6, height: 6)
                        .help("Connected")
                }
                Button(action: onClose) {
                    Image(systemName: "xmark")
                        .font(.system(size: 10, weight: .medium))
                        .foregroundColor(.white.opacity(0.5))
                        .frame(width: 20, height: 20)
                        .background(Color.white.opacity(0.1))
                        .cornerRadius(4)
                }
                .buttonStyle(.plain)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 12)

            Divider().background(Color.white.opacity(0.1))

            // Peers section
            if !state.peers.isEmpty {
                VStack(alignment: .leading, spacing: 6) {
                    Text("Connected Peers (\(state.peers.count))")
                        .font(.system(size: 10, weight: .medium))
                        .foregroundColor(.secondary)
                        .padding(.horizontal, 16)
                        .padding(.top, 8)

                    ScrollView(.horizontal, showsIndicators: false) {
                        HStack(spacing: 8) {
                            ForEach(state.peers) { peer in
                                peerBadge(peer)
                            }
                        }
                        .padding(.horizontal, 16)
                    }
                    .padding(.bottom, 8)
                }

                Divider().background(Color.white.opacity(0.1))
            }

            // Messages section
            if let error = state.error {
                errorBanner(error)
            } else if !state.isConnected && state.messages.isEmpty {
                VStack(spacing: 8) {
                    ProgressView()
                        .scaleEffect(0.7)
                    Text("Connecting...")
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else if state.messages.isEmpty {
                VStack(spacing: 8) {
                    Image(systemName: "bubble.left.and.bubble.right")
                        .font(.system(size: 24))
                        .foregroundColor(.secondary.opacity(0.4))
                    Text("No messages yet")
                        .font(.system(size: 11))
                        .foregroundColor(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 12) {
                            ForEach(state.messages) { message in
                                messageRow(message)
                                    .id(message.id)
                            }
                        }
                        .padding(16)
                    }
                    .onChange(of: state.messages.count) {
                        if let lastId = state.messages.last?.id {
                            withAnimation(.easeOut(duration: 0.2)) {
                                proxy.scrollTo(lastId, anchor: .bottom)
                            }
                        }
                    }
                }
            }
        }
        .frame(minWidth: 360, maxWidth: .infinity, minHeight: 300, maxHeight: .infinity)
        .background(
            RoundedRectangle(cornerRadius: 14)
                .fill(Color(red: 0.15, green: 0.16, blue: 0.17).opacity(0.98))
                .overlay(
                    RoundedRectangle(cornerRadius: 14)
                        .stroke(Color.white.opacity(0.1), lineWidth: 1)
                )
        )
        .clipShape(RoundedRectangle(cornerRadius: 14))
    }

    @ViewBuilder
    private func peerBadge(_ peer: PeerInfo) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 4) {
                Circle()
                    .fill(Color.green)
                    .frame(width: 5, height: 5)
                Text(peer.name)
                    .font(.system(size: 10, weight: .medium))
                    .foregroundColor(.white.opacity(0.85))
            }
            if !peer.summary.isEmpty {
                Text(peer.summary)
                    .font(.system(size: 9))
                    .foregroundColor(.white.opacity(0.7))
                    .lineLimit(1)
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 5)
        .background(Color.white.opacity(0.06))
        .cornerRadius(6)
    }

    @ViewBuilder
    private func messageRow(_ message: PeerMessage) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                Text(formatTime(message.sent_at))
                    .font(.system(size: 9, design: .monospaced))
                    .foregroundColor(.secondary.opacity(0.6))
                Text(message.from_name)
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundColor(Color(red: 0.47, green: 0.57, blue: 0.88))
                Image(systemName: "arrow.right")
                    .font(.system(size: 7))
                    .foregroundColor(.secondary.opacity(0.4))
                Text(message.to_name)
                    .font(.system(size: 10, weight: .semibold))
                    .foregroundColor(.orange)
            }
            Text(message.text)
                .font(.system(size: 12))
                .foregroundColor(.white.opacity(0.85))
                .textSelection(.enabled)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.white.opacity(0.03))
        .cornerRadius(8)
    }

    @ViewBuilder
    private func errorBanner(_ error: String) -> some View {
        VStack(spacing: 8) {
            Image(systemName: "exclamationmark.triangle")
                .font(.system(size: 24))
                .foregroundColor(.orange.opacity(0.7))
            Text(error)
                .font(.system(size: 11))
                .foregroundColor(.orange.opacity(0.8))
                .multilineTextAlignment(.center)
            Text("Sessions appear here once they register on the bus this daemon points at.")
                .font(.system(size: 10))
                .foregroundColor(.secondary)
                .multilineTextAlignment(.center)
        }
        .padding(24)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func formatTime(_ isoString: String) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        guard let date = formatter.date(from: isoString) else {
            // Try without fractional seconds
            formatter.formatOptions = [.withInternetDateTime]
            guard let date = formatter.date(from: isoString) else { return "" }
            return timeString(from: date)
        }
        return timeString(from: date)
    }

    private func timeString(from date: Date) -> String {
        let df = DateFormatter()
        df.dateFormat = "HH:mm"
        return df.string(from: date)
    }
}

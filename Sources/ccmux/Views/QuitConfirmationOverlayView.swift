import SwiftUI

class QuitConfirmationState: ObservableObject {
    @Published var progress: Double = 0.0
}

struct QuitConfirmationOverlayView: View {
    @ObservedObject var state: QuitConfirmationState

    var body: some View {
        VStack(spacing: 12) {
            Text("Hold ⌘Q to Quit")
                .font(.system(size: 14, weight: .medium))
                .foregroundColor(.white.opacity(0.85))

            GeometryReader { geo in
                Capsule()
                    .fill(Color.white.opacity(0.1))
                    .overlay(alignment: .leading) {
                        Capsule()
                            .fill(Color.accentColor)
                            .frame(width: geo.size.width * state.progress)
                    }
            }
            .frame(height: 6)
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 20)
        .frame(width: 260)
        .background(
            RoundedRectangle(cornerRadius: 14)
                .fill(Color(red: 0.11, green: 0.12, blue: 0.14).opacity(0.95))
                .overlay(
                    RoundedRectangle(cornerRadius: 14)
                        .stroke(Color.white.opacity(0.1), lineWidth: 1)
                )
        )
    }
}

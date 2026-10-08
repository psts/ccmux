import SwiftUI

/// The attention board: every pane of this window that is waiting on a human,
/// as a grid of tiles answered in place. The web lens's board.js is the same
/// feature with the same rules (AttentionBoard).
///
/// A passive tile is a picture: the pane's screen as text, or an agent's last
/// word, read over REST. It never resizes the shared pane. Clicking a tile
/// makes it ACTIVE: it hosts the live terminal or chat, which takes over the
/// pane's size like any lens that starts typing. Moving on from it (another
/// tile, Next, Done) tells the daemon "acted", which retires that claim.
struct AttentionBoardView: View {
    @ObservedObject var windowContext: WindowContext
    @StateObject private var model: AttentionBoardModel

    init(windowContext: WindowContext) {
        self.windowContext = windowContext
        _model = StateObject(wrappedValue: AttentionBoardModel())
    }

    private var windowName: String { windowContext.windowName ?? windowContext.autoName }

    var body: some View {
        GeometryReader { geo in
            VStack(spacing: 6) {
                strip
                if model.visible.isEmpty {
                    Text("Nothing needs you in \(windowName).")
                        .foregroundColor(.secondary)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    grid
                }
            }
            .padding(8)
            .onAppear {
                model.width = geo.size.width
                model.members = windowContext.ownedWorkspaceIds
                model.start()
            }
            .onChange(of: geo.size.width) {
                model.width = geo.size.width
                model.refresh()
            }
        }
        .background(Color(nsColor: NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)))
        .onChange(of: windowContext.ownedWorkspaceIds) {
            model.members = windowContext.ownedWorkspaceIds
            model.refresh()
        }
        .onDisappear { model.stop() }
    }

    // MARK: - Strip

    private var strip: some View {
        HStack(spacing: 6) {
            Text("Attention · \(windowName)").font(.system(size: 12, weight: .semibold))
            if !model.waiting.isEmpty {
                Text("+\(model.waiting.count) waiting:").foregroundColor(.secondary)
                ForEach(model.waiting) { c in
                    Button(c.name) { model.activate(c.pane) }
                        .foregroundColor(c.isBlocked ? .orange : .primary)
                }
            }
            Spacer()
            Button("Next ⌘↵") { model.next() }
                .help("Done with this one; open the next")
        }
        .font(.system(size: 11))
        .buttonStyle(.bordered)
        .controlSize(.small)
        // Cmd+Return is Next, except while a chat tile is active: Return
        // sends there, and Next would drop the draft. Same rule as the web lens.
        .background(
            Button("") { model.next() }
                .keyboardShortcut(.return, modifiers: .command)
                .disabled(model.activeIsChat)
                .opacity(0)
        )
    }

    // MARK: - Grid

    private var grid: some View {
        let cols = AttentionBoard.columns(for: model.visible.count)
        let rows = stride(from: 0, to: model.visible.count, by: cols).map {
            Array(model.visible[$0..<min($0 + cols, model.visible.count)])
        }
        return VStack(spacing: 6) {
            ForEach(rows.indices, id: \.self) { r in
                HStack(spacing: 6) {
                    ForEach(rows[r]) { c in tile(c) }
                    ForEach(0..<(cols - rows[r].count), id: \.self) { _ in
                        Color.clear.frame(maxWidth: .infinity, maxHeight: .infinity)
                    }
                }
            }
        }
    }

    private func tile(_ c: BoardClaim) -> some View {
        let isActive = c.pane == model.active
        return VStack(spacing: 0) {
            header(c, active: isActive)
            Divider()
            if isActive {
                liveView(c)
            } else {
                BoardPreviewView(preview: model.previews[c.pane])
                    .contentShape(Rectangle())
                    .onTapGesture { model.activate(c.pane) }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(nsColor: NSColor(red: 0.09, green: 0.10, blue: 0.11, alpha: 1.0)))
        .overlay(alignment: .leading) {
            Rectangle().fill(c.isBlocked ? Color.orange : Color.green).frame(width: 3)
        }
        .clipShape(RoundedRectangle(cornerRadius: 6))
        .overlay(RoundedRectangle(cornerRadius: 6).stroke(isActive ? Color.green : Color.gray.opacity(0.3)))
        .opacity(c.handled ? 0.5 : 1)
    }

    private func header(_ c: BoardClaim, active: Bool) -> some View {
        HStack(spacing: 8) {
            Text(c.name).font(.system(size: 12, weight: .semibold)).lineLimit(1)
            Text(c.reasonLabel).font(.system(size: 11)).foregroundColor(c.isBlocked ? .orange : .green)
            Text(AttentionBoard.waited(since: c.since, now: model.clock))
                .font(.system(size: 11)).foregroundColor(.secondary)
            Spacer()
            // A tile answers nothing by itself: to answer, open it.
            Button("Not now") { model.snooze(c) }
            if active {
                Button("Done") { model.next() }
            }
        }
        .buttonStyle(.bordered)
        .controlSize(.small)
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
    }

    /// The active tile's live view: the same views a workspace shows, so it
    /// is the real terminal (or chat), embedded here while the board is up.
    @ViewBuilder
    private func liveView(_ c: BoardClaim) -> some View {
        if c.chat, let ref = RemoteSessionService.shared.agentPanes[c.pane] {
            AgentPaneView(tabId: model.tabId(for: c.pane), paneId: c.pane, workingDirectory: c.workingDirectory,
                          agent: ref.agent, wsOrigin: ref.wsOrigin)
        } else {
            HostedTerminalPaneView(tabId: model.tabId(for: c.pane), paneId: c.pane, workingDirectory: c.workingDirectory)
        }
    }
}

/// A passive tile's picture. A screen is drawn monospaced and scaled so the
/// pane's whole width fits the tile; an agent's last word is plain text.
private struct BoardPreviewView: View {
    let preview: BoardPreview?

    var body: some View {
        GeometryReader { geo in
            if let preview, preview.cols > 0 {
                let size = max(4, min(13, geo.size.width / (CGFloat(preview.cols) * 0.6)))
                Text(preview.text)
                    .font(.system(size: size, design: .monospaced))
                    .fixedSize(horizontal: true, vertical: true)
                    .frame(width: geo.size.width, height: geo.size.height, alignment: .topLeading)
                    .padding(6)
                    .clipped()
            } else {
                Text(preview?.text ?? "…")
                    .font(.system(size: 12))
                    .frame(width: max(0, geo.size.width - 16), height: max(0, geo.size.height - 12),
                           alignment: .bottomLeading)
                    .padding(EdgeInsets(top: 6, leading: 8, bottom: 6, trailing: 8))
                    .clipped()
            }
        }
    }
}

/// An open window's "Attention" row: how many of its panes wait on a human,
/// and the way into its board. Same row and count rule as the web lens's
/// boardRow (snoozed claims do not count).
struct AttentionBoardRow: View {
    let members: Set<UUID>
    let isActive: Bool
    let onSelect: () -> Void
    @ObservedObject private var claims = RemoteSessionService.shared.claims
    @ObservedObject private var snoozes = BoardSnoozes.shared

    private var count: Int {
        let now = Date()
        return RemoteSessionService.shared.boardClaims(members: members)
            .filter { !AttentionBoard.isSnoozed($0, in: snoozes.byPane, now: now) }.count
    }

    var body: some View {
        let n = count
        HStack(spacing: 6) {
            Image(systemName: "scope")
                .foregroundColor(n > 0 ? .orange : .secondary)
                .frame(width: 14)
            Text("Attention")
                .foregroundColor(n > 0 ? .orange : .primary)
                .fontWeight(isActive ? .semibold : .regular)
            Spacer()
            if n > 0 {
                Text("\(n)")
                    .font(.system(size: 10, weight: .bold))
                    .padding(.horizontal, 6)
                    .background(Capsule().fill(Color.orange))
                    .foregroundColor(.black)
            }
        }
        .font(.system(size: 13))
        .contentShape(Rectangle())
        .onTapGesture(perform: onSelect)
    }
}

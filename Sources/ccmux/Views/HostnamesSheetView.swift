import SwiftUI

/// Per-workspace dev-hostname editor, presented as an AppKit sheet from the
/// hosted row's context menu (Hostnames…). Rows are {name, port}: the port is
/// whatever the app binds on its own (its script's -p, its config, the flag
/// the user types) — the daemon watches what the workspace's panes listen on
/// and routes each name there. "Listening now" shows that live list with a
/// Map button. The daemon validates (DNS label, port range, tailnet-wide
/// uniqueness) and its error text shows verbatim. Save replaces the whole
/// list (PUT semantics). Mirrors the web lens's hostnames modal.
struct HostnamesSheetView: View {
    let workspaceName: String
    let onSave: ([DaemonHostname], String) async -> String? // (rows, devCommand) → nil or error text
    let onCancel: () -> Void
    /// Detected rows + dev command from the repo's config files; prefilled when
    /// the workspace has nothing stored yet. nil = no detection (tests/previews).
    var fetchSuggestions: (() async -> DaemonSuggestionsResponse?)?

    @State private var rows: [EditableHostname]
    @State private var devCommand: String
    @State private var devCommandCaption = ""
    /// Detection alone: saving the field equal to this sends "" so the
    /// workspace keeps following the repo instead of freezing today's guess.
    @State private var detectedCommand = ""
    /// What the workspace's panes listen on right now; refreshed every 2 s
    /// while the sheet is open.
    @State private var listening: [DaemonListener] = []
    /// Non-empty when the stored command's repo-detected counterpart differs —
    /// shows the "use detected" badge under the command field.
    @State private var detectedChanged = ""
    @State private var status = ""
    @State private var saving = false

    struct EditableHostname: Identifiable {
        let id = UUID()
        var name: String
        var port: String
        var url: String?
        /// Which file a prefilled row came from ("docker-compose.yml") —
        /// shown as a caption so a detected guess is distinguishable.
        var source: String?
    }

    init(workspaceName: String, current: [DaemonHostname], devCommand: String = "",
         onSave: @escaping ([DaemonHostname], String) async -> String?, onCancel: @escaping () -> Void,
         fetchSuggestions: (() async -> DaemonSuggestionsResponse?)? = nil) {
        self.workspaceName = workspaceName
        self.onSave = onSave
        self.onCancel = onCancel
        self.fetchSuggestions = fetchSuggestions
        _rows = State(initialValue: current.map {
            EditableHostname(name: $0.name, port: $0.port == 0 ? "" : String($0.port), url: $0.url)
        })
        _devCommand = State(initialValue: devCommand)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Hostnames — \(workspaceName)")
                .font(.headline)
            Text("Each name serves this workspace's dev server over the tailnet. The port is the one the app binds on its own — ccmux never tells the server where to listen. The suffix comes from the daemon's Dev hostnames setting.")
                .font(.system(size: 11))
                .foregroundColor(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            ForEach($rows) { $row in
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 6) {
                        TextField(suggestedName, text: $row.name)
                            .textFieldStyle(.roundedBorder)
                            .font(.system(size: 12, design: .monospaced))
                        TextField("port", text: $row.port)
                            .textFieldStyle(.roundedBorder)
                            .font(.system(size: 12, design: .monospaced))
                            .frame(width: 70)
                            .help("The port the app binds on its own")
                        Button {
                            rows.removeAll { $0.id == row.id }
                        } label: {
                            Image(systemName: "xmark.circle.fill")
                                .foregroundColor(.secondary)
                        }
                        .buttonStyle(.borderless)
                        .help("Remove hostname")
                    }
                    if let caption = row.url ?? row.source.map({ "detected from \($0)" }) {
                        Text(caption)
                            .font(.system(size: 10, design: .monospaced))
                            .foregroundColor(.secondary)
                            .padding(.leading, 2)
                    }
                }
            }
            Button("Add hostname") {
                rows.append(EditableHostname(name: rows.isEmpty ? suggestedName : "", port: ""))
            }
            .controlSize(.small)

            listeningSection

            VStack(alignment: .leading, spacing: 2) {
                Text("Dev server command")
                    .font(.system(size: 11, weight: .semibold))
                Text("Runs in a workspace pane when you press ▶ on a hostname row — the pane is the log view. Prefilled from the repo; edit to override.")
                    .font(.system(size: 10))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                TextField("pnpm dev", text: $devCommand)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                if !devCommandCaption.isEmpty {
                    Text(devCommandCaption)
                        .font(.system(size: 10, design: .monospaced))
                        .foregroundColor(.secondary)
                        .padding(.leading, 2)
                }
                if !detectedChanged.isEmpty {
                    HStack(spacing: 6) {
                        Text("repo now detects: \(detectedChanged)")
                            .font(.system(size: 10, design: .monospaced))
                            .foregroundColor(.orange)
                        Button("Use detected") {
                            // Equal to detection = saved as "", so it keeps
                            // following the repo instead of freezing today's guess.
                            devCommand = detectedChanged
                            devCommandCaption = "detected from the repo — edit to override"
                            detectedChanged = ""
                        }
                        .controlSize(.mini)
                    }
                    .padding(.leading, 2)
                }
            }

            HStack {
                Text(status)
                    .font(.system(size: 11))
                    .foregroundColor(.red)
                    .lineLimit(2)
                Spacer()
                Button("Cancel", action: onCancel)
                    .keyboardShortcut(.cancelAction)
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(saving)
            }
        }
        .padding(18)
        .frame(width: 460)
        .task { await prefill() }
        .task { await pollListening() }
    }

    /// "Listening now": the workspace's live listeners. A port a row already
    /// names says "mapped"; any other gets Map, which adds a row for it.
    private var listeningSection: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text("Listening now")
                .font(.system(size: 11, weight: .semibold))
            if listening.isEmpty {
                Text("nothing yet — start the dev server in a pane and it shows up here")
                    .font(.system(size: 10))
                    .foregroundColor(.secondary)
            }
            ForEach(listening) { l in
                HStack(spacing: 8) {
                    Text(String(l.port))
                        .font(.system(size: 11, design: .monospaced))
                        .frame(width: 44, alignment: .leading)
                    Text(l.process ?? "")
                        .font(.system(size: 11, design: .monospaced))
                        .foregroundColor(.secondary)
                    if rows.contains(where: { $0.port.trimmingCharacters(in: .whitespaces) == String(l.port) }) {
                        Text("mapped")
                            .font(.system(size: 10))
                            .foregroundColor(.secondary)
                    } else {
                        Button("Map") { mapListener(l.port) }
                            .controlSize(.mini)
                    }
                }
            }
        }
    }

    /// Map adds a row for a live port, replacing a lone empty editor row.
    private func mapListener(_ port: Int) {
        if rows.count == 1, rows[0].name.isEmpty, rows[0].port.isEmpty {
            rows.removeAll()
        }
        let name = rows.isEmpty ? suggestedName : "\(suggestedName)-\(rows.count + 1)"
        rows.append(EditableHostname(name: name, port: String(port)))
    }

    /// Re-reads the live listeners every 2 s while the sheet is open, so a
    /// server started in a pane shows up without reopening.
    private func pollListening() async {
        guard let fetchSuggestions else { return }
        while !Task.isCancelled {
            try? await Task.sleep(nanoseconds: 2_000_000_000)
            if let s = await fetchSuggestions() { listening = s.listening ?? [] }
        }
    }

    /// Prefill an empty sheet with rows detected from the repo's config files
    /// (compose service names/ports, package.json dev scripts, EXPOSE) and the
    /// live listeners. The command field is PREFILLED with the detected
    /// command when nothing is stored; saving it unchanged sends "" so the
    /// workspace keeps following the repo.
    private func prefill() async {
        guard let fetchSuggestions, let detected = await fetchSuggestions() else { return }
        listening = detected.listening ?? []
        if rows.isEmpty {
            rows = (detected.suggestions ?? []).map {
                EditableHostname(name: $0.name, port: String($0.port), source: $0.source)
            }
        }
        detectedCommand = detected.detectedCommand ?? ""
        if devCommand.isEmpty, let command = detected.devCommand, !command.isEmpty,
           detected.devCommandSource != "workspace setting" {
            devCommand = command
            devCommandCaption = "detected from \(detected.devCommandSource ?? "the repo") — edit to override"
        } else if !devCommand.isEmpty, !detectedCommand.isEmpty, detectedCommand != devCommand {
            detectedChanged = detectedCommand
        }
    }

    /// Default first-row name: the workspace slug ("ChartLabs" → "chartlabs"),
    /// the same label the daemon's own suggestions and the web lens use.
    private var suggestedName: String {
        let slug = workspaceName.lowercased()
            .replacingOccurrences(of: "[^a-z0-9]+", with: "-", options: .regularExpression)
            .trimmingCharacters(in: CharacterSet(charactersIn: "-"))
        return slug.isEmpty ? "app" : slug
    }

    private func save() async {
        var outgoing: [DaemonHostname] = []
        for row in rows {
            let name = row.name.trimmingCharacters(in: .whitespaces)
            let portText = row.port.trimmingCharacters(in: .whitespaces)
            if name.isEmpty && portText.isEmpty {
                continue // half-empty editor row, not a mapping
            }
            guard let port = Int(portText), (1...65535).contains(port) else {
                status = "\(name.isEmpty ? "row" : name): port must be 1–65535 — the port the app binds itself"
                return
            }
            outgoing.append(DaemonHostname(name: name, port: port))
        }
        saving = true
        defer { saving = false }
        // The prefilled detected command, saved untouched, stays "" on the
        // daemon so it keeps following the repo. Same rule as the web lens.
        var command = devCommand.trimmingCharacters(in: .whitespaces)
        if command == detectedCommand { command = "" }
        if let error = await onSave(outgoing, command) {
            status = error
        }
    }
}

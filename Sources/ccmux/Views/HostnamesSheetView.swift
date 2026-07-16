import SwiftUI

/// Per-workspace dev-hostname editor, presented as an AppKit sheet from the
/// hosted row's context menu (Hostnames…). Rows are {name, port}; the daemon
/// validates (DNS label, port range, tailnet-wide uniqueness) and its error
/// text shows verbatim. Save replaces the whole list (PUT semantics).
struct HostnamesSheetView: View {
    let workspaceName: String
    let onSave: ([DaemonHostname]) async -> String? // nil = saved, else error text
    let onCancel: () -> Void
    /// Detected {name, port} rows from the repo's config files; prefilled when
    /// the workspace has no mappings yet. nil = no detection (tests/previews).
    var fetchSuggestions: (() async -> [DaemonPortSuggestion])?

    @State private var rows: [EditableHostname]
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

    init(workspaceName: String, current: [DaemonHostname],
         onSave: @escaping ([DaemonHostname]) async -> String?, onCancel: @escaping () -> Void,
         fetchSuggestions: (() async -> [DaemonPortSuggestion])? = nil) {
        self.workspaceName = workspaceName
        self.onSave = onSave
        self.onCancel = onCancel
        self.fetchSuggestions = fetchSuggestions
        _rows = State(initialValue: current.map {
            EditableHostname(name: $0.name, port: String($0.port), url: $0.url)
        })
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Hostnames — \(workspaceName)")
                .font(.headline)
            Text("Each name serves this workspace's dev server over the tailnet: name + port → https URL. The suffix comes from the daemon's Dev hostnames setting.")
                .font(.system(size: 11))
                .foregroundColor(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            ForEach($rows) { $row in
                VStack(alignment: .leading, spacing: 2) {
                    HStack(spacing: 6) {
                        TextField(suggestedName, text: $row.name)
                            .textFieldStyle(.roundedBorder)
                            .font(.system(size: 12, design: .monospaced))
                        TextField("3000", text: $row.port)
                            .textFieldStyle(.roundedBorder)
                            .font(.system(size: 12, design: .monospaced))
                            .frame(width: 70)
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
    }

    /// Prefill an empty sheet with rows detected from the repo's config files
    /// (compose service names/ports, package.json dev scripts, EXPOSE). Rows
    /// are ordinary editable rows — delete or rename before saving as usual.
    private func prefill() async {
        guard rows.isEmpty, let fetchSuggestions else { return }
        rows = (await fetchSuggestions()).map {
            EditableHostname(name: $0.name, port: String($0.port), source: $0.source)
        }
    }

    /// Default first-row name: the workspace slug ("ChartLabs" → "chartlabs-app").
    private var suggestedName: String {
        let slug = workspaceName.lowercased()
            .replacingOccurrences(of: "[^a-z0-9]+", with: "-", options: .regularExpression)
            .trimmingCharacters(in: CharacterSet(charactersIn: "-"))
        return slug.isEmpty ? "app" : slug + "-app"
    }

    private func save() async {
        var outgoing: [DaemonHostname] = []
        for row in rows {
            let name = row.name.trimmingCharacters(in: .whitespaces)
            if name.isEmpty && row.port.trimmingCharacters(in: .whitespaces).isEmpty {
                continue // half-empty editor row, not a mapping
            }
            guard let port = Int(row.port), (1...65535).contains(port) else {
                status = "\(name.isEmpty ? "row" : name): port must be 1–65535"
                return
            }
            outgoing.append(DaemonHostname(name: name, port: port))
        }
        saving = true
        defer { saving = false }
        if let error = await onSave(outgoing) {
            status = error
        }
    }
}

import SwiftUI

/// The llm account editor: one sheet for a single account, mirroring the web
/// lens's account modal — name, kind, upstream, credential, model aliases and
/// the upstream's model list.
///
/// The list behind it stays a summary (what the account is and how it is
/// doing), because the Accounts tab is read far more often than it is edited
/// and its order arrows are the thing you reach for. Everything you SET
/// rather than read lives here.
///
/// It edits a copy and hands it back on Save: the settings view owns the
/// account array, and the daemon replaces that array wholesale, so a
/// half-typed row must not reach it.
struct AccountEditorView: View {
    /// Live health for this account, as the row shows it; nil when it has
    /// never been asked for anything.
    let statusText: String
    let onDone: (DaemonSettingsView.EditableAccount?) -> Void

    @State private var account: DaemonSettingsView.EditableAccount
    @State private var models: [String] = []
    @State private var modelsNote = ""

    init(
        account: DaemonSettingsView.EditableAccount?,
        statusText: String,
        onDone: @escaping (DaemonSettingsView.EditableAccount?) -> Void
    ) {
        self.statusText = statusText
        self.onDone = onDone
        _account = State(
            initialValue: account
                ?? DaemonSettingsView.EditableAccount(
                    name: "", kind: "anthropic", baseURL: "", apiKey: "",
                    apiKeySet: false, aliases: ""))
    }

    var body: some View {
        VStack(spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                    HStack(spacing: 6) {
                        TextField("name", text: $account.name)
                            .font(.system(size: 12, design: .monospaced))
                        // The one list of kinds this app has; a fifth copy
                        // is how they drift apart.
                        Picker("", selection: $account.kind) {
                            ForEach(DaemonSettingsView.accountKindOptions, id: \.self) { Text($0).tag($0) }
                        }
                        .labelsHidden()
                        .frame(width: 110)
                    }
                    TextField(urlHint, text: $account.baseURL)
                        .font(.system(size: 12, design: .monospaced))
                    SecureField(keyHint, text: $account.apiKey)
                        .font(.system(size: 12, design: .monospaced))
                    HStack(spacing: 6) {
                        TextField("aliases: claude-haiku-*=qwen3-4b-32k", text: $account.aliases)
                            .font(.system(size: 12, design: .monospaced))
                        Picker("", selection: modelPick) {
                            Text("map claude → …").tag("")
                            ForEach(models, id: \.self) { Text($0).tag($0) }
                        }
                        .labelsHidden()
                        .frame(width: 190)
                        .help("List the upstream's models; picking one maps every claude-* request to it")
                    }
                    if !modelsNote.isEmpty {
                        Text(modelsNote).font(.system(size: 11)).foregroundColor(.secondary)
                    }
                    if !statusText.isEmpty {
                        Text(statusText).font(.system(size: 11)).foregroundColor(.secondary)
                    }
                }
                .padding(12)
            }
            Divider()
            HStack {
                Spacer()
                Button("Cancel") { onDone(nil) }
                    .keyboardShortcut(.cancelAction)
                Button("Save") { onDone(account) }
                    .keyboardShortcut(.defaultAction)
                    .disabled(account.name.trimmingCharacters(in: .whitespaces).isEmpty)
            }
            .padding(12)
        }
        .frame(minWidth: 480, idealWidth: 560, minHeight: 260)
        .textFieldStyle(.roundedBorder)
        .font(.system(size: 12))
        // One upstream, asked when you open it. The list used to ask every
        // account's upstream every time the tab opened.
        .task { await loadModels() }
    }

    private var urlHint: String {
        account.kind == "meridian"
            ? "base URL (empty = http://127.0.0.1:3456)" : "base URL, e.g. http://localhost:11434"
    }

    /// The credential line. A stored key is never echoed, so an empty box
    /// means "keep it" rather than "clear it" — the daemon's write-only rule.
    private var keyHint: String {
        if account.apiKeySet { return "key set — empty keeps it" }
        switch account.kind {
        case "claude": return "paste `claude setup-token` output"
        case "meridian": return "paste `claude setup-token` output (starts the sidecar)"
        default: return "api key (empty = your own login)"
        }
    }

    /// Picking a model rewrites the alias field: claude-* rules replaced,
    /// custom rules kept. Reads back the claude-* target so the picker keeps
    /// showing the choice.
    private var modelPick: Binding<String> {
        Binding(
            get: {
                DaemonSettingsView.parseAliases(account.aliases)
                    .first { $0["from"] == "claude-*" }?["to"] ?? ""
            },
            set: { model in
                guard !model.isEmpty else { return }
                let kept = DaemonSettingsView.parseAliases(account.aliases)
                    .filter { !($0["from"]?.hasPrefix("claude-") ?? false) }
                account.aliases = (kept + [["from": "claude-*", "to": model]])
                    .compactMap { pair in
                        guard let f = pair["from"], let t = pair["to"] else { return nil }
                        return "\(f)=\(t)"
                    }
                    .joined(separator: ", ")
            })
    }

    /// An upstream that doesn't answer says why: an empty picker that hides
    /// the reason reads as a broken feature.
    private func loadModels() async {
        let name = account.name.trimmingCharacters(in: .whitespaces)
        guard !name.isEmpty else { return } // unsaved: no upstream to ask yet
        let res = await RemoteSessionService.shared.fetchAccountModels(name)
        if let err = res.error {
            modelsNote = "couldn't list models: " + err
            return
        }
        models = res.models
    }
}

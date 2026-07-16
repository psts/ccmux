import SwiftUI

/// The app's Settings window content: edits the DAEMON-wide new-workspace
/// startup command and its per-folder overrides. The daemon is the single
/// source of truth (the web lens edits the same values), so this view is a
/// thin editor over GET/PUT /v1/settings — nothing is stored app-side.
struct DaemonSettingsView: View {
    @State private var command = ""
    @State private var rules: [EditableRule] = []
    @State private var status = ""
    @State private var loaded = false

    struct EditableRule: Identifiable {
        let id = UUID()
        var pathPrefix: String
        var command: String
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            VStack(alignment: .leading, spacing: 4) {
                Text("New workspace startup command")
                    .font(.headline)
                Text("Typed into every new hosted workspace's terminal, in every lens. Clear it to reset to the built-in default.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                TextField("claude …", text: $command)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
            }

            VStack(alignment: .leading, spacing: 4) {
                Text("Per-folder overrides")
                    .font(.headline)
                Text("Repos under a folder use its command instead; the longest matching folder wins.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                ForEach($rules) { $rule in
                    HStack(spacing: 6) {
                        TextField("/path/to/folder", text: $rule.pathPrefix)
                            .textFieldStyle(.roundedBorder)
                            .font(.system(size: 11, design: .monospaced))
                            .frame(minWidth: 220)
                        TextField("command", text: $rule.command)
                            .textFieldStyle(.roundedBorder)
                            .font(.system(size: 11, design: .monospaced))
                        Button {
                            rules.removeAll { $0.id == rule.id }
                        } label: {
                            Image(systemName: "xmark.circle.fill")
                                .foregroundColor(.secondary)
                        }
                        .buttonStyle(.borderless)
                        .help("Remove rule")
                    }
                }
                Button("Add folder rule") {
                    rules.append(EditableRule(pathPrefix: "", command: ""))
                }
                .controlSize(.small)
            }

            HStack {
                Text(status)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                Spacer()
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(!loaded)
            }
        }
        .padding(18)
        .frame(width: 560)
        .task { await load() }
    }

    private func load() async {
        do {
            apply(try await RemoteSessionService.shared.fetchSettings())
            status = ""
            loaded = true
        } catch {
            status = "Couldn't reach ccmuxd at \(DaemonConfig.baseURL)"
        }
    }

    private func save() async {
        let outgoing = rules.map { DaemonStartupRule(pathPrefix: $0.pathPrefix, command: $0.command) }
        guard let saved = await RemoteSessionService.shared.updateSettings(
            startupCommand: command, startupRules: outgoing) else {
            status = "Couldn't save — is the daemon running?"
            return
        }
        apply(saved) // show the resolved command + the rules that survived validation
        status = "Saved."
    }

    private func apply(_ settings: DaemonSettings) {
        command = settings.startupCommand
        rules = settings.startupRules.map { EditableRule(pathPrefix: $0.pathPrefix, command: $0.command) }
    }
}

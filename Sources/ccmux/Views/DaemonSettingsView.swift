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
    @State private var devDomain = ""
    @State private var cloudflareToken = ""    // typed value only; "" = leave unchanged
    @State private var tailscaleAuthKey = ""   // typed value only; "" = leave unchanged
    @State private var cloudflareTokenSet = false
    @State private var tailscaleAuthKeySet = false
    @State private var devCertStatus = "unset"

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

            VStack(alignment: .leading, spacing: 4) {
                Text("Dev hostnames")
                    .font(.headline)
                Text("Serves workspace dev servers over the tailnet (right-click a hosted workspace → Hostnames…). With a domain: https://<name>.<domain> via one wildcard cert (needs a Cloudflare DNS-edit token for the zone). Without: one ts.net node per hostname (the auth key registers them silently).")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                TextField("dev.sanlabs.io (empty = ts.net mode)", text: $devDomain)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                SecureField(cloudflareTokenSet ? "Cloudflare API token (set — type to replace)" : "Cloudflare API token", text: $cloudflareToken)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                SecureField(tailscaleAuthKeySet ? "Tailscale auth key (set — type to replace)" : "Tailscale auth key (optional, ts.net mode)", text: $tailscaleAuthKey)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                if devCertStatus != "unset" {
                    Text("Wildcard cert: \(devCertStatus)")
                        .font(.system(size: 11))
                        .foregroundColor(devCertStatus == "ready" ? .green : devCertStatus.hasPrefix("error") ? .red : .secondary)
                }
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
        // Secrets are write-only: an untouched field stays nil (unchanged on the
        // daemon); typed text replaces. The domain is plain state, always sent.
        guard let saved = await RemoteSessionService.shared.updateSettings(
            startupCommand: command, startupRules: outgoing,
            devDomain: devDomain,
            cloudflareToken: cloudflareToken.isEmpty ? nil : cloudflareToken,
            tailscaleAuthKey: tailscaleAuthKey.isEmpty ? nil : tailscaleAuthKey) else {
            status = "Couldn't save — a domain needs a Cloudflare token, and the daemon must be running."
            return
        }
        apply(saved) // show the resolved command + the rules that survived validation
        status = "Saved."
    }

    private func apply(_ settings: DaemonSettings) {
        command = settings.startupCommand
        rules = settings.startupRules.map { EditableRule(pathPrefix: $0.pathPrefix, command: $0.command) }
        devDomain = settings.devDomain
        cloudflareTokenSet = settings.cloudflareTokenSet
        tailscaleAuthKeySet = settings.tailscaleAuthKeySet
        devCertStatus = settings.devCertStatus
        cloudflareToken = ""
        tailscaleAuthKey = ""
    }
}

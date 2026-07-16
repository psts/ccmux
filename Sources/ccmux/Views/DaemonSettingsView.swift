import SwiftUI

/// The app's Settings window content: edits the DAEMON-wide new-workspace
/// startup command and its per-folder overrides. The daemon is the single
/// source of truth (the web lens edits the same values), so this view is a
/// thin editor over GET/PUT /v1/settings — nothing is stored app-side.
struct DaemonSettingsView: View {
    /// Called after a fully successful save (cert ready when a domain is set);
    /// the owner closes the window.
    var onDone: (() -> Void)?

    @State private var command = ""
    @State private var rules: [EditableRule] = []
    @State private var status = ""
    @State private var saving = false
    @State private var loaded = false
    @State private var devDomain = ""
    @State private var cloudflareToken = ""
    @State private var tailscaleAuthKey = ""
    @State private var cloudflareTokenSet = false
    @State private var tailscaleAuthKeySet = false
    @State private var devCertStatus = "unset"

    /// Stands in for a stored secret the daemon never echoes back: renders as
    /// dots so the field doesn't look mysteriously wiped after save. Untouched
    /// sentinel = leave unchanged; emptied field = clear; anything else = replace.
    private static let secretSentinel = "••••••••"

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
                SecureField("Cloudflare API token", text: $cloudflareToken)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                SecureField("Tailscale auth key (optional, ts.net mode)", text: $tailscaleAuthKey)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                if devCertStatus != "unset" {
                    Text("Wildcard cert: \(devCertStatus)")
                        .font(.system(size: 11))
                        .foregroundColor(devCertStatus == "ready" ? .green : devCertStatus.hasPrefix("error") ? .red : .secondary)
                }
            }

            HStack(spacing: 6) {
                if saving {
                    ProgressView()
                        .controlSize(.small)
                }
                Text(status)
                    .font(.system(size: 11))
                    .foregroundColor(statusColor)
                    .lineLimit(2)
                Spacer()
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(!loaded || saving)
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

    private var statusColor: Color {
        if status.hasPrefix("✓") { return .green }
        if status.hasPrefix("✗") { return .red }
        return .secondary
    }

    /// What to send for a secret field: untouched sentinel = nil (unchanged),
    /// emptied-after-set = "" (clear), anything else = the new value.
    private func outgoingSecret(_ field: String, wasSet: Bool) -> String? {
        if field == Self.secretSentinel { return nil }
        if field.isEmpty { return wasSet ? "" : nil }
        return field
    }

    /// Save, then (when a domain is set) wait for the wildcard cert verdict —
    /// spinner while issuing, ✓ then auto-close on success, ✗ stays open.
    private func save() async {
        saving = true
        defer { saving = false }
        status = "Saving…"
        let outgoing = rules.map { DaemonStartupRule(pathPrefix: $0.pathPrefix, command: $0.command) }
        guard let saved = await RemoteSessionService.shared.updateSettings(
            startupCommand: command, startupRules: outgoing,
            devDomain: devDomain,
            cloudflareToken: outgoingSecret(cloudflareToken, wasSet: cloudflareTokenSet),
            tailscaleAuthKey: outgoingSecret(tailscaleAuthKey, wasSet: tailscaleAuthKeySet)) else {
            status = "✗ Couldn't save — a domain needs a Cloudflare token, and the daemon must be running."
            return
        }
        apply(saved) // show the resolved command + the rules that survived validation
        if !saved.devDomain.isEmpty && !isCertVerdict(saved.devCertStatus) {
            status = "Issuing wildcard cert — usually well under a minute…"
            devCertStatus = await pollCertStatus()
        }
        if devCertStatus.hasPrefix("error") {
            status = "✗ \(devCertStatus)"
            return
        }
        if !devDomain.isEmpty && devCertStatus != "ready" {
            status = "Cert still issuing — it finishes in the background; reopen Settings to check."
            return
        }
        status = "✓ Saved"
        try? await Task.sleep(nanoseconds: 800_000_000)
        onDone?()
    }

    private func isCertVerdict(_ s: String) -> Bool { s == "ready" || s.hasPrefix("error") }

    /// Poll the daemon until the cert reaches a verdict (~2 min cap — DNS-01
    /// with pinned public resolvers normally lands in seconds).
    private func pollCertStatus() async -> String {
        for _ in 0..<60 {
            try? await Task.sleep(nanoseconds: 2_000_000_000)
            guard let s = try? await RemoteSessionService.shared.fetchSettings() else { continue }
            if isCertVerdict(s.devCertStatus) { return s.devCertStatus }
        }
        return "pending"
    }

    private func apply(_ settings: DaemonSettings) {
        command = settings.startupCommand
        rules = settings.startupRules.map { EditableRule(pathPrefix: $0.pathPrefix, command: $0.command) }
        devDomain = settings.devDomain
        cloudflareTokenSet = settings.cloudflareTokenSet
        tailscaleAuthKeySet = settings.tailscaleAuthKeySet
        devCertStatus = settings.devCertStatus
        // Stored secrets render as dots (the daemon never echoes them); an
        // untouched sentinel round-trips as "unchanged".
        cloudflareToken = cloudflareTokenSet ? Self.secretSentinel : ""
        tailscaleAuthKey = tailscaleAuthKeySet ? Self.secretSentinel : ""
    }
}

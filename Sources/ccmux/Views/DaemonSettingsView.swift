import SwiftUI

/// The app's Settings window content: edits the DAEMON-wide settings —
/// identity, llm accounts, the harness registry with its per-folder preselect
/// rules, dev hostnames. The daemon is the single source of truth (the web
/// lens edits the same values), so this view is a thin editor over GET/PUT
/// /v1/settings — nothing is stored app-side.
struct DaemonSettingsView: View {
    /// Called after a fully successful save (cert ready when a domain is set);
    /// the owner closes the window.
    var onDone: (() -> Void)?

    @State private var identity = ""
    @State private var rules: [EditableRule] = []
    @State private var status = ""
    @State private var saving = false
    @State private var loaded = false
    @State private var devDomain = ""
    @State private var lensHostname = ""
    @State private var cloudflareToken = ""
    @State private var tailscaleAuthKey = ""
    @State private var cloudflareTokenSet = false
    @State private var tailscaleAuthKeySet = false
    @State private var devCertStatus = "unset"
    @State private var llmRoute = ""
    @State private var accounts: [EditableAccount] = []
    /// Live per-account health by name, from GET /v1/settings.
    @State private var accountStatus: [String: DaemonLLMAccountStatus] = [:]
    @State private var harnesses: [EditableHarness] = []
    @State private var supportsLLM = false
    @State private var supportsHarnesses = false
    @State private var supportsHarnessRules = false

    /// Stands in for a stored secret the daemon never echoes back: renders as
    /// dots so the field doesn't look mysteriously wiped after save. Untouched
    /// sentinel = leave unchanged; emptied field = clear; anything else = replace.
    private static let secretSentinel = "••••••••"

    struct EditableRule: Identifiable {
        let id = UUID()
        var pathPrefix: String
        var harness: String
    }

    struct EditableAccount: Identifiable {
        let id = UUID()
        var name: String
        var kind: String
        var baseURL: String
        /// Empty = keep the stored key (the daemon's write-only semantics).
        var apiKey: String
        var apiKeySet: Bool
        /// "from=to, from2=to2" — parsed on save.
        var aliases: String
    }

    struct EditableHarness: Identifiable {
        let id = UUID()
        var icon: String
        var name: String
        var command: String
        var autoconfirm: Bool
        let source: String
        /// The daemon-resolved defaults this row started as: an untouched
        /// builtin/detected row is NOT persisted, so it stays live-resolved.
        let orig: [String]

        var untouchedDefault: Bool {
            !source.isEmpty && orig == [icon, name, command, autoconfirm ? "1" : "0"]
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            // One page per concern instead of one long scroll — mirrors the
            // web lens's settings tabs.
            TabView {
                generalTab.tabItem { Text("General") }
                if supportsLLM { accountsTab.tabItem { Text("Accounts") } }
                if supportsHarnesses { harnessesTab.tabItem { Text("Harnesses") } }
                devTab.tabItem { Text("Dev Hostnames") }
            }
            // FIXED height: a window that resizes per tab makes its own tab
            // bar jump under the pointer; each tab scrolls inside instead.
            .frame(height: 440)
            saveBar
        }
        .padding(18)
        .frame(width: 640)
        .task { await load() }
    }

    private var generalTab: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                identitySection
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var devTab: some View {
        ScrollView {
            devHostnamesSection
                .padding(10)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private var identitySection: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Who you are")
                .font(.headline)
            Text("Your Tailscale login email. Notifications and phone-push muting match on it, so it must be the same address your phone signs in with. Empty uses your macOS name, which then needs an identity alias on the daemon.")
                .font(.system(size: 11))
                .foregroundColor(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            TextField("you@example.com", text: $identity)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12, design: .monospaced))
        }
    }

    /// Which harness a new workspace under a folder PRESELECTS on its harness
    /// bar. A rule may name a harness deleted in this session — the picker
    /// keeps the name visible so the row stays editable; the daemon falls back
    /// to claude when resolving it.
    private var rulesSection: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("Per-folder default harness")
                .font(.headline)
            Text("New workspaces under a folder preselect this harness (nothing auto-starts); the longest matching folder wins.")
                .font(.system(size: 11))
                .foregroundColor(.secondary)
            ForEach($rules) { $rule in
                HStack(spacing: 6) {
                    TextField("/path/to/folder", text: $rule.pathPrefix)
                        .textFieldStyle(.roundedBorder)
                        .font(.system(size: 11, design: .monospaced))
                        .frame(minWidth: 220)
                    Picker("", selection: $rule.harness) {
                        ForEach(ruleHarnessNames(current: rule.harness), id: \.self) { name in
                            Text(name).tag(name)
                        }
                    }
                    .labelsHidden()
                    .frame(minWidth: 120)
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
                rules.append(EditableRule(pathPrefix: "", harness: harnesses.first?.name ?? "claude"))
            }
            .controlSize(.small)
        }
    }

    /// The current harness names, plus the rule's own name when it points at
    /// one that no longer exists (a Picker with a selection outside its
    /// options renders empty and silently rewrites on first touch).
    private func ruleHarnessNames(current: String) -> [String] {
        var names = harnesses.map(\.name).filter { !$0.isEmpty }
        if !current.isEmpty && !names.contains(current) { names.append(current) }
        return names
    }

    private var devHostnamesSection: some View {
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
            TextField("ccmux (serves this web UI at <name>.<domain>; empty = off)", text: $lensHostname)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12, design: .monospaced))
                .disabled(devDomain.isEmpty)
                .help("Reserved name for the ccmux web UI itself, e.g. \"ccmux\" → https://ccmux.\(devDomain.isEmpty ? "<domain>" : devDomain). Needs a dev domain.")
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
    }

    private var accountsTab: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 8) {
                Text("Default account (all panes)")
                    .font(.headline)
                Text("Which account answers a pane with no override of its own. Applies to each pane's next request — no restarts.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Picker("", selection: $llmRoute) {
                    Text("Anthropic (direct, your Claude login)").tag("")
                    ForEach(accounts) { a in
                        if !a.name.isEmpty { Text(a.name).tag(a.name) }
                    }
                }
                .labelsHidden()
                Text("Accounts")
                    .font(.headline)
                    .padding(.top, 6)
                Text("Claude accounts hold a token from `claude setup-token` and form a failover pool: when one hits its limit, the proxy switches to the next. Usage updates from live traffic.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                ForEach($accounts) { $account in
                    accountCard($account)
                }
                Button("Add account") {
                    accounts.append(EditableAccount(
                        name: "", kind: "anthropic", baseURL: "", apiKey: "", apiKeySet: false, aliases: ""))
                }
                .controlSize(.small)
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    /// The health footer of one account card, from the daemon's live status.
    private func accountStatusText(_ name: String) -> String? {
        guard let st = accountStatus[name] else { return nil }
        var parts: [String] = []
        switch st.state {
        case "ok": parts.append("● active")
        case "limited": parts.append("◐ limited" + (st.limitedUntil.map { " until \($0)" } ?? ""))
        case "unauthorized": parts.append("✕ credential rejected")
        case "untried": parts.append("○ no traffic yet")
        default: parts.append(st.state)
        }
        if st.sessionPct >= 0 { parts.append("session \(Int(st.sessionPct))%") }
        if st.weeklyPct >= 0 { parts.append("week \(Int(st.weeklyPct))%") }
        return parts.joined(separator: " · ")
    }

    private func accountCard(_ account: Binding<EditableAccount>) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                TextField("name", text: account.name)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                Picker("", selection: account.kind) {
                    Text("anthropic").tag("anthropic")
                    Text("openai").tag("openai")
                    Text("claude").tag("claude")
                    Text("codex").tag("codex")
                }
                .labelsHidden()
                .frame(width: 110)
                Button {
                    if llmRoute == account.wrappedValue.name { llmRoute = "" }
                    accounts.removeAll { $0.id == account.wrappedValue.id }
                } label: {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundColor(.secondary)
                }
                .buttonStyle(.borderless)
                .help("Remove account")
            }
            TextField("base URL, e.g. http://localhost:11434", text: account.baseURL)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12, design: .monospaced))
            HStack(spacing: 6) {
                SecureField(
                    account.wrappedValue.apiKeySet ? "key set — empty keeps it"
                        : account.wrappedValue.kind == "claude" ? "paste `claude setup-token` output"
                        : "api key (empty = your own login)",
                    text: account.apiKey)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                TextField("aliases: claude-haiku-*=qwen3-4b-32k", text: account.aliases)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
            }
            if let status = accountStatusText(account.wrappedValue.name) {
                Text(status)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
            }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.white.opacity(0.12)))
    }

    private var harnessesTab: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 8) {
                Text("Installed harnesses appear on their own; editing a builtin/detected row saves an override, deleting nothing — untouched rows stay live-resolved. The command field is where per-harness flags live.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                ForEach($harnesses) { $harness in
                    harnessCard($harness)
                }
                Button("Add harness") {
                    harnesses.append(EditableHarness(
                        icon: "", name: "", command: "", autoconfirm: false, source: "", orig: []))
                }
                .controlSize(.small)
                if supportsHarnessRules {
                    rulesSection
                        .padding(.top, 8)
                }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }

    private func harnessCard(_ harness: Binding<EditableHarness>) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                TextField("✳", text: harness.icon)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12))
                    .frame(width: 44)
                TextField("name", text: harness.name)
                    .textFieldStyle(.roundedBorder)
                    .font(.system(size: 12, design: .monospaced))
                if !harness.wrappedValue.source.isEmpty {
                    Text(harness.wrappedValue.source)
                        .font(.system(size: 10))
                        .foregroundColor(.secondary)
                        .padding(.horizontal, 7)
                        .padding(.vertical, 2)
                        .overlay(Capsule().strokeBorder(Color.white.opacity(0.15)))
                }
                Toggle("auto-ok", isOn: harness.autoconfirm)
                    .toggleStyle(.checkbox)
                    .font(.system(size: 11))
                    .help("Press Enter through its startup prompts")
                if harness.wrappedValue.source.isEmpty {
                    Button {
                        harnesses.removeAll { $0.id == harness.wrappedValue.id }
                    } label: {
                        Image(systemName: "xmark.circle.fill")
                            .foregroundColor(.secondary)
                    }
                    .buttonStyle(.borderless)
                    .help("Remove harness")
                }
            }
            TextField("command + flags", text: harness.command)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 12, design: .monospaced))
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.white.opacity(0.12)))
    }

    private var saveBar: some View {
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

    private func load() async {
        identity = DaemonConfig.identity
        do {
            apply(try await RemoteSessionService.shared.fetchSettings())
            status = ""
            loaded = true
        } catch {
            status = "Couldn't reach ccmuxd at \(DaemonConfig.baseURL)"
        }
    }

    /// Persist the app-local developer identity (this Mac's self-declared login,
    /// not a daemon setting); the storage rules live in DaemonConfig next to the
    /// read side. A change re-dials every daemon socket — the identity travels
    /// as a query param, so only a fresh dial presents it.
    private func persistIdentity() {
        if DaemonConfig.setIdentity(identity) {
            RemoteSessionService.shared.reconnectAll()
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
        persistIdentity() // app-local; saved even if the daemon rejects the rest
        // A route pointing at an account that was deleted or renamed in this
        // editing session falls back to direct — sending the stale name would
        // 400 the whole save with a message about none of the visible fields.
        if !llmRoute.isEmpty && !accounts.contains(where: { $0.name == llmRoute }) {
            llmRoute = ""
        }
        let outgoing = rules.map { DaemonHarnessRule(pathPrefix: $0.pathPrefix, harness: $0.harness) }
        let result = await RemoteSessionService.shared.updateSettings(
            devDomain: devDomain, lensHostname: lensHostname,
            cloudflareToken: outgoingSecret(cloudflareToken, wasSet: cloudflareTokenSet),
            tailscaleAuthKey: outgoingSecret(tailscaleAuthKey, wasSet: tailscaleAuthKeySet),
            // Only what the daemon offered: an older daemon silently DROPS
            // unknown llm/harness fields (never a 400), so sending them would
            // fake a save — and this Mac may front several hosts.
            llmRoute: supportsLLM ? llmRoute : nil,
            llmAccounts: supportsLLM ? outgoingAccounts() : nil,
            harnesses: supportsHarnesses ? outgoingHarnesses() : nil,
            harnessRules: supportsHarnessRules ? outgoing : nil)
        guard let saved = result.settings else {
            status = "✗ \(result.error ?? "Couldn't save — is the daemon running?")"
            return
        }
        apply(saved) // show the rules that survived validation
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

    /// A blank editor row isn't an account; empty apiKey keeps the stored key.
    private func outgoingAccounts() -> [[String: Any]] {
        accounts.filter { !$0.name.isEmpty || !$0.baseURL.isEmpty }.map { a in
            [
                "name": a.name, "kind": a.kind, "baseURL": a.baseURL,
                "apiKey": a.apiKey, "modelAliases": Self.parseAliases(a.aliases),
            ]
        }
    }

    /// Only overrides and new entries persist — an untouched builtin/detected
    /// row stays live-resolved on the daemon (same rule as the web editor).
    private func outgoingHarnesses() -> [[String: Any]] {
        harnesses.compactMap { h in
            if h.name.isEmpty && h.command.isEmpty { return nil }
            if h.untouchedDefault { return nil }
            return ["name": h.name, "icon": h.icon, "command": h.command, "autoconfirm": h.autoconfirm]
        }
    }

    /// "from=to, from2=to2" — rows without both sides are dropped as half-typed.
    private static func parseAliases(_ text: String) -> [[String: String]] {
        text.split(separator: ",").compactMap { part in
            let s = part.trimmingCharacters(in: .whitespaces)
            guard let eq = s.firstIndex(of: "=") else { return nil }
            let from = String(s[..<eq]).trimmingCharacters(in: .whitespaces)
            let to = String(s[s.index(after: eq)...]).trimmingCharacters(in: .whitespaces)
            if from.isEmpty || to.isEmpty { return nil }
            return ["from": from, "to": to]
        }
    }

    private func apply(_ settings: DaemonSettings) {
        rules = settings.harnessRules.map { EditableRule(pathPrefix: $0.pathPrefix, harness: $0.harness) }
        supportsLLM = settings.supportsLLM
        supportsHarnesses = settings.supportsHarnesses
        supportsHarnessRules = settings.supportsHarnessRules
        llmRoute = settings.llmRoute
        accountStatus = Dictionary(uniqueKeysWithValues: settings.llmAccountStatus.map { ($0.name, $0) })
        accounts = settings.llmAccounts.map {
            EditableAccount(
                name: $0.name, kind: $0.kind, baseURL: $0.baseURL,
                apiKey: "", apiKeySet: $0.apiKeySet,
                aliases: $0.modelAliases.map { "\($0.from)=\($0.to)" }.joined(separator: ", "))
        }
        harnesses = settings.harnesses.map {
            EditableHarness(
                icon: $0.icon ?? "", name: $0.name, command: $0.command ?? "",
                autoconfirm: $0.autoconfirm, source: $0.source,
                orig: [$0.icon ?? "", $0.name, $0.command ?? "", $0.autoconfirm ? "1" : "0"])
        }
        devDomain = settings.devDomain
        lensHostname = settings.lensHostname
        cloudflareTokenSet = settings.cloudflareTokenSet
        tailscaleAuthKeySet = settings.tailscaleAuthKeySet
        devCertStatus = settings.devCertStatus
        // Stored secrets render as dots (the daemon never echoes them); an
        // untouched sentinel round-trips as "unchanged".
        cloudflareToken = cloudflareTokenSet ? Self.secretSentinel : ""
        tailscaleAuthKey = tailscaleAuthKeySet ? Self.secretSentinel : ""
    }
}

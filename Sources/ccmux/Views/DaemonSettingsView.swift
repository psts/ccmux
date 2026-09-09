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
    @State private var sidecars: [String: DaemonSidecarStatus] = [:]
    @State private var harnesses: [EditableHarness] = []
    /// Base agents from GET /v1/agents; nil = this daemon has no agents support.
    @State private var agents: [DaemonAgent]? = nil
    @State private var agentStatus = ""
    @State private var editingAgent: AgentEditTarget? = nil
    @State private var editingAccount: AccountEditTarget? = nil
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

    /// The account-kind checkboxes, in the same order the web editor renders
    /// them (also the order the save payload serializes).
    static let accountKindOptions = ["anthropic", "openai", "claude", "codex", "meridian"]

    struct EditableHarness: Identifiable {
        let id = UUID()
        var icon: String
        var name: String
        var command: String
        var autoconfirm: Bool
        /// Which llm account kinds this harness may use; empty = its default.
        var kinds: Set<String>
        /// This harness's own account order; empty = follow the account list's
        /// order. An ARRAY, not a Set: the order is the whole content here,
        /// which is why the kinds above can stay a Set and this cannot.
        var order: [String]
        /// Whether the "custom for this harness" radio is on. Held apart from
        /// `order` being non-empty, which is NOT the same thing: with kinds
        /// that match no configured account the custom order resolves to
        /// nothing, and deriving the radio from emptiness would snap it back
        /// with no explanation instead of showing why the list is empty.
        /// Not in Snapshot: it is which control is lit, not stored state.
        var customOrder: Bool
        let source: String
        /// The daemon-resolved values this row started as: an untouched
        /// builtin/detected row is NOT persisted, so it stays live-resolved.
        /// A struct, not a positional list, so adding a field to the row
        /// extends the dirty check by construction. nil for added rows.
        let orig: Snapshot?

        struct Snapshot: Equatable {
            var icon: String
            var name: String
            var command: String
            var autoconfirm: Bool
            var kinds: Set<String>
            var order: [String]
        }

        var snapshot: Snapshot {
            Snapshot(icon: icon, name: name, command: command, autoconfirm: autoconfirm, kinds: kinds, order: order)
        }

        var untouchedDefault: Bool {
            !source.isEmpty && snapshot == orig
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
                if agents != nil { agentsTab.tabItem { Text("Agents") } }
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

    /// Which harness a new workspace under a folder PRESELECTS (the web
    /// lens's harness bar highlights it; nothing ever auto-starts).
    /// A rule may name a harness deleted in this session — the picker
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
                Text("Default account (panes ccmux did not start)")
                    .font(.headline)
                Text("Which account answers a pane with no harness of its own — a plain shell, a tool you started by hand. A pane running a harness follows that harness's accounts instead. Applies to each pane's next request — no restarts.")
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
                Text("Accounts (top first)")
                    .font(.headline)
                    .padding(.top, 6)
                Text("This order is the failover order: a harness tries these top-down, skipping the kinds it cannot use, and moves to the next when one hits its limit. A limited account drops to the back on its own. Claude accounts hold a token from `claude setup-token`; usage updates from live traffic.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                // Indexed so the row can show its position in the failover
                // order. Identity is the index, so a reorder redraws both
                // rows — which is what should happen when they swap.
                ForEach(accounts.indices, id: \.self) { index in
                    accountRow(index, accounts[index])
                }
                Button("Add account") { editingAccount = AccountEditTarget(index: accounts.count) }
                    .controlSize(.small)
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .sheet(item: $editingAccount) { target in
            AccountEditorView(
                account: target.index < accounts.count ? accounts[target.index] : nil,
                statusText: target.index < accounts.count
                    ? [accountStatusText(accounts[target.index].name), sidecarText(accounts[target.index])]
                        .compactMap { $0 }.joined(separator: " · ")
                    : ""
            ) { edited in
                editingAccount = nil
                guard let edited else { return } // cancelled
                if target.index < accounts.count {
                    accounts[target.index] = edited
                } else {
                    accounts.append(edited)
                }
            }
        }
    }

    /// The sidecar footer of a meridian account: the daemon runs one Meridian
    /// process per such account, so "is it running" is the status that matters.
    private func sidecarText(_ account: EditableAccount) -> String? {
        guard account.kind == "meridian" else { return nil }
        guard let sc = sidecars[account.name] else { return "⚙ sidecar not supervised by this daemon" }
        if sc.running {
            return "⚙ sidecar running (pid \(sc.pid)" + (sc.restarts > 0 ? ", \(sc.restarts) restarts)" : ")")
        }
        return "⚙ sidecar stopped" + (sc.lastError.isEmpty ? "" : ": \(sc.lastError)")
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

    /// One account in the list: what it is, how it is doing, and where it
    /// sits in the failover order. Everything you SET rather than read is in
    /// the editor sheet — this tab is read far more often than it is edited,
    /// and the order arrows are what you reach for.
    private func accountRow(_ index: Int, _ account: EditableAccount) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 8) {
                Text("\(index + 1)")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .frame(width: 14, alignment: .trailing)
                Text(account.name.isEmpty ? "(unnamed)" : account.name)
                    .font(.system(size: 12, weight: .semibold))
                Text(account.kind).font(.system(size: 11)).foregroundColor(.secondary)
                Spacer()
                Button {
                    moveAccount(account.id, by: -1)
                } label: {
                    Image(systemName: "chevron.up").foregroundColor(.secondary)
                }
                .buttonStyle(.borderless)
                .help("Try this account earlier")
                Button {
                    moveAccount(account.id, by: 1)
                } label: {
                    Image(systemName: "chevron.down").foregroundColor(.secondary)
                }
                .buttonStyle(.borderless)
                .help("Try this account later")
                Button("Edit") { editingAccount = AccountEditTarget(index: index) }
                    .controlSize(.small)
                Button {
                    removeAccount(account)
                } label: {
                    Image(systemName: "xmark.circle.fill").foregroundColor(.secondary)
                }
                .buttonStyle(.borderless)
                .help("Remove account")
            }
            if let status = accountStatusText(account.name) {
                Text(status).font(.system(size: 11)).foregroundColor(.secondary)
            }
            if let sidecar = sidecarText(account) {
                Text(sidecar).font(.system(size: 11)).foregroundColor(.secondary)
            }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.white.opacity(0.12)))
    }

    /// Base agents: a list, one row each; New and Edit open the editor sheet
    /// (AgentEditorView), where the whole base lives — role, harness,
    /// permissions, skills, servers, plugins, lifecycle and deployments.
    private var agentsTab: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 8) {
                Text("Role agents live as folders under ~/.ccmux/agents. Edit one to set its role, skills and tools; add it to a project from the window's ⚙ menu.")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                ForEach(Array((agents ?? []).enumerated()), id: \.offset) { idx, a in
                    agentRow(idx, a)
                }
                Button("New agent") { editingAgent = AgentEditTarget(agent: nil) }
                    .controlSize(.small)
                Text(agentStatus)
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .sheet(item: $editingAgent) { target in
            AgentEditorView(agent: target.agent, harnesses: harnesses.map(\.name), accounts: accounts.map(\.name)) {
                editingAgent = nil
                Task { await reloadAgents() }
            }
        }
    }

    private func agentRow(_ idx: Int, _ a: DaemonAgent) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 8) {
                // An icon-less base keeps the cell (so names line up) but
                // shows nothing in it: no stand-in glyph.
                Text(a.icon).frame(width: 22)
                Text(a.name).font(.system(size: 12, weight: .semibold))
                Text("v\(a.version)").font(.system(size: 11)).foregroundColor(.secondary)
                Text(a.harness).font(.system(size: 11)).foregroundColor(.secondary)
                Spacer()
                Button("Edit") { editingAgent = AgentEditTarget(agent: a) }.controlSize(.small)
                Button("Delete…", role: .destructive) { deleteAgent(idx) }.controlSize(.small)
            }
            Text(a.description).font(.system(size: 11)).foregroundColor(.secondary)
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.white.opacity(0.12)))
    }

    private func reloadAgents() async {
        let res = await RemoteSessionService.shared.fetchAgents()
        if let err = res.error { agentStatus = "Couldn't reload agents: \(err)"; return }
        agents = res.list
    }

    /// Bounds-checked row lookup: SwiftUI can hand a stale index after the
    /// array shrinks, and `agents?[idx]` only guards nil, not the range.
    private func agentAt(_ idx: Int) -> DaemonAgent? {
        guard let list = agents, idx >= 0, idx < list.count else { return nil }
        return list[idx]
    }

    private func deleteAgent(_ idx: Int) {
        guard let a = agentAt(idx) else { return }
        let alert = NSAlert()
        alert.messageText = "Delete agent “\(a.name)”?"
        alert.informativeText = "Removes its folder, skills included. Project instance folders stay."
        alert.alertStyle = .warning
        alert.addButton(withTitle: "Delete")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        Task {
            if let error = await RemoteSessionService.shared.deleteAgent(a.name) {
                agentStatus = "✗ \(a.name): \(error)"
                return
            }
            // By name, not the index captured before the await.
            if let at = agents?.firstIndex(where: { $0.name == a.name }) { agents?.remove(at: at) }
            agentStatus = "Deleted \(a.name)."
        }
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
                        icon: "", name: "", command: "", autoconfirm: false, kinds: [], order: [],
                        customOrder: false, source: "", orig: nil))
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
            // Which llm account kinds this harness can use — same row as the
            // web editor; none checked = the harness's default pairing.
            HStack(spacing: 8) {
                Text("accounts:")
                    .font(.system(size: 10))
                    .foregroundColor(.secondary)
                ForEach(Self.accountKindOptions, id: \.self) { kind in
                    Toggle(kind, isOn: kindBinding(harness, kind))
                        .toggleStyle(.checkbox)
                        .font(.system(size: 11))
                }
            }
            .help("Which llm account kinds this harness can use; none checked = its default")
            if let warning = dialectWarning(harness.wrappedValue.kinds) {
                Text(warning)
                    .font(.system(size: 11))
                    .foregroundColor(.orange)
                    .fixedSize(horizontal: false, vertical: true)
            }
            harnessOrderSection(harness)
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.white.opacity(0.12)))
    }

    /// A codex account speaks OpenAI's Responses API; every other kind speaks
    /// Anthropic's Messages API. Checking both is a warning and not a
    /// refusal: an unknown harness may genuinely need the override, and the
    /// daemon holds each pane's failover order to ONE dialect regardless.
    private func dialectWarning(_ kinds: Set<String>) -> String? {
        guard kinds.contains("codex"), kinds.contains(where: { $0 != "codex" }) else { return nil }
        return "codex speaks a different API than the other kinds — a pane will use whichever one answers first and ignore the rest"
    }

    /// The per-harness account order: off by default (the harness follows the
    /// account list), on shows the accounts its kinds allow, in the order it
    /// will try them.
    @ViewBuilder
    private func harnessOrderSection(_ harness: Binding<EditableHarness>) -> some View {
        let custom = harness.wrappedValue.customOrder
        HStack(spacing: 8) {
            Text("order:")
                .font(.system(size: 10))
                .foregroundColor(.secondary)
            Picker("", selection: orderModeBinding(harness)) {
                Text("follow the account order").tag(false)
                Text("custom for this harness").tag(true)
            }
            .pickerStyle(.radioGroup)
            .labelsHidden()
        }
        if custom {
            let ordered = orderedAccounts(harness.wrappedValue)
            if ordered.isEmpty {
                Text("no account matches the kinds above")
                    .font(.system(size: 11))
                    .foregroundColor(.secondary)
            } else {
                // Indices rather than `enumerated()`: the row needs both the
                // position and the name, and indexing reads plainer than
                // destructuring a tuple. (`id: \.element` would compile —
                // the same shape ships in PaneTabBar and SidebarView.)
                ForEach(ordered.indices, id: \.self) { i in
                    HStack(spacing: 6) {
                        Text("\(i + 1). \(ordered[i])")
                            .font(.system(size: 11, design: .monospaced))
                        Spacer()
                        Button {
                            moveHarnessAccount(harness, ordered, i, by: -1)
                        } label: {
                            Image(systemName: "chevron.up").foregroundColor(.secondary)
                        }
                        .buttonStyle(.borderless)
                        .help("Try this account earlier")
                        Button {
                            moveHarnessAccount(harness, ordered, i, by: 1)
                        } label: {
                            Image(systemName: "chevron.down").foregroundColor(.secondary)
                        }
                        .buttonStyle(.borderless)
                        .help("Try this account later")
                    }
                }
            }
        }
    }

    /// Turning the custom order ON seeds it with what the harness resolves to
    /// today, so the choice freezes the current list rather than starting
    /// from nothing. Turning it OFF clears it, which is what makes the
    /// account list's order reach this harness again.
    ///
    /// The radio's own state is stored, not derived from the order being
    /// non-empty: a harness whose kinds match no account seeds an EMPTY
    /// order, and deriving would flip the radio back with no explanation
    /// instead of showing "no account matches the kinds above". An empty
    /// custom order still persists as no override, because it resolves
    /// identically — the same rule the web lens follows.
    private func orderModeBinding(_ harness: Binding<EditableHarness>) -> Binding<Bool> {
        Binding(
            get: { harness.wrappedValue.customOrder },
            set: { on in
                harness.wrappedValue.customOrder = on
                harness.wrappedValue.order = on ? orderedAccounts(harness.wrappedValue) : []
            })
    }

    /// The accounts this harness may use, in the order it would try them: the
    /// ones it named first, then the rest as configured. Mirrors the daemon's
    /// own resolution — `subscriptionFirst` then `inOrder` — including
    /// dropping a named account that no longer exists.
    ///
    /// The subscriptionFirst half is not optional. Without it this preview
    /// showed "1. keyed, 2. sidecar" for opencode while the daemon tried the
    /// sidecar first, and because turning "custom" on SEEDS the stored order
    /// from this list, accepting what the editor showed silently demoted the
    /// subscription and moved spend onto a metered key.
    private func orderedAccounts(_ harness: EditableHarness) -> [String] {
        let allowed = Self.subscriptionFirst(
            accounts.filter { !$0.name.isEmpty && Self.kindAllowed(harness.kinds, $0.kind) },
            harness.kinds)
        let named = harness.order.filter(allowed.contains)
        return named + allowed.filter { !named.contains($0) }
    }

    /// Mirrors llmproxy.subscriptionFirst: a meridian account leads for a
    /// harness that declared meridian, because that sidecar spends a Claude
    /// subscription rather than a metered key.
    private static func subscriptionFirst(_ allowed: [EditableAccount], _ kinds: Set<String>) -> [String] {
        let names = allowed.map(\.name)
        guard kinds.contains("meridian") else { return names }
        return allowed.filter { $0.kind == "meridian" }.map(\.name)
            + allowed.filter { $0.kind != "meridian" }.map(\.name)
    }

    /// kindAllowed, mirrored from the daemon (llmproxy.KindAllowed): no kinds
    /// checked means any kind except the two a harness has to ask for.
    private static func kindAllowed(_ kinds: Set<String>, _ kind: String) -> Bool {
        if kinds.isEmpty { return kind != "codex" && kind != "meridian" }
        return kinds.contains(kind)
    }

    /// Swaps two VISIBLE rows inside the harness's STORED order.
    ///
    /// It works on the stored array rather than replacing it with `ordered`,
    /// because `ordered` is kind-filtered: assigning it would throw away the
    /// accounts an unchecked kind has parked, so unchecking a kind, nudging a
    /// visible row and rechecking would move a parked account from first to
    /// last and silently change which account the harness tries first. Same
    /// rule as the web lens's moveInOrder.
    private func moveHarnessAccount(
        _ harness: Binding<EditableHarness>, _ ordered: [String], _ i: Int, by delta: Int
    ) {
        let j = i + delta
        guard j >= 0 && j < ordered.count else { return }
        // Seed any visible account the stored order does not name yet — one
        // added since "custom" was turned on — so both sides of the swap are
        // findable. The web lens seeds the same way before it paints.
        var stored = harness.wrappedValue.order
        for name in ordered where !stored.contains(name) {
            stored.append(name)
        }
        guard let a = stored.firstIndex(of: ordered[i]),
            let b = stored.firstIndex(of: ordered[j])
        else { return }
        stored.swapAt(a, b)
        harness.wrappedValue.order = stored
    }

    /// Moves one account in the list, which IS the stored failover order —
    /// the daemon reads the array's order, so there is no separate weight to
    /// keep in step. A move off either end is a no-op rather than a wrap.
    /// Confirms first, the way deleting an agent does: the ✕ sits beside the
    /// order arrows, and the next Save takes the account's stored credential
    /// with it. Same prompt the web lens puts up.
    private func removeAccount(_ account: EditableAccount) {
        let alert = NSAlert()
        alert.messageText = "Remove account “\(account.name)”?"
        alert.informativeText = "Its stored key goes with it. Panes routed at it fall back to the default account."
        alert.alertStyle = .warning
        alert.addButton(withTitle: "Remove")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        if llmRoute == account.name { llmRoute = "" }
        accounts.removeAll { $0.id == account.id }
    }

    private func moveAccount(_ id: UUID, by delta: Int) {
        guard let i = accounts.firstIndex(where: { $0.id == id }) else { return }
        let j = i + delta
        guard j >= 0 && j < accounts.count else { return }
        accounts.swapAt(i, j)
    }

    private func kindBinding(_ harness: Binding<EditableHarness>, _ kind: String) -> Binding<Bool> {
        Binding(
            get: { harness.wrappedValue.kinds.contains(kind) },
            set: { on in
                if on {
                    harness.wrappedValue.kinds.insert(kind)
                } else {
                    harness.wrappedValue.kinds.remove(kind)
                }
            })
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
            let fetched = await RemoteSessionService.shared.fetchAgents()
            agents = fetched.supported ? fetched.list : nil
            if fetched.supported && fetched.error != nil { agentStatus = "✗ Couldn't load agents: \(fetched.error!)" }
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
    /// accountKinds only travel when at least one is checked; empty inherits
    /// the harness's default pairing on the daemon.
    private func outgoingHarnesses() -> [[String: Any]] {
        harnesses.compactMap { h in
            if h.name.isEmpty && h.command.isEmpty { return nil }
            if h.untouchedDefault { return nil }
            var out: [String: Any] = ["name": h.name, "icon": h.icon, "command": h.command, "autoconfirm": h.autoconfirm]
            let kinds = Self.accountKindOptions.filter(h.kinds.contains)
            if !kinds.isEmpty { out["accountKinds"] = kinds }
            // Only a custom order travels: its absence is what keeps the
            // account list's own order reaching this harness as accounts are
            // added and moved. Sending [] would pin an empty override.
            if !h.order.isEmpty { out["accountOrder"] = h.order }
            return out
        }
    }

    /// "from=to, from2=to2" — rows without both sides are dropped as half-typed.
    static func parseAliases(_ text: String) -> [[String: String]] {
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
        sidecars = settings.llmSidecars
        accounts = settings.llmAccounts.map {
            EditableAccount(
                name: $0.name, kind: $0.kind, baseURL: $0.baseURL,
                apiKey: "", apiKeySet: $0.apiKeySet,
                aliases: $0.modelAliases.map { "\($0.from)=\($0.to)" }.joined(separator: ", "))
        }
        // Which rows had the custom-order radio on. apply() runs after every
        // save, and rebuilding customOrder from order.isEmpty is exactly the
        // derivation this field exists to avoid: a harness whose kinds match
        // no account stores an empty order, so the radio snapped back the
        // moment the user saved.
        let wasCustom = Set(harnesses.filter(\.customOrder).map(\.name))
        harnesses = settings.harnesses.map { h in
            let snap = EditableHarness.Snapshot(
                icon: h.icon ?? "", name: h.name, command: h.command ?? "",
                autoconfirm: h.autoconfirm, kinds: Set(h.accountKinds), order: h.accountOrder)
            return EditableHarness(
                icon: snap.icon, name: snap.name, command: snap.command,
                autoconfirm: snap.autoconfirm, kinds: snap.kinds, order: snap.order,
                customOrder: !snap.order.isEmpty || wasCustom.contains(h.name),
                source: h.source, orig: snap)
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

/// Which account row the editor sheet is open on. An index one past the
/// end means "new": the sheet hands back an account the list then appends.
struct AccountEditTarget: Identifiable {
    let index: Int
    var id: Int { index }
}

/// What the agents tab is editing: an existing base, or nil for a new one.
/// Identifiable so `.sheet(item:)` can present it.
struct AgentEditTarget: Identifiable {
    let agent: DaemonAgent?
    var id: String { agent?.name ?? "new" }
}

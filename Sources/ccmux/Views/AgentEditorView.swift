import AppKit
import SwiftUI
import UniformTypeIdentifiers

/// The agent editor: one sheet for a whole base, mirroring the web lens's
/// agentmodal.js — identity, role, harness and model, permissions, skills
/// (from a git folder URL or dropped files), MCP servers (from a pasted
/// snippet), opencode plugins, lifecycle, and where it is deployed with
/// "Restart to apply". Save is one PUT; skills, servers and restart act at
/// once against the daemon.
struct AgentEditorView: View {
    let harnesses: [String]
    let accounts: [String]
    let onDone: () -> Void

    @State private var agent: DaemonAgent
    @State private var isNew: Bool
    @State private var bashAllowText: String
    @State private var pluginsText: String
    @State private var status = ""
    @State private var skills: [DaemonSkill] = []
    @State private var skillURL = ""
    @State private var skillsStatus = ""
    @State private var servers: [DaemonMCPServer] = []
    @State private var mcpSnippet = ""
    @State private var mcpStatus = ""
    @State private var deployments: [DaemonAgentDeployment] = []
    @State private var deployStatus = ""
    @State private var scheduleCounts: [String: Int] = [:]
    @State private var dropTargeted = false

    private let service = RemoteSessionService.shared

    init(agent: DaemonAgent?, harnesses: [String], accounts: [String], onDone: @escaping () -> Void) {
        self.harnesses = harnesses
        self.accounts = accounts
        self.onDone = onDone
        let a = agent ?? DaemonAgent(name: "")
        _agent = State(initialValue: a)
        _isNew = State(initialValue: agent == nil)
        _bashAllowText = State(initialValue: a.permissions.bashAllow.joined(separator: "\n"))
        _pluginsText = State(initialValue: a.plugins.joined(separator: "\n"))
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Text(isNew ? "New agent" : "⚙ \(agent.name)").font(.system(size: 13, weight: .semibold))
                if !agent.version.isEmpty { Text("v\(agent.version)").font(.system(size: 11)).foregroundColor(.secondary) }
                Spacer()
                Button("Close", action: onDone).controlSize(.small)
            }
            .padding(12)
            Divider()
            ScrollView {
                VStack(alignment: .leading, spacing: 10) {
                    identity
                    role
                    harnessAndModel
                    permissions
                    skillsSection
                    mcpSection
                    pluginsSection
                    lifecycle
                    instancesSection
                }
                .padding(12)
            }
            Divider()
            HStack {
                Text(status).font(.system(size: 11)).foregroundColor(.secondary).lineLimit(2)
                Spacer()
                Button("Save") { Task { await save() } }
                    .keyboardShortcut(.defaultAction)
                    .disabled(agent.name.isEmpty || agent.description.isEmpty)
            }
            .padding(12)
        }
        .frame(minWidth: 560, idealWidth: 640, minHeight: 520, idealHeight: 760)
        .textFieldStyle(.roundedBorder)
        .font(.system(size: 12))
        .task { if !isNew { await loadAll() } }
    }

    // MARK: - Sections

    private func section<Content: View>(_ title: String, @ViewBuilder content: () -> Content) -> some View {
        GroupBox(label: Text(title).font(.system(size: 12, weight: .semibold))) {
            VStack(alignment: .leading, spacing: 6, content: content)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(4)
        }
    }

    private var identity: some View {
        section("Identity") {
            HStack(spacing: 6) {
                TextField("⚙", text: $agent.icon).frame(width: 40)
                TextField("name (a-z, 0-9, -)", text: $agent.name).disabled(!isNew)
            }
            TextField("One sentence: what it does and when to call it. Other agents read this to decide.", text: $agent.description)
        }
    }

    private var role: some View {
        section("Role") {
            TextEditor(text: $agent.instructions)
                .font(.system(size: 12, design: .monospaced))
                .frame(minHeight: 140)
                .border(Color.white.opacity(0.12))
        }
    }

    private var harnessAndModel: some View {
        section("Harness and model") {
            HStack(spacing: 6) {
                Text("harness").foregroundColor(.secondary)
                Picker("", selection: $agent.harness) {
                    Text("default harness").tag("")
                    ForEach(harnesses, id: \.self) { Text($0).tag($0) }
                    if !agent.harness.isEmpty, !harnesses.contains(agent.harness) { Text(agent.harness).tag(agent.harness) }
                }
                .labelsHidden().frame(width: 140)
                Text("account").foregroundColor(.secondary)
                Picker("", selection: $agent.account) {
                    Text("default").tag("")
                    ForEach(accounts, id: \.self) { Text($0).tag($0) }
                    if !agent.account.isEmpty, !accounts.contains(agent.account) { Text(agent.account).tag(agent.account) }
                }
                .labelsHidden()
            }
            HStack(spacing: 6) {
                Text("model").foregroundColor(.secondary)
                TextField("default for the harness, e.g. anthropic/claude-sonnet-5", text: $agent.model)
            }
        }
    }

    private var permissions: some View {
        section("Permissions") {
            HStack(spacing: 8) {
                permissionPicker("read", $agent.permissions.read)
                permissionPicker("edit", $agent.permissions.edit)
                permissionPicker("bash", $agent.permissions.bash)
                permissionPicker("webfetch", $agent.permissions.webfetch)
            }
            Text("bash patterns always allowed, one per line, e.g. git log *").font(.system(size: 11)).foregroundColor(.secondary)
            TextEditor(text: $bashAllowText).font(.system(size: 12, design: .monospaced)).frame(minHeight: 44).border(Color.white.opacity(0.12))
        }
    }

    private func permissionPicker(_ label: String, _ value: Binding<String>) -> some View {
        HStack(spacing: 4) {
            Text(label).foregroundColor(.secondary)
            Picker("", selection: value) {
                ForEach(["allow", "ask", "deny"], id: \.self) { Text($0).tag($0) }
            }
            .labelsHidden().frame(width: 80)
        }
    }

    private var skillsSection: some View {
        section("Skills") {
            if isNew {
                Text("Save the agent first, then add skills.").font(.system(size: 11)).foregroundColor(.secondary)
            } else {
                if skills.isEmpty { Text("No skills yet.").font(.system(size: 11)).foregroundColor(.secondary) }
                ForEach(skills) { sk in
                    HStack(alignment: .top, spacing: 8) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(sk.name).font(.system(size: 12, weight: .semibold))
                            Text(sk.description).font(.system(size: 11)).foregroundColor(.secondary)
                            if let src = sk.source, !src.isEmpty { Text(src).font(.system(size: 10)).foregroundColor(.secondary).lineLimit(1) }
                        }
                        Spacer()
                        if let src = sk.source, !src.isEmpty {
                            Button("Update") { Task { await run(skillsStatus: "Updated \(sk.name).") { await service.updateSkill(agent: agent.name, skill: sk.name) } } }.controlSize(.small)
                        }
                        Button("Remove") {
                            guard confirmRemoval("skill", sk.name) else { return }
                            Task { await run(skillsStatus: "Removed \(sk.name).") { await service.deleteSkill(agent: agent.name, skill: sk.name) } }
                        }.controlSize(.small)
                    }
                }
                HStack(spacing: 6) {
                    TextField("https://github.com/owner/repo/tree/main/skills/name", text: $skillURL)
                    Button("Add from URL") { Task { await addSkillURL() } }.controlSize(.small).disabled(skillURL.isEmpty)
                }
                Text("Or drop a SKILL.md or a skill folder here")
                    .font(.system(size: 11)).foregroundColor(dropTargeted ? .primary : .secondary)
                    .frame(maxWidth: .infinity).padding(10)
                    .overlay(RoundedRectangle(cornerRadius: 6).stroke(dropTargeted ? Color.green : Color.secondary.opacity(0.4), style: StrokeStyle(lineWidth: 1, dash: [4])))
                    .onDrop(of: [.fileURL], isTargeted: $dropTargeted) { providers in
                        Task { await dropped(providers) }
                        return true
                    }
                Text(skillsStatus).font(.system(size: 11)).foregroundColor(.secondary)
            }
        }
    }

    private var mcpSection: some View {
        section("MCP servers") {
            if isNew {
                Text("Save the agent first.").font(.system(size: 11)).foregroundColor(.secondary)
            } else {
                if servers.isEmpty { Text("No servers yet. The peers bus is always there.").font(.system(size: 11)).foregroundColor(.secondary) }
                ForEach(servers) { sv in
                    HStack(alignment: .top, spacing: 8) {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(sv.name).font(.system(size: 12, weight: .semibold))
                            Text(([sv.command] + (sv.args ?? [])).joined(separator: " ")).font(.system(size: 11)).foregroundColor(.secondary)
                            if let env = sv.env, !env.isEmpty { Text("env: " + env.keys.sorted().joined(separator: ", ")).font(.system(size: 10)).foregroundColor(.secondary) }
                        }
                        Spacer()
                        Button("Remove") {
                            guard confirmRemoval("MCP server", sv.name) else { return }
                            Task { await run(mcpStatus: "Removed \(sv.name).") { await service.deleteMCP(agent: agent.name, server: sv.name) } }
                        }.controlSize(.small)
                    }
                }
                TextEditor(text: $mcpSnippet).font(.system(size: 11, design: .monospaced)).frame(minHeight: 60).border(Color.white.opacity(0.12))
                HStack {
                    Button("Add servers") { Task { await addMCP() } }.controlSize(.small).disabled(mcpSnippet.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                    Text(mcpStatus.isEmpty ? "Paste the snippet from the server's readme: {\"mcpServers\": {...}}" : mcpStatus).font(.system(size: 11)).foregroundColor(.secondary)
                }
            }
        }
    }

    private var pluginsSection: some View {
        section("opencode plugins") {
            Text("plugin packages, one per line").font(.system(size: 11)).foregroundColor(.secondary)
            TextEditor(text: $pluginsText).font(.system(size: 12, design: .monospaced)).frame(minHeight: 44).border(Color.white.opacity(0.12))
        }
    }

    private var lifecycle: some View {
        section("Lifecycle") {
            HStack(spacing: 8) {
                Text("start").foregroundColor(.secondary)
                Picker("", selection: $agent.start) {
                    Text("fresh").tag("fresh")
                    Text("continue").tag("continue")
                }
                .labelsHidden().frame(width: 100)
                Text("idle").foregroundColor(.secondary)
                TextField("10", value: $agent.idleExitMinutes, format: .number).frame(width: 50)
                Text("min, 0 = never sleeps").font(.system(size: 11)).foregroundColor(.secondary)
                Toggle("keep alive", isOn: $agent.keepAlive).toggleStyle(.checkbox)
            }
        }
    }

    private var instancesSection: some View {
        section("Instances") {
            if isNew {
                Text("Not deployed yet. Add it to a window from the window's ⚙ menu.").font(.system(size: 11)).foregroundColor(.secondary)
            } else {
                if deployments.isEmpty { Text("Not deployed to any window yet. Add it from a window's ⚙ menu.").font(.system(size: 11)).foregroundColor(.secondary) }
                ForEach(deployments) { d in
                    HStack {
                        Text("\(d.window) · \(d.state)").font(.system(size: 12))
                        Spacer()
                        if let v = d.paneVersion, !v.isEmpty {
                            Text("runs v\(v)" + ((d.drift ?? false) ? " ↻ older than the base" : "")).font(.system(size: 11)).foregroundColor(.secondary)
                        }
                        if let n = scheduleCounts[d.windowId], n > 0 {
                            Text("· \(n) schedule\(n == 1 ? "" : "s")").font(.system(size: 11)).foregroundColor(.secondary)
                        }
                    }
                }
                HStack {
                    Button("Restart to apply") { Task { await restart() } }.controlSize(.small)
                        .disabled(!deployments.contains { $0.state == "running" })
                    Text(deployStatus).font(.system(size: 11)).foregroundColor(.secondary)
                }
            }
        }
    }

    // MARK: - Actions

    private func save() async {
        agent.permissions.bashAllow = lines(bashAllowText)
        agent.plugins = lines(pluginsText)
        if let error = await service.putAgent(agent) { status = "Not saved: \(error)"; return }
        let wasNew = isNew
        isNew = false
        if let err = await refreshVersion() {
            status = "Saved \(agent.name), but the version could not be read back: \(err)"
        } else {
            status = "Saved v\(agent.version)." + (wasNew ? " You can add skills and servers now." : "")
        }
        await loadAll()
    }

    /// The same confirmation the web lens asks before a destructive removal.
    private func confirmRemoval(_ kind: String, _ name: String) -> Bool {
        let alert = NSAlert()
        alert.messageText = "Remove \(kind) “\(name)”?"
        alert.informativeText = "This deletes it from the agent's folder and bumps the base."
        alert.alertStyle = .warning
        alert.addButton(withTitle: "Remove")
        alert.addButton(withTitle: "Cancel")
        return alert.runModal() == .alertFirstButtonReturn
    }

    private func lines(_ text: String) -> [String] {
        text.split(separator: "\n").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
    }

    private func loadAll() async {
        async let s = service.fetchSkills(agent: agent.name)
        async let m = service.fetchMCP(agent: agent.name)
        async let d = service.fetchDeployments(agent: agent.name)
        let (sk, se, de) = await (s, m, d)
        skills = sk.0; if let e = sk.1 { skillsStatus = e }
        servers = se.0; if let e = se.1 { mcpStatus = e }
        deployments = de.0; if let e = de.1 { deployStatus = e }
        var counts: [String: Int] = [:]
        for d in deployments {
            counts[d.windowId] = await service.fetchSchedules(windowId: d.windowId, agent: agent.name).0.count
        }
        scheduleCounts = counts
    }

    /// Re-reads the base's version after a change; the error text when the
    /// list could not be fetched.
    @discardableResult
    private func refreshVersion() async -> String? {
        let res = await service.fetchAgents()
        if let err = res.error { return err }
        if let fresh = res.list.first(where: { $0.name == agent.name }) { agent.version = fresh.version }
        return nil
    }

    /// run does one skill or server change and reloads; the status line
    /// says what happened either way.
    private func run(skillsStatus ok: String? = nil, mcpStatus okMCP: String? = nil, _ call: () async -> String?) async {
        if let error = await call() {
            if ok != nil { skillsStatus = error } else { mcpStatus = error }
            return
        }
        if let ok { skillsStatus = ok }
        if let okMCP { mcpStatus = okMCP }
        await loadAll()
        await refreshVersion()
    }

    private func addSkillURL() async {
        skillsStatus = "Fetching…"
        let url = skillURL
        await run(skillsStatus: "Installed.") { await service.addSkill(agent: agent.name, url: url) }
        if skillsStatus == "Installed." { skillURL = "" }
    }

    private func addMCP() async {
        let snippet = mcpSnippet
        await run(mcpStatus: "Added.") { await service.addMCP(agent: agent.name, snippet: snippet) }
        if mcpStatus == "Added." { mcpSnippet = "" }
    }

    private func restart() async {
        if let error = await service.restartInstances(agent: agent.name) { deployStatus = error; return }
        deployStatus = "Restarting running instances; they pick up the new base as they come back."
        try? await Task.sleep(nanoseconds: 3_000_000_000)
        await loadAll()
    }

    /// dropped reads file URLs from the drop: a folder is walked (text files
    /// only), single files go in by name; SKILL.md must be among them.
    private func dropped(_ providers: [NSItemProvider]) async {
        var files: [String: String] = [:]
        var folder = ""
        for p in providers {
            guard let url = await fileURL(from: p) else { continue }
            var isDir: ObjCBool = false
            FileManager.default.fileExists(atPath: url.path, isDirectory: &isDir)
            if isDir.boolValue {
                folder = folder.isEmpty ? url.lastPathComponent : folder
                walk(url, prefix: "", into: &files)
            } else if let text = try? String(contentsOf: url, encoding: .utf8) {
                files[url.lastPathComponent] = text
            }
        }
        guard files["SKILL.md"] != nil else { skillsStatus = "That has no SKILL.md."; return }
        let name = folder.isEmpty ? "skill" : folder
        await run(skillsStatus: "Installed.") { await service.addSkill(agent: agent.name, name: name, files: files) }
    }

    private func fileURL(from provider: NSItemProvider) async -> URL? {
        await withCheckedContinuation { cont in
            provider.loadItem(forTypeIdentifier: UTType.fileURL.identifier, options: nil) { item, _ in
                if let data = item as? Data, let url = URL(dataRepresentation: data, relativeTo: nil) { cont.resume(returning: url) }
                else if let url = item as? URL { cont.resume(returning: url) }
                else { cont.resume(returning: nil) }
            }
        }
    }

    private func walk(_ dir: URL, prefix: String, into files: inout [String: String]) {
        guard let items = try? FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: [.isDirectoryKey]) else { return }
        for item in items {
            let isDir = (try? item.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) ?? false
            if isDir {
                walk(item, prefix: prefix + item.lastPathComponent + "/", into: &files)
            } else if let text = try? String(contentsOf: item, encoding: .utf8) {
                files[prefix + item.lastPathComponent] = text
            }
        }
    }
}

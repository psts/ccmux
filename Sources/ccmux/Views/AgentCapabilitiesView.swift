import AppKit
import SwiftUI

/// The agent editor's capabilities section, mirroring the web lens's
/// agentcaps.js: whether a claude agent is isolated (loads only its base
/// and the catalog entries ticked here, none of the host user's Claude
/// setup), which catalog entries it gets, which skills are switched off,
/// and the tool rules. The fields live on the editor's draft and are saved
/// with its one PUT; the catalog itself changes at once.
struct AgentCapabilitiesView: View {
    @Binding var agent: DaemonAgent
    @Binding var toolAllowText: String
    @Binding var toolDenyText: String
    /// The base's own skills, as <agent>:<skill>.
    let baseSkills: [String]

    @State private var catalog: [DaemonCapability] = []
    @State private var installed: [String] = []
    @State private var status = ""
    @State private var newID = ""
    @State private var newKind = "plugin"
    @State private var newPlugin = ""
    @State private var newPath = ""
    @State private var newServer = ""

    private let service = RemoteSessionService.shared

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Toggle("isolated: load only what is ticked here, not this host's Claude setup (claude harness; applies at the next wake)", isOn: $agent.isolated)
                .toggleStyle(.checkbox)
            if catalog.isEmpty {
                hint("The catalog is empty. Add an entry below, or import what this host's agents use today.")
            }
            ForEach(catalog) { c in catalogRow(c) }
            hint("skills (untick to switch one off)")
            skillToggles
            hint("tools that run without a card, one rule per line")
            ruleEditor($toolAllowText)
            hint("tools that never run")
            ruleEditor($toolDenyText)
            addForm
            if !status.isEmpty { hint(status) }
        }
        .task { await load() }
    }

    private func hint(_ text: String) -> some View {
        Text(text).font(.system(size: 11)).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
    }

    private func ruleEditor(_ text: Binding<String>) -> some View {
        TextEditor(text: text).font(.system(size: 12, design: .monospaced)).frame(minHeight: 44).border(Color.white.opacity(0.12))
    }

    private func catalogRow(_ c: DaemonCapability) -> some View {
        HStack(alignment: .top, spacing: 8) {
            Toggle("", isOn: picked(c.id)).toggleStyle(.checkbox).labelsHidden()
            VStack(alignment: .leading, spacing: 2) {
                Text(c.id).font(.system(size: 12, weight: .semibold))
                Text(c.summary).font(.system(size: 11)).foregroundColor(.secondary).lineLimit(1)
                if let fx = c.sideEffects, !fx.isEmpty {
                    Text("side effects: " + fx.joined(separator: ", ")).font(.system(size: 10)).foregroundColor(.secondary)
                }
            }
            Spacer()
            Button("Remove") { Task { await remove(c.id) } }.controlSize(.small)
        }
    }

    /// picked binds one catalog entry's tick to the draft's capabilities.
    private func picked(_ id: String) -> Binding<Bool> {
        Binding(
            get: { agent.capabilities.contains(id) },
            set: { on in
                agent.capabilities.removeAll { $0 == id }
                if on { agent.capabilities.append(id) }
            })
    }

    /// The base's skills and those of every ticked plugin; ticked = on.
    private var skillNames: [String] {
        baseSkills + catalog.filter { agent.capabilities.contains($0.id) }.flatMap { $0.skills ?? [] }
    }

    @ViewBuilder private var skillToggles: some View {
        if skillNames.isEmpty {
            hint("No skills.")
        } else {
            ForEach(skillNames, id: \.self) { name in
                Toggle(name, isOn: Binding(
                    get: { !agent.skillsOff.contains(name) },
                    set: { on in
                        agent.skillsOff.removeAll { $0 == name }
                        if !on { agent.skillsOff.append(name) }
                    }))
                .toggleStyle(.checkbox)
            }
        }
    }

    private var addForm: some View {
        DisclosureGroup("add to the catalog") {
            HStack(spacing: 6) {
                TextField("id", text: $newID).frame(width: 160)
                Picker("", selection: $newKind) {
                    Text("installed plugin").tag("plugin")
                    Text("plugin folder").tag("path")
                    Text("MCP server").tag("mcp")
                }
                .labelsHidden().frame(width: 150)
            }
            switch newKind {
            case "plugin":
                Picker("", selection: $newPlugin) {
                    Text("choose a plugin").tag("")
                    ForEach(installed, id: \.self) { Text($0).tag($0) }
                }
                .labelsHidden()
            case "path":
                TextField("/absolute/path/to/plugin", text: $newPath)
            default:
                TextEditor(text: $newServer).font(.system(size: 11, design: .monospaced)).frame(minHeight: 50).border(Color.white.opacity(0.12))
                hint(#"{"type": "http", "url": "http://127.0.0.1:8930/x/mcp", "headers": {"Authorization": "Bearer ${TOKEN}"}}"#)
            }
            HStack {
                Button("Add") { Task { await add() } }.controlSize(.small).disabled(newID.isEmpty)
                Button("Import from this host") { Task { await importHost() } }.controlSize(.small)
            }
        }
        .font(.system(size: 11))
    }

    // MARK: - Actions

    private func load() async {
        let (list, ids, err) = await service.fetchCapabilities()
        catalog = list
        installed = ids
        if let err { status = "Couldn't load the catalog: \(err)" }
    }

    private func add() async {
        let body: [String: Any]
        switch newKind {
        case "plugin": body = ["kind": "plugin", "plugin": newPlugin]
        case "path": body = ["kind": "plugin", "path": newPath.trimmingCharacters(in: .whitespaces)]
        default:
            guard let data = newServer.data(using: .utf8), let server = try? JSONSerialization.jsonObject(with: data) else {
                status = "The server is not JSON."
                return
            }
            body = ["kind": "mcp", "server": server]
        }
        if let error = await service.putCapability(id: newID, body: body) { status = error; return }
        status = "Added \(newID)."
        newID = ""
        await load()
    }

    private func remove(_ id: String) async {
        let alert = NSAlert()
        alert.messageText = "Remove “\(id)” from the catalog?"
        alert.informativeText = "The daemon refuses while an agent still lists it."
        alert.addButton(withTitle: "Remove")
        alert.addButton(withTitle: "Cancel")
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        if let error = await service.deleteCapability(id: id) { status = error; return }
        status = "Removed \(id)."
        await load()
    }

    private func importHost() async {
        let (res, err) = await service.importCapabilities()
        if let err { status = err; return }
        let added = res?.added ?? []
        let skipped = res?.skipped ?? []
        status = (added.isEmpty ? "Nothing new to import." : "Imported " + added.joined(separator: ", ") + ".")
            + (skipped.isEmpty ? "" : " Skipped: " + skipped.joined(separator: "; "))
        await load()
    }
}

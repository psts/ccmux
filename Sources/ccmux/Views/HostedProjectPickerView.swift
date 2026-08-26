import SwiftUI

/// Sheet content for "New hosted session": browse the daemon's projects root
/// and pick a folder. Hosted folders live on the daemon's filesystem (which may
/// be a remote server), so this replaces any local NSOpenPanel — every listing
/// comes from GET /v1/projects, never from this Mac's disk.
///
/// Navigation: double-click (or the chevron) drills into a folder — projects
/// can nest below the root — the back button walks up, and **Add** creates the
/// hosted session from the selected folder.
struct HostedProjectPickerView: View {
    /// Second argument: a one-off raw startup command ("" = a bare shell; the
    /// tab strip's + menu offers harness tabs). Third: the host label to
    /// create on ("" = the hub / single-host default).
    let onPick: (DaemonProject, String, String) -> Void
    let onCancel: () -> Void

    @State private var phase: Phase = .loading
    @State private var root = ""
    @State private var path = ""        // listed folder, relative to root
    @State private var parent: String?  // one level up; nil at the root
    @State private var filter = ""
    @State private var selection: DaemonProject.ID?
    @State private var commandOverride = ""
    @State private var newFolderName = ""
    @State private var newFolderGit = false
    @State private var createError: String?
    @State private var selectedHost = ""              // "" until hosts load (federation)
    private let hosts = RemoteSessionService.shared.hostList

    enum Phase: Equatable {
        case loading
        case failed(String)
        case loaded([DaemonProject])
    }

    private var filtered: [DaemonProject] {
        guard case .loaded(let projects) = phase else { return [] }
        guard !filter.isEmpty else { return projects }
        return projects.filter { $0.name.localizedCaseInsensitiveContains(filter) }
    }

    private var selectedProject: DaemonProject? {
        filtered.first { $0.id == selection }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            header
            content
            footer
        }
        .padding(16)
        .frame(width: 440, height: 480)
        .task {
            selectedHost = RemoteSessionService.shared.defaultCreateHost
            await load(path: "")
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("New Hosted Session")
                .font(.headline)
            // Federation: pick which host to create on (only when there's a choice).
            if hosts.count > 1 {
                Picker("Host", selection: $selectedHost) {
                    ForEach(hosts) { host in
                        Text(host.isSelf ? "\(host.id) (hub)" : host.id).tag(host.id)
                    }
                }
                .pickerStyle(.menu)
                .onChange(of: selectedHost) { Task { await load(path: "") } }
            }
            HStack(spacing: 4) {
                if let parent {
                    Button {
                        Task { await load(path: parent) }
                    } label: {
                        Image(systemName: "chevron.left")
                    }
                    .buttonStyle(.borderless)
                    .help("Back")
                }
                Text(displayPath)
                    .font(.system(size: 11, design: .monospaced))
                    .foregroundColor(.secondary)
                    .lineLimit(1)
                    .truncationMode(.head)
            }
            TextField("Filter", text: $filter)
                .textFieldStyle(.roundedBorder)
        }
    }

    private var displayPath: String {
        path.isEmpty ? root : "\(root)/\(path)"
    }

    @ViewBuilder
    private var content: some View {
        switch phase {
        case .loading:
            centered { ProgressView() }
        case .failed(let message):
            centered {
                VStack(spacing: 6) {
                    Text("Couldn't reach the daemon")
                        .foregroundColor(.secondary)
                    Text(message)
                        .font(.system(size: 11))
                        .foregroundColor(.secondary.opacity(0.7))
                }
            }
        case .loaded(let projects) where projects.isEmpty:
            centered {
                Text("No folders in here")
                    .foregroundColor(.secondary)
            }
        case .loaded:
            projectList
        }
    }

    private var projectList: some View {
        List(filtered, selection: $selection) { project in
            projectRow(project)
        }
        .listStyle(.inset)
    }

    private func projectRow(_ project: DaemonProject) -> some View {
        HStack {
            Image(systemName: "folder")
                .foregroundColor(.secondary)
            Text(project.name)
            Spacer()
            if project.git {
                Text("git")
                    .font(.system(size: 10, weight: .medium))
                    .padding(.horizontal, 6)
                    .padding(.vertical, 2)
                    .background(Color.secondary.opacity(0.2))
                    .cornerRadius(4)
                    .foregroundColor(.secondary)
            }
            Button {
                Task { await descend(into: project) }
            } label: {
                Image(systemName: "chevron.right")
                    .font(.system(size: 10))
                    .foregroundColor(.secondary)
            }
            .buttonStyle(.borderless)
            .help("Browse into \(project.name)")
        }
        .contentShape(Rectangle())
        .onTapGesture(count: 2) { Task { await descend(into: project) } }
        .onTapGesture { selection = project.id }
    }

    /// New folder in the listed location: plain (a group of projects) or
    /// git-inited (a repo-to-be). Creation happens on the daemon's filesystem,
    /// same as every listing.
    private var newFolderRow: some View {
        VStack(alignment: .leading, spacing: 4) {
            if let createError {
                Text(createError)
                    .font(.system(size: 11))
                    .foregroundColor(.red)
                    .lineLimit(2)
            }
            HStack(spacing: 8) {
                TextField("new folder here", text: $newFolderName)
                    .textFieldStyle(.roundedBorder)
                    .onSubmit { Task { await createFolder() } }
                Toggle("git init", isOn: $newFolderGit)
                    .toggleStyle(.checkbox)
                    .help("Also run git init — for a folder that will hold a repo, not more folders")
                Button("Create") {
                    Task { await createFolder() }
                }
                .disabled(newFolderName.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }
    }

    private func createFolder() async {
        let name = newFolderName.trimmingCharacters(in: .whitespaces)
        guard !name.isEmpty else { return }
        createError = nil
        let rel = path.isEmpty ? name : "\(path)/\(name)"
        if let error = await RemoteSessionService.shared.createProjectFolder(
            host: selectedHost, path: rel, git: newFolderGit) {
            createError = error
            return
        }
        newFolderName = ""
        newFolderGit = false
        await load(path: path) // re-list; the new folder appears in place
        selection = filtered.first { $0.name == name }?.id
    }

    private var footer: some View {
        VStack(alignment: .leading, spacing: 8) {
            newFolderRow
            TextField(overridePlaceholder, text: $commandOverride)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 11, design: .monospaced))
                .help("One-off raw startup command for this workspace; empty opens a shell — the tab strip's + menu adds harness tabs.")
            HStack {
                Spacer()
                Button("Cancel", action: onCancel)
                    .keyboardShortcut(.cancelAction)
                Button("Add") {
                    if let project = selectedProject {
                        onPick(project, commandOverride.trimmingCharacters(in: .whitespaces), selectedHost)
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(selectedProject == nil)
            }
        }
    }

    private var overridePlaceholder: String {
        "startup command — empty = shell"
    }

    private func centered<V: View>(@ViewBuilder _ inner: () -> V) -> some View {
        inner().frame(maxWidth: .infinity, maxHeight: .infinity)
    }

    private func descend(into project: DaemonProject) async {
        await load(path: path.isEmpty ? project.name : "\(path)/\(project.name)")
    }

    private func load(path newPath: String) async {
        phase = .loading
        selection = nil
        filter = ""
        do {
            let list = try await RemoteSessionService.shared.fetchProjects(host: selectedHost, path: newPath)
            root = list.root
            path = list.path
            parent = list.parent
            phase = .loaded(list.projects)
        } catch {
            phase = .failed(error.localizedDescription)
        }
    }
}

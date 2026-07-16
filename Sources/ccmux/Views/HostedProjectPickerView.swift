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
    /// Second argument: a one-off startup-command override (nil = the daemon
    /// resolves it from per-folder rules / the Settings default).
    let onPick: (DaemonProject, String?) -> Void
    let onCancel: () -> Void

    @State private var phase: Phase = .loading
    @State private var root = ""
    @State private var path = ""        // listed folder, relative to root
    @State private var parent: String?  // one level up; nil at the root
    @State private var filter = ""
    @State private var selection: DaemonProject.ID?
    @State private var commandOverride = ""
    @State private var defaultCommand = ""

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
        .frame(width: 440, height: 440)
        .task {
            await load(path: "")
            defaultCommand = (try? await RemoteSessionService.shared.fetchSettings())?.startupCommand ?? ""
        }
    }

    private var header: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text("New Hosted Session")
                .font(.headline)
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

    private var footer: some View {
        VStack(alignment: .leading, spacing: 8) {
            TextField(overridePlaceholder, text: $commandOverride)
                .textFieldStyle(.roundedBorder)
                .font(.system(size: 11, design: .monospaced))
                .help("One-off startup command for this workspace; empty uses the daemon's default (per-folder rules apply).")
            HStack {
                Spacer()
                Button("Cancel", action: onCancel)
                    .keyboardShortcut(.cancelAction)
                Button("Add") {
                    if let project = selectedProject {
                        let trimmed = commandOverride.trimmingCharacters(in: .whitespaces)
                        onPick(project, trimmed.isEmpty ? nil : trimmed)
                    }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(selectedProject == nil)
            }
        }
    }

    private var overridePlaceholder: String {
        defaultCommand.isEmpty
            ? "startup command — empty = default"
            : "startup command — empty = default (\(defaultCommand))"
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
            let list = try await RemoteSessionService.shared.fetchProjects(path: newPath)
            root = list.root
            path = list.path
            parent = list.parent
            phase = .loaded(list.projects)
        } catch {
            phase = .failed(error.localizedDescription)
        }
    }
}

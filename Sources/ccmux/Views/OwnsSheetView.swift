import SwiftUI

/// "What It Owns…": the one line every session in a window is told about a
/// repo, so work there is handed to its session. Haiku writes it from the
/// repo's CLAUDE.md and README; a typed line overrides it, and "Use summary"
/// hands it back. Presented as an AppKit sheet from the hosted row's context
/// menu. Mirrors the web lens's owns.js.
struct OwnsSheetView: View {
    let workspaceName: String
    let load: () async -> Result<DaemonRepoOwns, RepoOwnsError>
    let save: (String) async -> Result<DaemonRepoOwns, RepoOwnsError>
    let onClose: () -> Void

    @State private var auto = ""
    @State private var manual = ""
    @State private var loaded = false
    @State private var status = ""
    @State private var saving = false

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text("What \(workspaceName) owns").font(.headline)
            Text("Every session in this window is told what each repo owns, so work in this repo is handed to its session. Haiku writes it from the repo's CLAUDE.md and README; type your own line to override it.")
                .font(.system(size: 11)).foregroundColor(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            Text(summaryText).font(.system(size: 11)).foregroundColor(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            TextField("e.g. API, database, auth, billing jobs", text: $manual)
                .textFieldStyle(.roundedBorder)
                .onSubmit { commit(manual) }
            if !status.isEmpty {
                Text(status).font(.system(size: 11)).foregroundColor(.red)
            }
            HStack {
                Spacer()
                Button("Cancel", action: onClose).keyboardShortcut(.cancelAction)
                Button("Use summary") { commit("") }
                Button("Save") { commit(manual) }.keyboardShortcut(.defaultAction)
            }
            .disabled(saving)
        }
        .padding(16)
        .frame(width: 440)
        .task {
            switch await load() {
            case .success(let o): auto = o.auto; manual = o.manual
            case .failure(let e): status = e.message
            }
            loaded = true
        }
    }

    private var summaryText: String {
        if !loaded { return "Reading…" }
        if auto.isEmpty {
            return "No summary yet. It is written the next time a session in this window starts, if the repo has a CLAUDE.md or README."
        }
        return "Summary: \(auto)"
    }

    /// Saves and closes; a refusal stays in the sheet, like the web lens.
    private func commit(_ line: String) {
        saving = true
        status = ""
        Task {
            let result = await save(line.trimmingCharacters(in: .whitespacesAndNewlines))
            saving = false
            switch result {
            case .success: onClose()
            case .failure(let e): status = e.message
            }
        }
    }
}

import Foundation

/// One entry of a directory listing, as the file explorer's tree renders it.
struct FileSourceEntry {
    let name: String
    let isDirectory: Bool
}

/// Where a file explorer reads and writes its files. Local workspaces use the
/// Mac's filesystem; hosted workspaces go through the daemon's file routes,
/// because the repo lives on the daemon's host (same reason git status is
/// computed daemon-side). Paths are repo-root-relative; absolute paths are
/// accepted when they point inside the root.
protocol FileSource {
    /// True when the source can be watched with `FileWatcher` (local disk only).
    var watchesDisk: Bool { get }
    func read(path: String) async -> String?
    /// Overwrites an existing file. Returns success.
    func write(path: String, content: String) async -> Bool
    func list(path: String) async -> [FileSourceEntry]?
}

// MARK: - Local disk

struct LocalFileSource: FileSource {
    let rootPath: String
    var watchesDisk: Bool { true }

    private func absolute(_ path: String) -> String {
        path.hasPrefix("/") ? path : (rootPath as NSString).appendingPathComponent(path)
    }

    func read(path: String) async -> String? {
        guard let data = FileManager.default.contents(atPath: absolute(path)) else { return nil }
        return String(data: data, encoding: .utf8)
    }

    func write(path: String, content: String) async -> Bool {
        (try? content.write(toFile: absolute(path), atomically: true, encoding: .utf8)) != nil
    }

    func list(path: String) async -> [FileSourceEntry]? {
        let dir = absolute(path)
        guard let names = try? FileManager.default.contentsOfDirectory(atPath: dir) else { return nil }
        return names.map { name in
            var isDir: ObjCBool = false
            let child = (dir as NSString).appendingPathComponent(name)
            FileManager.default.fileExists(atPath: child, isDirectory: &isDir)
            return FileSourceEntry(name: name, isDirectory: isDir.boolValue)
        }
    }
}

// MARK: - Daemon (hosted workspaces)

/// Reads/writes a hosted workspace's repo via the daemon's file routes
/// (`/v1/workspaces/{id}/files`, `/v1/workspaces/{id}/dir`). The base URL is
/// resolved per request like every other daemon call, so hub adoption and
/// `CCMUXD_URL` pins keep working.
struct DaemonFileSource: FileSource {
    let daemonId: String
    var watchesDisk: Bool { false }

    private func url(_ route: String, path: String) -> URL? {
        var comps = URLComponents(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/\(route)")
        comps?.queryItems = [URLQueryItem(name: "path", value: path)]
        return comps?.url
    }

    func read(path: String) async -> String? {
        struct FileResponse: Decodable { let content: String }
        guard let url = url("files", path: path),
              let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200
        else { return nil }
        return (try? JSONDecoder().decode(FileResponse.self, from: data))?.content
    }

    func write(path: String, content: String) async -> Bool {
        guard let url = URL(string: "\(DaemonConfig.baseURL)/v1/workspaces/\(daemonId)/files"),
              let body = try? JSONEncoder().encode(["path": path, "content": content])
        else { return false }
        var req = URLRequest(url: url)
        req.httpMethod = "PUT"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = body
        guard let (_, resp) = try? await URLSession.shared.data(for: req) else { return false }
        return (resp as? HTTPURLResponse)?.statusCode == 204
    }

    func list(path: String) async -> [FileSourceEntry]? {
        struct DirResponse: Decodable {
            struct Entry: Decodable { let name: String; let dir: Bool }
            let entries: [Entry]
        }
        guard let url = url("dir", path: path),
              let (data, resp) = try? await URLSession.shared.data(from: url),
              (resp as? HTTPURLResponse)?.statusCode == 200,
              let decoded = try? JSONDecoder().decode(DirResponse.self, from: data)
        else { return nil }
        return decoded.entries.map { FileSourceEntry(name: $0.name, isDirectory: $0.dir) }
    }
}

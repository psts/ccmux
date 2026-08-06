import AppKit

/// Self-update from GitHub releases — the app-side sibling of `ccmuxd upgrade`.
///
/// Two entry points, one flow: "Check for Updates…" (always answers) and the
/// automatic launch + periodic check (speaks only when an update exists). Both
/// ask the releases API for the latest tag, and when it's
/// newer than this bundle's version: downloads `ccmux-app.zip`, verifies the
/// code signature of the unpacked app, clears its quarantine flag (the updater
/// is a local process acting on the user's explicit request — this is what
/// makes self-update frictionless even for a not-yet-notarized build), swaps
/// the bundle in place with a rollback path, and relaunches.
///
/// Integrity model: TLS to GitHub plus a strict `codesign --verify` on the
/// downloaded app. Team-ID pinning is deliberately not enforced yet — interim
/// CI builds may be ad-hoc signed until notarization credentials exist.
@MainActor
final class UpdaterService {
    static let shared = UpdaterService()

    private let repo = "psts/ccmux"
    private let assetName = "ccmux-app.zip"
    private var running = false
    private var runningQuiet = false
    private var autoTimer: Timer?
    /// Version the user declined in a QUIET prompt — the 4h timer must not
    /// re-ask for the same release every cycle. The menu path ignores this.
    private var declinedVersion: String?

    /// Menu path ("Check for Updates…"): always answers, including "you're up
    /// to date".
    func checkForUpdates() { start(quiet: false) }

    /// Automatic path: one check at launch plus one every `interval`. Quiet —
    /// the update prompt is the ONLY thing it ever shows before the user
    /// consents; up-to-date, a still-building release, and pre-consent network
    /// errors pass silently (logged, never alerted), so it never nags someone
    /// who's current or offline. AFTER the user clicks Update, failures alert
    /// regardless — quiet describes unsolicited interruptions, not the outcome
    /// of an action the user asked for.
    func startAutomaticChecks(interval: TimeInterval = 4 * 3600) {
        start(quiet: true)
        autoTimer?.invalidate()
        autoTimer = Timer.scheduledTimer(withTimeInterval: interval, repeats: true) { _ in
            Task { @MainActor in UpdaterService.shared.start(quiet: true) }
        }
    }

    private func start(quiet: Bool) {
        guard !running else {
            // The menu press must never silently no-op (a hung quiet request
            // can hold `running` for its whole network timeout).
            if !quiet && runningQuiet {
                alert("Already checking",
                      "An automatic update check is running — try again in a moment.")
            }
            return
        }
        // Dev builds: an unbundled `swift build` binary (no Info.plist →
        // version "0") and git-describe-stamped local bundles both compare
        // "older" than the latest release, so the auto-check would offer a
        // downgrade/overwrite on every launch.
        if quiet && (Bundle.main.bundleIdentifier == nil || !Self.autoCheckEligible(currentVersion)) {
            return
        }
        running = true
        runningQuiet = quiet
        Task {
            defer { running = false }
            await check(quiet: quiet)
        }
    }

    /// Versions the AUTOMATIC check may act on: release-shaped only. Rejected:
    /// no numeric segment at all ("dev"), and git-describe source stamps
    /// ("0.1.4-2-gabc123", "…-dirty") — every release compares newer than the
    /// former and later releases beat the latter, so the auto-check would
    /// offer to overwrite a developer's local build. The menu path still
    /// allows all of them deliberately. Internal for the test neighbor.
    nonisolated static func autoCheckEligible(_ current: String) -> Bool {
        current.contains(where: \.isNumber)
            && !current.contains("-g")
            && !current.hasSuffix("-dirty")
    }

    private var currentVersion: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0"
    }

    private func check(quiet: Bool) async {
        let release: Release
        do {
            release = try await latestRelease()
        } catch {
            // Pre-consent failure: quiet stays quiet for the USER but never
            // for the log — an invisibly broken auto-update is worse than none.
            if quiet { NSLog("[ccmux updater] quiet check failed: \(error)") }
            else { alert("Update failed", error.localizedDescription) }
            return
        }
        let latest = release.tag.hasPrefix("v") ? String(release.tag.dropFirst()) : release.tag
        let current = currentVersion
        guard Self.isNewer(latest, than: current) else {
            if !quiet {
                alert("You're up to date",
                      current == latest
                          ? "ccmux \(current) is the latest release."
                          : "You're on \(current), ahead of the latest release (\(latest)) — likely a source build.")
            }
            return
        }
        if quiet && latest == declinedVersion { return } // asked once; don't nag every 4h
        guard let asset = release.assetURL else {
            if !quiet {
                alert("Update available",
                      "ccmux \(latest) is out, but its app download isn't attached yet — the release may still be building. Try again in a few minutes.")
            }
            return
        }
        guard confirm("Update to ccmux \(latest)?",
                      "You're on \(current). ccmux will download the update, replace itself, and relaunch. Terminals keep running — they live in the daemon, not the app.",
                      updateIsDefault: !quiet) else {
            if quiet { declinedVersion = latest }
            return
        }
        do {
            try await downloadAndInstall(from: asset)
        } catch {
            // Post-consent: the user asked for this install — its failure is
            // never quiet.
            alert("Update failed", error.localizedDescription)
        }
    }

    // MARK: release lookup

    private struct Release { let tag: String; let assetURL: URL? }

    private func latestRelease() async throws -> Release {
        struct APIRelease: Decodable {
            struct Asset: Decodable {
                let name: String
                let browser_download_url: URL
            }
            let tag_name: String
            let assets: [Asset]
        }
        let url = URL(string: "https://api.github.com/repos/\(repo)/releases/latest")!
        let (data, response) = try await URLSession.shared.data(from: url)
        guard (response as? HTTPURLResponse)?.statusCode == 200 else {
            throw UpdateError("Could not reach the releases API.")
        }
        let rel = try JSONDecoder().decode(APIRelease.self, from: data)
        let asset = rel.assets.first(where: { $0.name == assetName })
        return Release(tag: rel.tag_name, assetURL: asset?.browser_download_url)
    }

    /// Numeric dot-segment comparison; a source build ("0.1.4-dirty", "dev")
    /// compares by its leading numeric segments and never sees a same-version
    /// "update". Internal for the test neighbor.
    nonisolated static func isNewer(_ candidate: String, than current: String) -> Bool {
        func nums(_ s: String) -> [Int] {
            s.split(separator: ".").map { Int($0.prefix(while: \.isNumber)) ?? 0 }
        }
        let a = nums(candidate), b = nums(current)
        for i in 0..<max(a.count, b.count) {
            let x = i < a.count ? a[i] : 0
            let y = i < b.count ? b[i] : 0
            if x != y { return x > y }
        }
        return false
    }

    // MARK: download + swap

    private func downloadAndInstall(from url: URL) async throws {
        let dest = Bundle.main.bundleURL
        guard FileManager.default.isWritableFile(atPath: dest.deletingLastPathComponent().path) else {
            throw UpdateError("Can't write next to \(dest.path) — move ccmux.app to /Applications and try again.")
        }
        let (zip, _) = try await URLSession.shared.download(from: url)
        let newApp = try await Task.detached { try Self.unpackAndVerify(zip: zip) }.value

        // Swap with rollback: aside-rename the running bundle (legal on macOS —
        // the process keeps its inode), move the new one in, undo on failure.
        let aside = dest.deletingLastPathComponent().appendingPathComponent(".ccmux-previous.app")
        try? FileManager.default.removeItem(at: aside)
        try FileManager.default.moveItem(at: dest, to: aside)
        do {
            try FileManager.default.moveItem(at: newApp, to: dest)
        } catch {
            try? FileManager.default.moveItem(at: aside, to: dest)
            throw error
        }
        try? FileManager.default.removeItem(at: aside)
        try? FileManager.default.removeItem(at: newApp.deletingLastPathComponent()) // staging dir
        relaunch(dest)
    }

    private nonisolated static func unpackAndVerify(zip: URL) throws -> URL {
        let stage = FileManager.default.temporaryDirectory
            .appendingPathComponent("ccmux-update-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: stage, withIntermediateDirectories: true)
        try run("/usr/bin/ditto", "-x", "-k", zip.path, stage.path)
        try? FileManager.default.removeItem(at: zip)
        guard let appName = try FileManager.default.contentsOfDirectory(atPath: stage.path)
            .first(where: { $0.hasSuffix(".app") }) else {
            throw UpdateError("The downloaded archive contains no app bundle.")
        }
        let newApp = stage.appendingPathComponent(appName)
        try run("/usr/bin/codesign", "--verify", "--deep", "--strict", newApp.path)
        // Self-anchoring authenticity: once a Developer-ID-signed build is what's
        // running, every update must carry the SAME team — an ad-hoc or
        // foreign-signed bundle is refused. While the running build has no team
        // (dev/ad-hoc bootstrap phase), signature consistency is all we can ask.
        if let team = teamID(of: Bundle.main.bundleURL) {
            guard teamID(of: newApp) == team else {
                throw UpdateError("The downloaded app is not signed by the expected team (\(team)) — refusing to install it.")
            }
        }
        // Clearing quarantine here is the explicit-user-intent step that lets a
        // signed-but-not-notarized build launch without Gatekeeper friction.
        try? run("/usr/bin/xattr", "-dr", "com.apple.quarantine", newApp.path)
        return newApp
    }

    /// The TeamIdentifier from a bundle's signature, nil for ad-hoc/unsigned.
    private nonisolated static func teamID(of bundle: URL) -> String? {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/codesign")
        p.arguments = ["-dvv", bundle.path]
        let out = Pipe()
        p.standardError = out // codesign prints details to stderr
        guard (try? p.run()) != nil else { return nil }
        p.waitUntilExit()
        let text = String(data: out.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
        for line in text.split(separator: "\n") where line.hasPrefix("TeamIdentifier=") {
            let id = String(line.dropFirst("TeamIdentifier=".count))
            return id == "not set" ? nil : id
        }
        return nil
    }

    private nonisolated static func run(_ tool: String, _ args: String...) throws {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: tool)
        p.arguments = args
        let err = Pipe()
        p.standardError = err
        try p.run()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            let msg = String(data: err.fileHandleForReading.readDataToEndOfFile(), encoding: .utf8) ?? ""
            throw UpdateError("\(URL(fileURLWithPath: tool).lastPathComponent) failed: \(msg.trimmingCharacters(in: .whitespacesAndNewlines))")
        }
    }

    private func relaunch(_ app: URL) {
        // Wait for THIS process to actually exit before launching, and launch a
        // NEW instance (-n): a bare `open` after a fixed sleep races the app's
        // own shutdown (state save + terminal reaping can outrun it) and would
        // merely re-activate the dying instance — the documented `open`
        // re-activation trap. Capped at ~30s so the helper can't linger forever.
        let pid = ProcessInfo.processInfo.processIdentifier
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-c",
            "n=0; while kill -0 \(pid) 2>/dev/null && [ $n -lt 150 ]; do sleep 0.2; n=$((n+1)); done; open -n \"\(app.path)\""]
        try? p.run()
        NSApp.terminate(nil)
    }

    // MARK: UI

    private struct UpdateError: LocalizedError {
        let message: String
        init(_ m: String) { message = m }
        var errorDescription: String? { message }
    }

    private func alert(_ title: String, _ text: String) {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        a.runModal()
    }

    /// updateIsDefault false = the QUIET path: the alert interrupts whatever
    /// the user was doing (it can fire mid-typing in a terminal), so Return
    /// must NOT mean "replace and relaunch the app" — Cancel gets the default.
    /// The menu path keeps Update as the default; the user just asked for it.
    private func confirm(_ title: String, _ text: String, updateIsDefault: Bool) -> Bool {
        let a = NSAlert()
        a.messageText = title
        a.informativeText = text
        if updateIsDefault {
            a.addButton(withTitle: "Update")
            a.addButton(withTitle: "Cancel")
            return a.runModal() == .alertFirstButtonReturn
        }
        a.addButton(withTitle: "Cancel")
        a.addButton(withTitle: "Update")
        return a.runModal() == .alertSecondButtonReturn
    }
}

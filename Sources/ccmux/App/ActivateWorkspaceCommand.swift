import AppKit

/// AppleScript command: `tell application "ccmux" to activate workspace "backend"`
/// Finds the workspace by name, switches its owning window to display it,
/// and brings that window to front (switching macOS Spaces if needed).
class ActivateWorkspaceCommand: NSScriptCommand {
    override func performDefaultImplementation() -> Any? {
        guard let workspaceName = directParameter as? String else {
            scriptErrorNumber = errOSACantAccess
            scriptErrorString = "Expected a workspace name (text)."
            return nil
        }

        // Find the app delegate and window manager
        guard let appDelegate = NSApp.delegate as? AppDelegate,
              let windowManager = appDelegate.windowManagerForScripting else {
            scriptErrorNumber = -1
            scriptErrorString = "Application not ready."
            return nil
        }

        // Find the workspace by name (case-insensitive) — local or hosted
        let all = windowManager.workspaceManager.workspaces + RemoteSessionService.shared.workspaces
        guard let workspace = all.first(where: {
            $0.name.caseInsensitiveCompare(workspaceName) == .orderedSame
        }) else {
            scriptErrorNumber = errOSACantAccess
            scriptErrorString = "No workspace named '\(workspaceName)' found."
            return nil
        }

        // Find which window owns this workspace
        guard let ownerWc = windowManager.windowOwning(workspaceId: workspace.id) else {
            scriptErrorNumber = errOSACantAccess
            scriptErrorString = "Workspace '\(workspaceName)' is not in any open window."
            return nil
        }

        // Switch that window to display this workspace
        ownerWc.windowContext.displayedWorkspaceId = workspace.id
        ownerWc.updateWindowTitle()

        // Bring the window to front (switches Spaces automatically)
        if let window = ownerWc.window {
            NSApp.activate(ignoringOtherApps: true)
            window.makeKeyAndOrderFront(nil)
        }

        return nil
    }
}

/// AppleScript command: `tell application "ccmux" to list workspaces`
/// Returns a newline-separated list of all workspace names with their window.
class ListWorkspacesCommand: NSScriptCommand {
    override func performDefaultImplementation() -> Any? {
        guard let appDelegate = NSApp.delegate as? AppDelegate,
              let windowManager = appDelegate.windowManagerForScripting else {
            return "Application not ready."
        }

        var lines: [String] = []
        for wc in windowManager.windowControllers {
            let windowName = wc.windowContext.windowName ?? wc.window?.title ?? "Window"
            let ownedIds = wc.windowContext.ownedWorkspaceIds
            let workspaces = (windowManager.workspaceManager.workspaces + RemoteSessionService.shared.workspaces)
                .filter { ownedIds.contains($0.id) }
                .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }

            for ws in workspaces {
                let displayed = wc.windowContext.displayedWorkspaceId == ws.id ? " *" : ""
                lines.append("\(windowName): \(ws.name)\(displayed)")
            }
        }

        return lines.joined(separator: "\n")
    }
}

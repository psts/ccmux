import AppKit

/// AppleScript command: `tell application "ccmux" to focus (window id 12345)`
/// Brings the specified window to front, switching macOS Spaces if needed.
class FocusWindowCommand: NSScriptCommand {
    override func performDefaultImplementation() -> Any? {
        guard let specifier = directParameter as? NSScriptObjectSpecifier else {
            scriptErrorNumber = errOSACantAccess
            scriptErrorString = "Expected a window specifier."
            return nil
        }

        let evaluated = specifier.objectsByEvaluatingSpecifier
        guard let window = evaluated as? NSWindow else {
            scriptErrorNumber = errOSACantAccess
            scriptErrorString = "Could not find the specified window."
            return nil
        }

        focusWindow(window)
        return nil
    }

    private func focusWindow(_ window: NSWindow) {
        // Temporarily allow the window to join all spaces so it can pull to current space
        let originalBehavior = window.collectionBehavior
        window.collectionBehavior.insert(.canJoinAllSpaces)

        // Activate the app and bring window to front
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()

        // Restore original collection behavior after the space switch completes
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.5) {
            window.collectionBehavior = originalBehavior
        }
    }
}

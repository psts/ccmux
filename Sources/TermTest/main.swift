import AppKit
import SwiftTerm

// Change this to test different values
let termProgram = "WezTerm"
let termProgramVersion = "20240203-110809-5046fc22"

class AppDelegate: NSObject, NSApplicationDelegate {
    var window: NSWindow!
    var terminal: LocalProcessTerminalView!

    func applicationDidFinishLaunching(_ notification: Notification) {
        window = NSWindow(
            contentRect: NSRect(x: 200, y: 200, width: 900, height: 500),
            styleMask: [.titled, .closable, .resizable, .miniaturizable],
            backing: .buffered,
            defer: false
        )
        window.title = "TermTest — TERM_PROGRAM=\(termProgram)"

        terminal = LocalProcessTerminalView(frame: window.contentView!.bounds)
        terminal.autoresizingMask = [.width, .height]
        terminal.font = NSFont(name: "Monaco", size: 12) ?? NSFont.monospacedSystemFont(ofSize: 12, weight: .regular)
        terminal.nativeForegroundColor = NSColor(white: 0.85, alpha: 1.0)
        terminal.nativeBackgroundColor = NSColor(red: 0.11, green: 0.12, blue: 0.14, alpha: 1.0)

        window.contentView!.addSubview(terminal)

        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        let shellName = "-" + (shell as NSString).lastPathComponent

        var env = ProcessInfo.processInfo.environment
        env["TERM"] = "xterm-256color"
        env["COLORTERM"] = "truecolor"
        env["TERM_PROGRAM"] = termProgram
        env["TERM_PROGRAM_VERSION"] = termProgramVersion
        env["LANG"] = env["LANG"] ?? "en_US.UTF-8"

        let envArray = env.map { "\($0.key)=\($0.value)" }

        terminal.startProcess(
            executable: shell,
            args: [],
            environment: envArray,
            execName: shellName
        )

        window.makeKeyAndOrderFront(nil)
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }
}

let app = NSApplication.shared
let delegate = AppDelegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.activate(ignoringOtherApps: true)
app.run()

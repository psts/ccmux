import AppKit
import SwiftUI

@MainActor
class PeerMessagesController {
    private var panel: NSPanel?
    private var state = PeerMessagesState()
    private var clickOutsideMonitor: Any?
    private var isShowing = false
    private var lastDismissTime: Date?

    func toggle(group: String, relativeTo window: NSWindow?) {
        if isShowing {
            dismiss()
        } else {
            // Don't reopen if just dismissed (click-outside monitor race)
            if let last = lastDismissTime, Date().timeIntervalSince(last) < 0.3 {
                return
            }
            show(group: group, relativeTo: window)
        }
    }

    func show(group: String, relativeTo window: NSWindow?) {
        guard !isShowing else { return }
        isShowing = true

        state.start(group: group)

        let view = PeerMessagesOverlayView(state: state, onClose: { [weak self] in
            self?.dismiss()
        })
        let hostingView = NSHostingView(rootView: view)

        // Size panel relative to the parent window (40% width, 80% height)
        let refFrame = window?.frame ?? (NSScreen.main ?? NSScreen.screens.first!).visibleFrame
        let panelWidth = round(refFrame.width * 0.4)
        let panelHeight = round(refFrame.height * 0.8)

        let panel = NSPanel(
            contentRect: NSRect(x: 0, y: 0, width: panelWidth, height: panelHeight),
            styleMask: [.titled, .resizable, .nonactivatingPanel, .fullSizeContentView],
            backing: .buffered,
            defer: false
        )
        panel.contentView = hostingView
        panel.level = .floating
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.titlebarAppearsTransparent = true
        panel.titleVisibility = .hidden
        panel.isMovable = false
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        panel.minSize = NSSize(width: 360, height: 300)
        panel.standardWindowButton(.closeButton)?.isHidden = true
        panel.standardWindowButton(.miniaturizeButton)?.isHidden = true
        panel.standardWindowButton(.zoomButton)?.isHidden = true

        // Position at top-right of the window
        if let window {
            let windowFrame = window.frame
            let panelSize = panel.frame.size
            let origin = NSPoint(
                x: windowFrame.maxX - panelSize.width - 16,
                y: windowFrame.maxY - panelSize.height - 48
            )
            panel.setFrameOrigin(origin)
        } else {
            let screen = NSScreen.main ?? NSScreen.screens.first!
            let screenFrame = screen.visibleFrame
            let panelSize = panel.frame.size
            let origin = NSPoint(
                x: screenFrame.maxX - panelSize.width - 16,
                y: screenFrame.maxY - panelSize.height - 48
            )
            panel.setFrameOrigin(origin)
        }

        panel.orderFront(nil)
        self.panel = panel

        // Click-outside-to-dismiss
        clickOutsideMonitor = NSEvent.addLocalMonitorForEvents(matching: .leftMouseDown) { [weak self] event in
            guard let self, let panel = self.panel else { return event }
            if let eventWindow = event.window {
                if eventWindow === panel { return event }
                let screenPoint = eventWindow.convertPoint(toScreen: event.locationInWindow)
                if !panel.frame.contains(screenPoint) {
                    self.dismiss()
                }
            } else {
                self.dismiss()
            }
            return event
        }
    }

    func dismiss() {
        guard isShowing else { return }
        isShowing = false
        lastDismissTime = Date()

        state.stop()

        if let monitor = clickOutsideMonitor {
            NSEvent.removeMonitor(monitor)
            clickOutsideMonitor = nil
        }

        panel?.orderOut(nil)
        panel = nil
    }
}

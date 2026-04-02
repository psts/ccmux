import AppKit
import SwiftUI

class QuitConfirmationController {
    private var eventMonitor: Any?
    private var quitPanel: NSPanel?
    private var quitTimer: Timer?
    private var quitStartTime: Date?
    private var isShowingConfirmation = false
    private var state = QuitConfirmationState()

    static let holdDuration: TimeInterval = 2.0

    func install() {
        eventMonitor = NSEvent.addLocalMonitorForEvents(
            matching: [.keyDown, .keyUp, .flagsChanged]
        ) { [weak self] event in
            self?.handleEvent(event) ?? event
        }
    }

    func teardown() {
        if let monitor = eventMonitor {
            NSEvent.removeMonitor(monitor)
            eventMonitor = nil
        }
        cancelQuit()
    }

    private func handleEvent(_ event: NSEvent) -> NSEvent? {
        switch event.type {
        case .keyDown:
            if event.modifierFlags.contains(.command),
               event.charactersIgnoringModifiers == "q" {
                if !event.isARepeat && !isShowingConfirmation {
                    startQuitConfirmation()
                }
                return nil
            }
        case .keyUp:
            if isShowingConfirmation,
               event.charactersIgnoringModifiers == "q" {
                cancelQuit()
                return nil
            }
        case .flagsChanged:
            if isShowingConfirmation,
               !event.modifierFlags.contains(.command) {
                cancelQuit()
                return nil
            }
        default:
            break
        }
        return event
    }

    private func startQuitConfirmation() {
        isShowingConfirmation = true
        state.progress = 0.0
        quitStartTime = Date()
        showPanel()

        quitTimer = Timer.scheduledTimer(withTimeInterval: 1.0 / 60.0, repeats: true) { [weak self] _ in
            self?.timerFired()
        }
    }

    private func timerFired() {
        guard let startTime = quitStartTime else { return }
        let elapsed = Date().timeIntervalSince(startTime)
        let progress = min(1.0, elapsed / Self.holdDuration)
        state.progress = progress

        if progress >= 1.0 {
            quitTimer?.invalidate()
            quitTimer = nil
            NSApp.terminate(nil)
        }
    }

    private func cancelQuit() {
        isShowingConfirmation = false
        quitTimer?.invalidate()
        quitTimer = nil
        quitStartTime = nil
        state.progress = 0.0
        dismissPanel()
    }

    private func showPanel() {
        let view = QuitConfirmationOverlayView(state: state)
        let hostingView = NSHostingView(rootView: view)
        hostingView.setFrameSize(hostingView.fittingSize)

        let panel = NSPanel(
            contentRect: hostingView.bounds,
            styleMask: [.borderless, .nonactivatingPanel],
            backing: .buffered,
            defer: false
        )
        panel.contentView = hostingView
        panel.level = .floating
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = false
        panel.ignoresMouseEvents = true
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]

        let screen = NSApp.keyWindow?.screen ?? NSScreen.main ?? NSScreen.screens.first!
        let screenFrame = screen.visibleFrame
        let panelSize = hostingView.bounds.size
        let origin = NSPoint(
            x: screenFrame.midX - panelSize.width / 2,
            y: screenFrame.midY - panelSize.height / 2
        )
        panel.setFrameOrigin(origin)
        panel.orderFront(nil)
        quitPanel = panel
    }

    private func dismissPanel() {
        quitPanel?.orderOut(nil)
        quitPanel = nil
    }
}

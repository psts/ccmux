import AppKit
import SwiftUI

/// The scratchpad's text area. An NSTextView rather than SwiftUI's TextEditor,
/// because TextEditor offers no way to set the scroll position: a scratchpad
/// opens scrolled to the bottom, where the newest notes are, with the cursor
/// at the end so typing carries on from there.
struct ScratchpadTextView: NSViewRepresentable {
    @Binding var text: String

    func makeCoordinator() -> Coordinator {
        Coordinator(text: $text)
    }

    func makeNSView(context: Context) -> NSScrollView {
        let scrollView = NSTextView.scrollableTextView()
        scrollView.borderType = .noBorder
        scrollView.drawsBackground = false
        guard let textView = scrollView.documentView as? NSTextView else { return scrollView }

        textView.isRichText = false
        textView.allowsUndo = true
        textView.isAutomaticQuoteSubstitutionEnabled = false
        textView.isAutomaticDashSubstitutionEnabled = false
        textView.isAutomaticTextReplacementEnabled = false
        textView.isAutomaticSpellingCorrectionEnabled = false
        textView.font = NSFont.monospacedSystemFont(ofSize: 13, weight: .regular)
        textView.textColor = NSColor(white: 0.85, alpha: 1.0)
        textView.drawsBackground = false
        textView.insertionPointColor = NSColor.white
        textView.textContainerInset = NSSize(width: 4, height: 4)

        textView.string = text
        textView.delegate = context.coordinator

        // The view has no size until SwiftUI lays it out, so scroll on the next turn.
        DispatchQueue.main.async {
            textView.setSelectedRange(NSRange(location: (textView.string as NSString).length, length: 0))
            textView.scrollToEndOfDocument(nil)
        }
        return scrollView
    }

    func updateNSView(_ nsView: NSScrollView, context: Context) {
        guard let textView = nsView.documentView as? NSTextView else { return }
        // Only an outside change (the Clean text button) is pushed in; typing is
        // already in the view, and resetting it would jump the cursor.
        guard textView.string != text else { return }
        // Replaced as an undoable edit, not by assigning `string`: that skips the
        // undo manager, leaving earlier typing's ranges to replay against the
        // shorter cleaned text on Cmd-Z.
        let full = NSRange(location: 0, length: (textView.string as NSString).length)
        guard textView.shouldChangeText(in: full, replacementString: text) else { return }
        context.coordinator.isUpdating = true
        textView.textStorage?.replaceCharacters(in: full, with: text)
        textView.didChangeText()
        context.coordinator.isUpdating = false
    }

    final class Coordinator: NSObject, NSTextViewDelegate {
        var text: Binding<String>
        /// Set while updateNSView pushes text in, so the echo is not written
        /// back to the binding in the middle of a SwiftUI view update.
        var isUpdating = false

        init(text: Binding<String>) {
            self.text = text
        }

        func textDidChange(_ notification: Notification) {
            guard !isUpdating, let textView = notification.object as? NSTextView else { return }
            text.wrappedValue = textView.string
        }
    }
}

import AppKit

extension NSView {
    /// Take this view out of the responder chain and the view hierarchy, before the
    /// last reference to it is dropped.
    ///
    /// A window does not retain its first responder. Releasing a view that still
    /// holds that role leaves the window pointing at freed memory, and the next
    /// action dispatched to the first responder reads it — `sendAction(_:to:from:)`
    /// with a nil target starts its walk at `window.firstResponder`. That is a
    /// use-after-free, and it presents as a segfault somewhere inside AppKit rather
    /// than anywhere near the code that dropped the reference.
    ///
    /// Closing a pane is exactly that situation: the terminal being closed is
    /// usually the one being typed in, so it is usually the first responder.
    ///
    /// AppKit resets the first responder itself when a view is removed from its
    /// superview, so this is belt-and-braces for the ordinary case — but it is the
    /// only thing covering a view whose superview is already gone, which is the
    /// state a terminal is in whenever its tab is not the visible one.
    ///
    /// Main thread only, like every other AppKit call in its callers. Not
    /// `@MainActor`, because `TerminalStore` and `WorkspaceAttachment` are not
    /// isolated and already touch views this way; annotating just this method would
    /// force that refactor rather than describe today's contract.
    func detachFromResponderChain() {
        if let window, window.firstResponder === self {
            // Aim at the window rather than nil: makeFirstResponder(nil) leaves the
            // window with no responder at all, which is a second way to lose keys.
            window.makeFirstResponder(window)
        }
        removeFromSuperview()
    }
}

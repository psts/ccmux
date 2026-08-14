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
    /// Scope: this covers a terminal that is still on screen and focused when its
    /// owner drops it. A view outside the window's hierarchy cannot hold the role
    /// at all — removing it releases it, and AppKit will not hand it back — so a
    /// terminal in a background tab is not a stale-responder risk and needs nothing
    /// here (`ResponderDetachTests` pins both halves of that).
    ///
    /// AppKit does the same reset when a view is removed from its superview, so on
    /// the ordinary path this is belt-and-braces. It earns its place by not
    /// depending on the order the caller happens to tear things down in.
    ///
    /// Main thread only, like every other AppKit call in its callers. Not
    /// `@MainActor`, because `TerminalStore` and `WorkspaceAttachment` are not
    /// isolated and already touch views this way; annotating just this method would
    /// force that refactor rather than describe today's contract.
    func detachFromResponderChain() {
        if let window, window.firstResponder === self {
            // Passing the window states the intent. (`nil` reaches the same end
            // state — AppKit documents it as making the window its own first
            // responder — but says it by omission.)
            window.makeFirstResponder(window)
        }
        removeFromSuperview()
    }
}

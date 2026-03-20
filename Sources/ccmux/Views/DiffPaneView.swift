import SwiftUI
import WebKit

struct DiffPaneView: NSViewRepresentable {
    let config: DiffConfig

    func makeNSView(context: Context) -> WKWebView {
        let webView = WKWebView(frame: .zero)
        webView.setValue(false, forKey: "drawsBackground")
        loadDiff(into: webView)
        return webView
    }

    func updateNSView(_ nsView: WKWebView, context: Context) {}

    private func loadDiff(into webView: WKWebView) {
        Task { @MainActor in
            let diffOutput = await GitService.diff(repoPath: config.repoPath, target: config.diffTarget)
            let html = DiffPaneView.renderDiffHTML(diffOutput)
            webView.loadHTMLString(html, baseURL: nil)
        }
    }

    static func renderDiffHTML(_ diff: String) -> String {
        var lines: [String] = []
        for line in diff.split(separator: "\n", omittingEmptySubsequences: false) {
            let escaped = String(line)
                .replacingOccurrences(of: "&", with: "&amp;")
                .replacingOccurrences(of: "<", with: "&lt;")
                .replacingOccurrences(of: ">", with: "&gt;")

            if line.hasPrefix("+") && !line.hasPrefix("+++") {
                lines.append("<span class=\"added\">\(escaped)</span>")
            } else if line.hasPrefix("-") && !line.hasPrefix("---") {
                lines.append("<span class=\"removed\">\(escaped)</span>")
            } else if line.hasPrefix("@@") {
                lines.append("<span class=\"hunk\">\(escaped)</span>")
            } else if line.hasPrefix("diff ") || line.hasPrefix("index ") || line.hasPrefix("---") || line.hasPrefix("+++") {
                lines.append("<span class=\"header\">\(escaped)</span>")
            } else {
                lines.append(escaped)
            }
        }

        return """
        <!DOCTYPE html>
        <html>
        <head>
        <meta charset="utf-8">
        <style>
            body { background: #1c1d21; color: #d4d4d4; font-family: ui-monospace, "SF Mono", monospace; font-size: 13px; margin: 0; padding: 12px; line-height: 1.5; }
            pre { margin: 0; white-space: pre-wrap; word-wrap: break-word; }
            .added { background: rgba(0, 180, 0, 0.12); color: #4ec94e; display: block; }
            .removed { background: rgba(220, 0, 0, 0.12); color: #f14c4c; display: block; }
            .header { color: #569cd6; font-weight: bold; }
            .hunk { color: #c586c0; }
            .empty { color: #666; text-align: center; padding: 40px; }
        </style>
        </head>
        <body>
        <pre>\(lines.isEmpty ? "<div class=\"empty\">No changes</div>" : lines.joined(separator: "\n"))</pre>
        </body>
        </html>
        """
    }
}

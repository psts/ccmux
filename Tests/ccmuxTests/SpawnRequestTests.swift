import XCTest
@testable import ccmux

final class SpawnRequestTests: XCTestCase {

    private func url(_ s: String) -> URL {
        guard let u = URL(string: s) else {
            XCTFail("bad test URL: \(s)")
            return URL(string: "ccmux://invalid")!
        }
        return u
    }

    // MARK: - parse

    func testParsesAllFields() {
        let req = SpawnRequest.parse(from: url(
            "ccmux://spawn?repo=/Users/me/proj&prompt=help%20me&requester=abc123"
        ))
        XCTAssertEqual(req, SpawnRequest(repoPath: "/Users/me/proj", prompt: "help me", requester: "abc123"))
    }

    func testRequesterIsOptional() {
        let req = SpawnRequest.parse(from: url("ccmux://spawn?repo=/x&prompt=hi"))
        XCTAssertEqual(req?.repoPath, "/x")
        XCTAssertEqual(req?.prompt, "hi")
        XCTAssertNil(req?.requester)
    }

    func testExpandsTilde() {
        let req = SpawnRequest.parse(from: url("ccmux://spawn?repo=~/proj&prompt=hi"))
        XCTAssertEqual(req?.repoPath, (("~/proj") as NSString).expandingTildeInPath)
        XCTAssertTrue(req?.repoPath.hasPrefix("/") == true)
    }

    func testDecodesPercentEncodedPrompt() {
        // "fix the bug & ship it" with special chars
        let req = SpawnRequest.parse(from: url(
            "ccmux://spawn?repo=/x&prompt=fix%20%22the%22%20bug%20%26%20%24PATH"
        ))
        XCTAssertEqual(req?.prompt, "fix \"the\" bug & $PATH")
    }

    func testDecodesPlusAsSpace() {
        // JS URLSearchParams encodes spaces as '+'. (Regression: URLComponents
        // leaves '+' literal, which produced "help+me" prompts.)
        let req = SpawnRequest.parse(from: url("ccmux://spawn?repo=/x&prompt=help+me+now"))
        XCTAssertEqual(req?.prompt, "help me now")
    }

    func testPreservesLiteralPlusFromPercent2B() {
        // A genuine '+' is sent as %2B and must survive (not become a space).
        let req = SpawnRequest.parse(from: url("ccmux://spawn?repo=/x&prompt=c%2B%2B+rocks"))
        XCTAssertEqual(req?.prompt, "c++ rocks")
    }

    func testWrongHostReturnsNil() {
        XCTAssertNil(SpawnRequest.parse(from: url("ccmux://other?repo=/x&prompt=hi")))
    }

    func testWrongSchemeReturnsNil() {
        XCTAssertNil(SpawnRequest.parse(from: url("https://spawn?repo=/x&prompt=hi")))
    }

    func testMissingRepoReturnsNil() {
        XCTAssertNil(SpawnRequest.parse(from: url("ccmux://spawn?prompt=hi")))
    }

    func testMissingPromptReturnsNil() {
        XCTAssertNil(SpawnRequest.parse(from: url("ccmux://spawn?repo=/x")))
    }

    func testEmptyValuesReturnNil() {
        XCTAssertNil(SpawnRequest.parse(from: url("ccmux://spawn?repo=&prompt=hi")))
        XCTAssertNil(SpawnRequest.parse(from: url("ccmux://spawn?repo=/x&prompt=")))
    }

    // MARK: - shell quoting

    func testShellQuoteWrapsPlainText() {
        XCTAssertEqual(SpawnRequest.shellSingleQuote("hello world"), "'hello world'")
    }

    func testShellQuoteEscapesSingleQuotes() {
        XCTAssertEqual(SpawnRequest.shellSingleQuote("it's a test"), "'it'\\''s a test'")
    }

    func testShellQuoteNeutralizesShellMetacharacters() {
        // $ ` ; & should all be literal inside single quotes
        XCTAssertEqual(SpawnRequest.shellSingleQuote("$(rm -rf /)`x`;&"), "'$(rm -rf /)`x`;&'")
    }

    func testClaudeStartupCommandIncludesChannelFlagAndQuotesPrompt() {
        let req = SpawnRequest(repoPath: "/x", prompt: "help peer 'abc'", requester: nil)
        XCTAssertEqual(
            req.claudeStartupCommand(),
            "env -u CLAUDE_PEERS_NAME -u CLAUDE_PEERS_PROJECT "
                + "claude --dangerously-load-development-channels server:claude-peers -- "
                + "'help peer '\\''abc'\\'''"
        )
    }

    func testClaudeStartupCommandStructure() {
        let cmd = SpawnRequest(repoPath: "/x", prompt: "hi", requester: nil).claudeStartupCommand()
        XCTAssertTrue(cmd.contains("--dangerously-load-development-channels server:claude-peers"))
        XCTAssertTrue(cmd.contains(" -- 'hi'"))  // prompt is positional, after the option terminator
        XCTAssertTrue(cmd.hasPrefix("env -u CLAUDE_PEERS_NAME -u CLAUDE_PEERS_PROJECT "))
    }
}

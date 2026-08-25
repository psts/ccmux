import XCTest
@testable import ccmux

/// Pins the settings capability probe: the Mac only SENDS llm/harness fields
/// when the daemon's GET offered them, because an older daemon silently DROPS
/// unknown fields (decodeJSON ignores unknown keys — never a 400), so sending
/// them would fake a successful save — and the probe is key PRESENCE, which
/// nothing else tests.
final class DaemonSettingsCapabilityTests: XCTestCase {

    func testModernDaemonOffersLLMAndHarnesses() throws {
        let json = """
        {"llmRoute":"ollama",
         "llmAccounts":[{"name":"ollama","kind":"anthropic","baseURL":"http://localhost:11434",
                         "apiKeySet":false,"modelAliases":[{"from":"claude-haiku-*","to":"qwen3-4b-32k"}]}],
         "harnesses":[{"name":"claude","icon":"✳","command":"claude","autoconfirm":true,"source":"builtin"},
                      {"name":"pi","command":"pi","source":"detected"}],
         "harnessRules":[{"pathPrefix":"/w/CL","harness":"pi"}]}
        """
        let s = try JSONDecoder().decode(DaemonSettings.self, from: Data(json.utf8))
        XCTAssertTrue(s.supportsLLM)
        XCTAssertTrue(s.supportsHarnesses)
        XCTAssertTrue(s.supportsHarnessRules)
        XCTAssertEqual(s.harnessRules.first?.harness, "pi")
        XCTAssertEqual(s.llmRoute, "ollama")
        XCTAssertEqual(s.llmAccounts.count, 1)
        XCTAssertEqual(s.llmAccounts[0].modelAliases.first?.to, "qwen3-4b-32k")
        XCTAssertEqual(s.harnesses.map(\.name), ["claude", "pi"])
        XCTAssertTrue(s.harnesses[0].autoconfirm)
        XCTAssertEqual(s.harnesses[1].source, "detected")
        XCTAssertFalse(s.harnesses[1].autoconfirm)
    }

    func testOlderDaemonOffersNeither() throws {
        // Pre-proxy daemons serve settings without the llm/harness keys at all;
        // the editors must then send none of them. (The retired
        // startupCommand/startupRules keys such daemons still emit are ignored.)
        let json = #"{"startupCommand":"claude","startupRules":[],"devDomain":""}"#
        let s = try JSONDecoder().decode(DaemonSettings.self, from: Data(json.utf8))
        XCTAssertFalse(s.supportsLLM)
        XCTAssertFalse(s.supportsHarnesses)
        XCTAssertFalse(s.supportsHarnessRules)
        XCTAssertEqual(s.llmRoute, "")
        XCTAssertTrue(s.llmAccounts.isEmpty)
        XCTAssertTrue(s.harnesses.isEmpty)
    }

    func testRoutePresenceAloneIsEnough() throws {
        // A daemon with the proxy but nothing configured still offers the keys.
        let json = #"{"llmRoute":"","llmAccounts":[],"harnesses":[]}"#
        let s = try JSONDecoder().decode(DaemonSettings.self, from: Data(json.utf8))
        XCTAssertTrue(s.supportsLLM)
        XCTAssertTrue(s.supportsHarnesses)
    }

    func testHarnessesWithoutRulesKeyDivergesTheProbes() throws {
        // The mixed case that matters: a v0.1.38 daemon serves `harnesses` but
        // predates `harnessRules`. The Mac must save harnesses while omitting
        // rules — sending them would be silently dropped and fake a save.
        let json = #"{"llmRoute":"","llmAccounts":[],"harnesses":[{"name":"claude","command":"claude"}]}"#
        let s = try JSONDecoder().decode(DaemonSettings.self, from: Data(json.utf8))
        XCTAssertTrue(s.supportsHarnesses)
        XCTAssertFalse(s.supportsHarnessRules)
        XCTAssertTrue(s.harnessRules.isEmpty)
    }
}

// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "ccmux",
    platforms: [.macOS(.v14)],
    dependencies: [
        .package(url: "https://github.com/migueldeicaza/SwiftTerm.git", from: "1.12.0"),
    ],
    targets: [
        .executableTarget(
            name: "ccmux",
            dependencies: ["SwiftTerm"],
            path: "Sources/ccmux",
            resources: [
                .copy("Resources")
            ]
        ),
        .testTarget(
            name: "ccmuxTests",
            dependencies: ["ccmux"],
            path: "Tests/ccmuxTests"
        ),
    ]
)

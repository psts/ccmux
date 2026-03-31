// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "ccmux",
    platforms: [.macOS(.v14)],
    dependencies: [
        .package(path: "SwiftTerm"),
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

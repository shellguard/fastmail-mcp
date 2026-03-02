// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "fastmail-mcp",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "fastmail-mcp",
            path: "Sources/fastmail-mcp"
        )
    ]
)

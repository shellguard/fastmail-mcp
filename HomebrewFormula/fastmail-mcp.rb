class FastmailMcp < Formula
  desc "MCP server for Fastmail — email, contacts, masked email, and more via JMAP"
  homepage "https://github.com/shellguard/fastmail-mcp"
  version "1.3.0"
  license "MIT"

  on_macos do
    on_intel do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.3.0/fastmail-mcp-darwin-amd64.tar.gz"
      sha256 "56501ec1a071c97a6f5b5e06c1e6decbc0fe0b9b2cd3ef11964506ddebd44156"
    end
    on_arm do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.3.0/fastmail-mcp-darwin-arm64.tar.gz"
      sha256 "e0ca38275276c4cd45b410853b4e365eba620ae33e64896eb099bffd30c1e8e8"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.3.0/fastmail-mcp-linux-amd64.tar.gz"
      sha256 "fca7e2cc892e547b11806c55ce4221dec01b43833272a786f0b5a0684182e007"
    end
    on_arm do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.3.0/fastmail-mcp-linux-arm64.tar.gz"
      sha256 "3f137f2c6fa4b3621825449fac0719679875241faf4894b9f43dbe2dc03a89ad"
    end
  end

  def install
    bin.install "fastmail-mcp"
  end

  def caveats
    <<~EOS
      Create a Fastmail API token at:
        Fastmail > Settings > Privacy & Security > API tokens
        Scopes: Email, Email submission, Contacts, Masked Email

      Register with Claude Desktop — add to config:
        {
          "mcpServers": {
            "fastmail": {
              "command": "#{bin}/fastmail-mcp",
              "env": { "FASTMAIL_TOKEN": "your-token-here" }
            }
          }
        }

      Config location:
        macOS:   ~/Library/Application Support/Claude/claude_desktop_config.json
        Linux:   ~/.config/Claude/claude_desktop_config.json
    EOS
  end

  test do
    input = '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
    output = pipe_output(bin/"fastmail-mcp", input, 0)
    assert_match "fastmail-mcp", output
  end
end

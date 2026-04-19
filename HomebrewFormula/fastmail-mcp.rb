class FastmailMcp < Formula
  desc "MCP server for Fastmail — email, contacts, masked email, and more via JMAP"
  homepage "https://github.com/shellguard/fastmail-mcp"
  version "1.2.0"
  license "MIT"

  on_macos do
    on_intel do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.2.0/fastmail-mcp-darwin-amd64.tar.gz"
      sha256 "1b22f4a03345307e7baabeb1ebd8ad146a84e342e1c02284b839cb88301a3b20"
    end
    on_arm do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.2.0/fastmail-mcp-darwin-arm64.tar.gz"
      sha256 "9db868f895c1410ca80883bb1528b9d674c84a58c7c64b4fb18dc24c3bd724a8"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.2.0/fastmail-mcp-linux-amd64.tar.gz"
      sha256 "a1392b89f65cb497c785e9c3b678a9cc3d1a440b052001ea6fe00f89951f0b47"
    end
    on_arm do
      url "https://github.com/shellguard/fastmail-mcp/releases/download/v1.2.0/fastmail-mcp-linux-arm64.tar.gz"
      sha256 "9e5ce7c65d631ea02c24230d4bf058913c284908d395e7bb986910e1ffad1ccf"
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

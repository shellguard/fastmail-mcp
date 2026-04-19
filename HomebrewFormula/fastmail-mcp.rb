class FastmailMcp < Formula
  desc "MCP server for Fastmail — 72 tools for email, calendar, contacts, Sieve, and more"
  homepage "https://github.com/shellguard/fastmail-mcp"
  license "MIT"

  on_macos do
    on_intel do
      url "https://github.com/shellguard/fastmail-mcp/releases/latest/download/fastmail-mcp-darwin-amd64.tar.gz"
    end
    on_arm do
      url "https://github.com/shellguard/fastmail-mcp/releases/latest/download/fastmail-mcp-darwin-arm64.tar.gz"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/shellguard/fastmail-mcp/releases/latest/download/fastmail-mcp-linux-amd64.tar.gz"
    end
    on_arm do
      url "https://github.com/shellguard/fastmail-mcp/releases/latest/download/fastmail-mcp-linux-arm64.tar.gz"
    end
  end

  def install
    bin.install "fastmail-mcp"
  end

  def caveats
    <<~EOS
      Set your Fastmail API token:
        export FASTMAIL_TOKEN="your-token-here"

      Generate a token at:
        Fastmail → Settings → Privacy & Security → API tokens
        Required scopes: Mail, Contacts, Calendars, Submission

      Register with Claude Desktop:
        Add to ~/Library/Application Support/Claude/claude_desktop_config.json:
        {
          "mcpServers": {
            "fastmail": {
              "command": "#{bin}/fastmail-mcp",
              "env": { "FASTMAIL_TOKEN": "your-token-here" }
            }
          }
        }
    EOS
  end

  test do
    assert_match "fastmail-mcp", shell_output("#{bin}/fastmail-mcp --version 2>&1", 1)
  end
end

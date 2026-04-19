class FastmailMcp < Formula
  desc "MCP server for Fastmail — email, calendar, contacts, Sieve, and more via JMAP"
  homepage "https://github.com/shellguard/fastmail-mcp"
  url "https://github.com/shellguard/fastmail-mcp/archive/refs/tags/v1.0.0.tar.gz"
  sha256 "782714e8f06eacefcdb728e603c1a3bd11458aceff48cfc995ca6242d73da8b7"
  license "MIT"

  depends_on "go" => :build

  def install
    ldflags = "-s -w -X main.version=#{version}"
    system "go", "build", *std_go_args(ldflags: ldflags)
  end

  def caveats
    <<~EOS
      Create a Fastmail API token at:
        Fastmail > Settings > Privacy & Security > API tokens
        Required scopes: Mail, Contacts, Calendars, Submission

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

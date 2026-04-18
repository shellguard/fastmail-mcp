#!/bin/bash
set -euo pipefail

BINARY_NAME="fastmail-mcp"

# ── Detect OS ────────────────────────────────────────────────────────────────
OS="$(uname -s)"
case "$OS" in
    Darwin)
        INSTALL_DIR="/usr/local/bin"
        CLAUDE_CONFIG="$HOME/Library/Application Support/Claude/claude_desktop_config.json"
        ;;
    MINGW*|MSYS*|CYGWIN*)
        INSTALL_DIR="$LOCALAPPDATA/Programs/fastmail-mcp"
        CLAUDE_CONFIG="$APPDATA/Claude/claude_desktop_config.json"
        BINARY_NAME="fastmail-mcp.exe"
        mkdir -p "$INSTALL_DIR"
        ;;
    Linux)
        INSTALL_DIR="$HOME/.local/bin"
        CLAUDE_CONFIG="${XDG_CONFIG_HOME:-$HOME/.config}/Claude/claude_desktop_config.json"
        mkdir -p "$INSTALL_DIR"
        ;;
    *)
        echo "Unsupported OS: $OS" >&2
        exit 1
        ;;
esac

# ── 1. Build ─────────────────────────────────────────────────────────────────
echo "Building $BINARY_NAME..."
go build -o "$BINARY_NAME" .

# ── 2. Install binary ────────────────────────────────────────────────────────
echo "Installing to $INSTALL_DIR/$BINARY_NAME..."
cp "$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"
chmod +x "$INSTALL_DIR/$BINARY_NAME"

# ── 3. Register in Claude Desktop config ─────────────────────────────────────
echo "Updating Claude Desktop config..."
python3 - "$CLAUDE_CONFIG" "$INSTALL_DIR/$BINARY_NAME" <<'PYEOF'
import sys, json, os

config_path, binary_path = sys.argv[1], sys.argv[2]

config = {}
if os.path.exists(config_path):
    with open(config_path) as f:
        config = json.load(f)

config.setdefault("mcpServers", {})["fastmail"] = {
    "command": binary_path,
    "args": [],
    "env": {
        "FASTMAIL_TOKEN": os.environ.get("FASTMAIL_TOKEN", "YOUR_TOKEN_HERE")
    }
}

os.makedirs(os.path.dirname(config_path), exist_ok=True)
with open(config_path, "w") as f:
    json.dump(config, f, indent=2)
    f.write("\n")
PYEOF

echo ""
echo "Done. Set FASTMAIL_TOKEN in the Claude Desktop config, then restart Claude Desktop."

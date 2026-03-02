#!/bin/bash
set -euo pipefail

BINARY_NAME="fastmail-mcp"
INSTALL_DIR="/usr/local/bin"
CLAUDE_CONFIG="$HOME/Library/Application Support/Claude/claude_desktop_config.json"

# ── 1. Build ──────────────────────────────────────────────────────────────────
echo "Building $BINARY_NAME..."
swift build -c release
BINARY=".build/release/$BINARY_NAME"

# ── 2. Install binary ─────────────────────────────────────────────────────────
echo "Installing to $INSTALL_DIR/$BINARY_NAME..."
cp "$BINARY" "$INSTALL_DIR/$BINARY_NAME"
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

with open(config_path, "w") as f:
    json.dump(config, f, indent=2)
    f.write("\n")
PYEOF

echo ""
echo "Done. Set FASTMAIL_TOKEN in the Claude Desktop config, then restart Claude Desktop."

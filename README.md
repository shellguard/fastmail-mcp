# fastmail-mcp

An MCP server that gives AI assistants full access to your Fastmail account — email, calendar, contacts, Sieve filters, masked email aliases, and more. 72 tools covering the complete Fastmail JMAP API.

Built for [Claude Desktop](https://claude.ai/download), [Claude Code](https://claude.com/claude-code), and any MCP-compatible client.

## Install

### Homebrew (macOS / Linux)

```bash
brew install shellguard/tap/fastmail-mcp
```

### Scoop (Windows)

```powershell
scoop bucket add shellguard https://github.com/shellguard/fastmail-mcp
scoop install fastmail-mcp
```

### From source

```bash
go install github.com/shellguard/fastmail-mcp@latest
```

### Manual

Download a pre-built binary from [Releases](https://github.com/shellguard/fastmail-mcp/releases) and add it to your PATH.

## Setup

### 1. Create a Fastmail API token

Go to **Fastmail > Settings > Privacy & Security > API tokens > New API token**.

Required scopes: **Mail, Contacts, Calendars, Submission**.

### 2. Register with Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "fastmail": {
      "command": "fastmail-mcp",
      "env": {
        "FASTMAIL_TOKEN": "your-token-here"
      }
    }
  }
}
```

Or run the install script which does this automatically:

```bash
FASTMAIL_TOKEN="your-token" ./install.sh
```

### 3. Restart Claude Desktop

The Fastmail tools will appear in Claude's tool list.

## What can it do?

### Email (16 tools)
Read, search, send, reply, forward, move, delete, archive, flag, snooze, and batch-process emails. Full thread view. Draft management. Search with highlighted snippets. Import raw .eml files.

### Calendar (10 tools)
List calendars, browse events by date range, create/update/delete events, RSVP to invitations. Create and manage calendars themselves. Full JSCalendar support including recurrence, participants, alerts, and locations.

### Contacts (8 tools)
List, search, create, update, and delete contacts. Manage address books. Accepts both simple fields (`firstName`, `emails` as strings) and full JSContact format.

### Sieve Filters (7 tools)
List, read, create, update, delete, activate, and validate server-side mail filtering rules. Query available Sieve extensions. Fastmail supports `vnd.cyrus.jmapquery` for using JMAP filter syntax inside Sieve rules.

### Spam Management (3 tools)
Report spam, report phishing, and correct false positives. Each action trains Fastmail's Bayesian spam filter via `$junk`/`$phishing`/`$notjunk` keywords.

### Masked Email (3 tools)
Create, list, and manage Fastmail's anonymous email aliases. Create per-site aliases for signups, disable them when done, investigate which alias is leaking.

### Newsletter Management (2 tools)
Detect newsletters via `List-Id`/`List-Unsubscribe` headers. RFC 8058 one-click unsubscribe for trusted senders.

### Mailbox Organization (8 tools)
Create, rename, and delete folders. Bridge inbox for structured message processing. Duplicate detection across mailboxes. Mailbox statistics with sender/domain aggregation.

### Productivity (3 tools)
Find unreplied sent emails. Analyze any sender's history and trust signals. Vacation auto-responder management.

### Other (8 tools)
Storage quota. Sending identity management. Email delivery tracking. Attachment download URLs. Read receipt (MDN) send/parse.

## Example workflows

### Inbox cleanup
```
You: "Clean up my inbox"
Claude: [runs fm_get_mailbox_stats] → "You have 3,200 unread. Top sender: newsletter@company.com (847 emails). Want me to handle these?"
You: "Archive the newsletters, report the spam"
Claude: [runs fm_archive_email for newsletters, fm_report_spam for spam, then fm_set_sieve_script to auto-file future newsletters]
```

### Spam management
```
You: "I'm getting a lot of spam"
Claude: [runs fm_get_mailbox_stats + fm_detect_newsletters to identify patterns]
       [runs fm_report_spam to train filter]
       [writes and activates Sieve rules to auto-junk future spam]
```

### Duplicate removal
```
You: "My mailbox was imported twice"
Claude: [runs fm_find_duplicates] → "Found 142 duplicate groups (1,847 extra copies)"
You: "Remove them"
Claude: [runs fm_delete_email with the suggested delete IDs]
```

### Calendar management
```
You: "Schedule a meeting with the team next Tuesday at 2pm"
Claude: [runs fm_list_calendars, fm_create_event with title, time, duration, participants]
```

## Architecture

- **Language:** Go (stdlib only, zero dependencies)
- **Protocol:** [MCP](https://modelcontextprotocol.io/) over JSON-RPC stdio
- **API:** [JMAP](https://jmap.io/) (RFC 8620/8621) + Fastmail extensions
- **Platforms:** macOS, Windows, Linux (amd64 + arm64)
- **Binary size:** ~2.5 MB

### JMAP coverage

72 tools covering 35 JMAP methods across 15 object types:

| Object | Methods |
|--------|---------|
| Email | get, query, set, import, parse |
| Mailbox | get, query, set |
| Thread | get |
| SearchSnippet | get |
| Identity | get, set |
| EmailSubmission | get, query, set |
| VacationResponse | get, set |
| ContactCard | get, query, set |
| AddressBook | get, set |
| Calendar | get, set |
| CalendarEvent | get, query, set |
| MaskedEmail | get, set |
| SieveScript | get, set, validate |
| Quota | get |
| MDN | send, parse |

Only delta-sync (`*/changes`), push notifications, and cross-account copy are omitted by design (not useful for stateless MCP).

### Security

- API token never logged or included in error messages
- JMAP session URL validated as HTTPS
- All HTTP responses capped at 50MB via `io.LimitReader`
- Email body values capped at 1MB per part
- Search filter keys allowlisted to prevent JMAP filter injection
- Keyword values validated against RFC 8621 character class
- Batch ID arrays capped (500 general, 100 for permanent delete)
- Download URLs use `url.PathEscape`/`url.QueryEscape`
- MDN send pre-checks for `Disposition-Notification-To` header
- Sieve script deletion checks active status before deactivating

## Development

```bash
make build        # Build for current platform
make vet          # Run go vet
make test         # Run tests
make release      # Cross-compile all 6 platforms
make checksums    # SHA-256 checksums for release archives
```

### Adding a new tool

1. Add function in the appropriate `tools_*.go` file
2. Add `toolDefinition` to `definitions.go`
3. Add handler entry to `toolHandlers` in `definitions.go`
4. `make build`

### File structure

```
main.go              Entry point, MCP server, JSON-RPC handling
jmap.go              Session discovery, HTTP helpers, blob upload/download
helpers.go           JSON helpers, serialization, utilities
definitions.go       72 tool definitions + handler dispatch map
tools_email.go       Email CRUD, search, thread, batch, draft, forward
tools_mailbox.go     Mailbox CRUD, bridge inbox
tools_calendar.go    Calendar + event CRUD, RSVP
tools_contacts.go    Contacts CRUD, address books
tools_sieve.go       Sieve script management
tools_workflow.go    Stats, dedup, newsletter, spam, archive, snooze, flag
tools_misc.go        Identity, masked email, vacation, quota, MDN
```

## License

MIT

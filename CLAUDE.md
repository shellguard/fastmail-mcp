# Fastmail MCP — Claude Code Guide

## Project Overview

A single-file Go MCP server (`main.go`) that exposes Fastmail email, contacts, and identities as MCP tools via the JMAP API. No external dependencies — stdlib only.

Cross-platform: builds for macOS, Windows, and Linux from a single codebase.

## Build & Install

```bash
go build -o fastmail-mcp .                          # Build for current platform
GOOS=windows GOARCH=amd64 go build -o fastmail-mcp.exe .  # Cross-compile for Windows
./install.sh                                         # Build + install + register with Claude Desktop
```

## Configuration

Set `FASTMAIL_TOKEN` environment variable with a Fastmail API token.
Generate one at: Fastmail → Settings → Privacy & Security → API tokens → New API token.
Required scopes: Mail, Contacts, Submission.

## Architecture

Single-file Go (`main.go`), no external dependencies. Uses `net/http` for synchronous JMAP HTTP calls.

**Logical sections (in order):**
1. MCP protocol types (`toolDefinition`, `mcpError`)
2. JMAP session discovery + HTTP helpers (`sessionFor`, `jmapCall`, `doHTTPWithRetry`)
3. JSON helpers (`getString`, `getMap`, `respData`, `respList`, etc.)
4. Tool implementations (one Go function per tool)
5. Serialization helpers (`emailSummaryDict`, `contactSummaryDict`, etc.)
6. Utility functions (`intParam`, `contains`, `parseBridgeSubject`)
7. Tool definitions (`tools` slice)
8. Tool dispatch (`callTool` via `toolHandlers` map)
9. MCP server (`run`, `handleMessage` — JSON-RPC stdio loop)
10. Entry point (`main`)

## JMAP API Pattern

All JMAP calls follow this pattern:
1. Session discovery: `GET https://api.fastmail.com/jmap/session` (cached)
2. Method calls: `POST` to `session.apiUrl` with `{"using": [...], "methodCalls": [...]}`
3. For listing: two-step `Foo/query` → `Foo/get` using back-references (`#ids`)
4. Rate limiting: automatic retry on 429 with `Retry-After` header

## Tools (14 total)

### Email (8)
- `fm_list_mailboxes` — all mailboxes with role, unread/total counts
- `fm_list_emails` — emails in mailbox; params: mailboxId, limit, offset, onlyUnread
- `fm_get_email` — full email by ID with body, HTML, attachments
- `fm_search_emails` — search with text or JMAP filter JSON; params: query, mailboxId, limit
- `fm_send_email` — send email; params: to, subject, body, cc, replyToId
- `fm_mark_read` — mark read/unread; params: ids, read
- `fm_move_email` — move to mailbox; params: ids, mailboxId
- `fm_delete_email` — move to Trash; params: ids

### Bridge Inbox (2)
- `fm_list_bridge_messages` — unread emails in Bridge mailbox with parsed structured types
- `fm_ack_bridge_message` — mark read + move to Bridge/Processed

### Contacts (2)
- `fm_list_contacts` — list/search contacts; params: limit, search
- `fm_get_contact` — contact by ID with full details

### Identity (1)
- `fm_list_identities` — sending identities (email addresses)

## Bridge Message Convention

The bridge inbox is a designated Fastmail mailbox (default name: "Bridge") for structured messages that Claude can process as actionable items.

### Subject format
```
[TYPE] description
```

Supported types:
- `[TASK]` — actionable task, e.g. `[TASK] Buy groceries`
- `[EVENT]` — calendar event, e.g. `[EVENT] Doctor appointment 2026-03-15 14:00`
- `[NOTE]` — note/reminder, e.g. `[NOTE] Remember to call Mom`

### Setup
1. Create a mailbox called "Bridge" in Fastmail
2. Create a subfolder "Processed" under Bridge (Bridge/Processed)
3. Send structured emails to yourself with `[TYPE] description` subjects
4. Use `fm_list_bridge_messages` to read and `fm_ack_bridge_message` to process

### How it works
- `fm_list_bridge_messages` reads unread emails from the Bridge mailbox
- If the subject matches `[TYPE] description`, returns `bridgeType` and `bridgeDescription` fields
- Body is free text — returned as-is for the caller to interpret
- `fm_ack_bridge_message` marks the message read and moves to Bridge/Processed

## Adding a New Tool

1. Add a Go function with signature `func myTool(params m) (any, error)` in the tool implementations section
2. Add a `toolDefinition` to the `tools` slice
3. Add an entry to the `toolHandlers` map
4. `go build .`

## Key Design Notes

- Session is cached for process lifetime (no re-discovery per call), protected by `sync.Mutex`
- 429 rate limit: retries up to 2 times with `Retry-After` delay (capped at 30s)
- `fm_send_email` accepts both `[{name, email}]` objects and plain `["email"]` string arrays for to/cc
- `fm_send_email` moves sent mail to the Sent folder (falls back to destroying draft if no Sent folder)
- `fm_search_emails` query can be plain text (becomes `{"text": query}`) or a JSON JMAP filter string
- Contacts use `https://www.fastmail.com/dev/contacts` capability and `ContactCard/query+get`
- Sending uses `urn:ietf:params:jmap:submission` capability with `Email/set` + `EmailSubmission/set`
- Limit params are capped at 200 to prevent oversized JMAP responses
- JSON-RPC notifications (no `id`) never receive responses
- Proper JSON-RPC error codes: -32600 (invalid request), -32601 (method not found), -32602 (invalid params)
- Max input line size: 10MB
